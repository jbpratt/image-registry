# Design: native-read / etcd-write hybrid (POC record)

This is an experiment record for the phase 2 POC, not a product spec. It
documents what workstream 1 actually implements, how to turn it on, what the
metrics mean, and what must be true before any of this ships. The scope,
hypotheses, and verified facts live in `NATIVE-READ-POC.md`; this document does
not repeat them.

## Problem and why now

The pull hot path resolves tag → digest, manifest GET, and blob stat through
the ImageStream/etcd path, so every pull adds apiserver and etcd load
proportional to pull QPS. The three constraints that originally forced etcd
into the read path (watch-based triggers, eventually-consistent S3, etcd being
the only consistent store already on the payload) have all expired — see
`NATIVE-READ-POC.md` "Background". Distribution's native storage layout is now
metadata-complete for locally-pushed manifests and layers and strongly
consistent, so the read path can be served from storage while push stays
byte-for-byte unchanged (synchronous ImageStreamMapping).

The POC isolates **metadata reads only**. Authorization still calls the
apiserver on every request and is held constant across every A/B measurement, so
it cannot confound the result. The single most valuable number this experiment
produces is the **fallback rate**: how often a native read misses and falls back
to the imagestream, which quantifies real-world migration debt.

## Options considered

- **(a) native-read / etcd-write hybrid — chosen for the POC.** Reads try the
  storage-backed service first and fall back to the imagestream on a miss;
  writes are unchanged, so etcd stays authoritative. Tradeoffs: smallest diff
  (no new services, no new stateful component), push semantics untouched, fully
  reversible by a flag. Cost: native reads can serve stale content after an
  imagestream-side mutation until pruning removes the link (the accepted skews
  below), and every fallback pays one extra storage read.

- **(b) full projection (registry storage authoritative, controller projects
  CRs).** Storage becomes the source of truth; a level-triggered controller
  reconciles ImageStream CRs from storage. Tradeoffs: removes etcd from the read
  path entirely and resolves the staleness problem at the source, but is a much
  larger change, introduces a new controller to operate, and inverts the
  two-masters relationship (ImageStreams still accept `oc tag`/imports, which
  must then be routed through the registry or dropped). Deferred; the POC's
  workstream 4 stub explores the controller mechanics without committing to it.

- **(c) status quo with caching.** Keep etcd authoritative for reads and lean
  harder on the repo-scoped descriptor cache. Tradeoffs: no correctness risk and
  no new machinery, but it does not remove apiserver/etcd load under cache
  misses or cold cache, which is the entire point — so it cannot satisfy the H1
  perf hypothesis. Rejected as the primary path; retained as the fallback if the
  POC's numbers do not justify the hybrid.

## Opt-in mechanism as implemented

Native reads are **off by default** and gated by a config block plus an anchored
per-repository regex, so one cluster can run both modes side by side in a single
benchmark run.

- Config type `NativeReadPath` (`configuration/configuration.go:126,177-180`):
  `enabled` (bool) and `reporegex` (string).
- `reporegex` is compiled as a **full-string** match against the repository name
  `namespace/name`, wrapped `^(?:pattern)$` (`nativeread.go:31-38`). An empty
  pattern matches every repository. The match happens once at repository
  construction (`app.go:124-130`, `repository.go` `nativeRead` field), so the
  flag is fixed for the life of the repository object.
- When `enabled` is false, or the regex does not match, every read uses the
  existing imagestream path and `tagService.Tag` remains a no-op — behavior is
  byte-for-byte unchanged except that the pre-existing blob-stat branches now
  also emit the resolution metric.

Enable native reads for repositories under the `perf-*` namespace:

```yaml
openshift:
  nativereadpath:
    enabled: true
    reporegex: perf-[^/]+/.*
```

Enable for every repository (measure the global fallback rate): set `enabled:
true` and omit `reporegex` (or set it to `.*`).

## Measurements

One metric is emitted:
`imageregistry_native_read_resolutions_total{resolution, operation}`
(`metrics/prometheus.go:156-164`).

- `resolution` ∈ `native` (served from storage, no apiserver call), `fallback`
  (native missed, served via imagestream), `native_error` (native failed for a
  reason other than a miss — storage outage/permission/I/O — logged at warn and
  served via imagestream).
- `operation` ∈ `tag_get`, `manifest_get`, `blob_stat`. Tag-link **write**
  failures on push are also recorded as `native_error`/`tag_get` (see Findings):
  a rising `native_error` on `tag_get` therefore reflects storage degradation on
  either tag reads or tag-link writes, not migration debt.

Route-level p50/p99 histograms are reused for latency; no new latency metric is
added.

Mapping to the go/no-go criteria and current state:

| Criterion (from plan) | Signal | State |
|---|---|---|
| 1. ≥90% apiserver QPS cut, p99 improvement | `apiserver_request_total` (image verbs) + route p99, A/B via reporegex | **measured (WS3): >99% cut on tag/manifest, p99 improved** |
| 2. Zero content diffs; crossmount explained | workstream 2 harness (digest/byte asserts) | **measured (WS2): zero diffs, crossmount explained** |
| 3. Pulls survive apiserver outage window | outage drill under pull load | **measured (WS3): 100% native pull success, API-independent (auth phase out of scope)** |
| 4. Projection converges after chaos; lag p99 bound | workstream 4 controller + chaos | fakes only; real push→CR lag needs cluster |
| 5. Fallback rate on realistic data + backfill design | `native_read_resolutions_total{resolution=fallback}` | **measured (WS3): 0% — but freshly-pushed data only, not realistic migration debt** |
| Error classification correctness (miss vs storage error) | unit tests | **measured** (`tagservice_test.go`, `manifestservice_test.go`, `nativeread_test.go` pass) |
| Push atomicity on native tag-link error | unit test | **measured** (`TestTagNativeReadTagErrorKeepsPushAtomic`) |

Everything under "needs cluster" requires SNO/CRC with an S3 (minio) or
filesystem backend; the integration framework's fake control plane would
understate the apiserver win.

## Kill criteria

Go/no-go bars, copied from `NATIVE-READ-POC.md`:

1. ≥90% reduction in image-API apiserver QPS under pure pull load, with a
   measurable p99 improvement (bar fixed after the baseline run).
2. Zero content diffs in workstream 2; crossmount behavior understood and
   explainable, even if broken.
3. Pulls succeed for the full apiserver outage window.
4. Projection converges after every chaos scenario; lag p99 within bound.
5. Fallback rate measured on realistic data, with a written (not built) backfill
   design for driving it down.

Concrete kill triggers — hitting any of these ends the hybrid line of work:

- **Fallback rate above the baseline-fixed bar** on realistic data (the extra
  storage read per fallback erases the apiserver win before backfill lands).
- **Any content diff** in workstream 2 between native and imagestream reads for
  a registry-pushed image (H3 is a hard invariant, not a tunable).
- **Crossmount behavior that cannot be explained** — cross-repo blob mounts are
  authorized via imagestream metadata and native layer links may disagree; an
  unexplained result blocks, an explained-but-broken one is a finding.
- **Unbounded tombstone skew** — if the stale-read window after an
  imagestream-side delete/retarget cannot be bounded by a tombstone/generation
  mechanism, native reads cannot be trusted for correctness-sensitive repos.

## Findings so far

Folding in the ir-8y7 read-only assessment of the POC branch:

- **Workstream 1 is implemented as planned.** Config knob + anchored full-string
  reporegex (`configuration.go:126,177-180`; `app.go:124-130`;
  `nativeread.go:31-38`; `repository.go` `nativeRead` field). `tagService.Get`
  native-first with imagestream fallback and tag dual-write on `Tag`
  (`tagservice.go`); `All`/`Lookup` stay imagestream-only by construction.
  `manifestService.Get` native-first with fallback (`manifestservice.go:74-77`);
  `Exists` unchanged. Blob `Stat` keeps its storage-first behavior and is only
  instrumented (`blobdescriptorservice.go`).
- **Error classification is correct.** Native not-found (`ErrTagUnknown`,
  `ErrManifestUnknownRevision`) counts as `fallback`; anything else warns and
  counts as `native_error`, so storage degradation is never mistaken for
  migration debt (`nativeread.go:40-55`). Verified by unit tests.
- **ACCEPTED SKEW — production blocker, not a POC bug (stale tag links).**
  Native `tagService.Get` serves the stored tag link after the tag is retargeted
  or deleted on the imagestream side, because `Untag` is a no-op
  (`tagservice.go:33-36`). `oc tag`/`oc tag -d` update etcd but not the native
  link, so native reads resolve the old digest until pruning. Needs
  tombstone/generation handling before production (plan fact 6).
- **ACCEPTED SKEW — production blocker, not a POC bug (stale manifest
  revisions).** Native `manifestService.Get` serves a manifest with a storage
  revision link even after every imagestream reference to it is removed
  (`manifestservice.go:74-77`); the imagestream path would 404 in that window.
  Same tombstone/generation requirement (plan fact 6).
- **FIXED in this stack — push atomicity.** Previously a native tag-link write
  error after the ImageStreamMapping had already been created returned the error
  and turned one push into a partial write. `tagService.Tag` now logs at warn,
  records `native_error`/`tag_get`, and returns nil; the ISM is authoritative
  and reads fall back (`tagservice.go`, `TestTagNativeReadTagErrorKeepsPushAtomic`).
- **FIXED in this stack — blob-stat undercount.** The successful global-blob,
  empty-digest, and repository-cache branches of `Stat` did not record a
  resolution, undercounting the link-miss rate. They now record
  `fallback`/`blob_stat`, consistent with the existing `HasBlob` branch
  (`blobdescriptorservice.go`).
- **Rebase overlap with the distribution v3.1.1 bump was mechanical.** The three
  cherry-picks conflicted only in `repository.go`, where the bump added
  `withAuthContext(ctx)` calls in the `Tags`/`Manifests` constructors; each was
  resolved by keeping that line and taking the POC's native wiring. No semantic
  conflict. `configuration.go` and `tagservice_test.go` auto-merged.

### Workstream 4 status (projection stub)

The level-triggered projection controller stub landed cluster-free in
`pkg/projection` plus `cmd/native-read-projector` (~480 lines non-test). What the
fakes (fake `TagLister` + versioned image clientset) **measured**: reconcile is
idempotent — a repeated identical push produces no second status write; a native
tag whose head already matches is left byte-for-byte, so no-op detection is exact.
Chaos with 30% of events dropped before enqueue converges every tag across 10
repos after one resync, because reconcile re-lists the tag store rather than
applying event payloads. Out-of-order delivery converges to the `TagLister` truth,
not event order. A restarted controller with an empty queue converges from the
tag store alone via the injectable resync list. Event-to-write lag p99 over the
chaos load was **~16ms** with fakes (bound asserted < 2s). The stub only
adds/updates status tags and never removes, honoring facts 3/6; spec is untouched.

What still **needs a cluster**: real push→CR lag under load (the H4 number that
matters), the native tag-store adapter behind `TagLister` (a documented TODO in
the binary), registry-side notification config to actually emit the webhooks, and
the two-masters interaction (`oc tag` writes racing the projection, fact 6).
Deliberately skipped per the bead: tag deletion/tombstones, Image CRs, an
operator/deployment manifest.

## Workstream 2 results (CRC, 2026-09-04)

Harness: `test/integration/nativeread/` (new package), run against CRC
(OpenShift 4.20.1, `KUBECONFIG=$HOME/.crc/machines/crc/kubeconfig`). The
existing suite was green on CRC before any change (`TestDeleteManifest` PASS,
3.16s). Each test builds two in-process registries over one shared filesystem
storage root — one with native reads on (`reporegex=.*`), one off — plus the
same cluster, so the *same* image is read both ways. Two registries are
required because the framework's default inmemory driver is per-instance, so
the native-off registry could not serve blobs pushed to the native-on one; the
native knob and the filesystem driver are both passed via
`testframework.RegistryOption`s implemented in the test package, so
`pkg/testframework` itself is unchanged.

Full run: `go test ./test/integration/nativeread/... -run . -count=1 -v` →
`ok ... 25.508s`, all PASS. Namespaces use the `nrpoc-` prefix and are deleted
by `master.Close()`.

Expected-failure handling: the crossmount and imagestream-side-skew cases are
**recorded with `t.Logf` and never fail the suite** (chosen over `t.Skip` and
env gating so the run output is the finding record). The content-equality,
no-ghost, and metric cases are hard assertions.

| Test | Mode | Expected | Observed | Finding |
|---|---|---|---|---|
| 1. push + read (schema2 & OCI), by tag & by digest | native vs imagestream | identical digest & bytes | identical (731 B schema2 / 693 B OCI; 3 blob refs stat+get equal) | H3 holds: zero content diffs |
| 1. tag overwrite then immediate GET | native | new digest | native returned the new digest immediately | dual-write on `Tag` works on overwrite |
| 2. cross-repo blob mount into native repo | native | expected first failure (finding) | `201 Created`; blob then servable from dst natively | mount is a blob-link/auth op, unaffected by the native read flag; no divergence observed |
| 3. delete manifest via registry API, GET by digest | native & imagestream | no ghost | both `manifest unknown` after delete | native serves no ghost once the storage revision link is removed |
| 4a. `oc tag -d` (istag delete), storage link intact | native vs imagestream | skew (native still serves) | by tag: native **served** old digest, imagestream `manifest unknown`; by digest: same split | **accepted skew confirmed** — native serves deleted content until pruning (plan fact 6) |
| 4b. `oc tag` move latest A→B | native vs imagestream | skew (native serves old) | native resolved **old** digest A, imagestream resolved **new** digest B | **accepted skew confirmed** — moved tag resolves stale natively |
| 5. resolution metric | native registry | native hits + fallback for etcd-only tag | `tag_get{native}=1`, `tag_get{fallback}=1`, `manifest_get{native}=3`, `blob_stat{native}=17` | metric classifies native vs fallback correctly; etcd-only tag (created via image API) falls back as designed |

Notes:

- **Go/no-go criterion 2 (zero content diffs) is met** for registry-pushed
  images: every native read was byte-identical to the imagestream read, by tag
  and by digest, for both schema2 and OCI. Crossmount is explained (a blob-link
  operation, orthogonal to native tag/manifest resolution) rather than broken.
- **The two accepted skews are reproduced on a real cluster**, quantifying the
  window the tombstone/generation design must drive to zero: after `oc tag -d`
  or `oc tag`, native reads keep serving the old digest by tag and by digest
  while the imagestream path already 404s or returns the new digest.
- `blob_stat` resolutions are all `native` here because every blob was pushed
  locally to its repo (repo layer-link hit); the `fallback` branch is only
  taken on a repo-link miss, which this harness does not exercise.
- The resolution counter is per-sink: only the metrics-enabled registry records,
  so the metric test's counts are self-contained rather than accumulated across
  the other tests.

## Workstream 3 results (CRC, 2026-09-04, scaled 100x5)

### Environment

- CRC, OpenShift 4.20.1 (single node), `KUBECONFIG=$HOME/.crc/machines/crc/kubeconfig`.
- Backend: minio single pod in `nrpoc-minio` (emptyDir), Service on 9000, bucket
  `nrpoc`, reached from the workstation via `oc port-forward` on local port 19000.
- Registry under test: the POC `dockerregistry` binary built from this branch,
  run **out of cluster** on the workstation (cheapest option that worked), with
  `KUBECONFIG` pointed at CRC and S3 pointed at minio through the port-forward.
  `storage.redirect.disable: true` so blob reads are proxied through the registry
  (out-of-cluster clients cannot follow minio presigned redirects, and this also
  makes the blob-HEAD load actually exercise the registry's `blob_stat` path).
- **A/B on one instance via the reporegex split** (`reporegex: nrpoc-native/.*`):
  repos under `nrpoc-native/*` read native-first, repos under `nrpoc-etcd/*` read
  imagestream-only. Auth is held constant — the same openshift auth controller
  (TokenReview + per-repo SAR on every request) runs in both arms, so only the
  metadata source differs.
- Load tool: `hey` (built into the scratchpad dir, not system-wide).

### Method

- Seed: 100 repos x 5 tags per mode of one tiny `FROM scratch` image (distinct
  tag names), pushed with `skopeo copy` (`hack/nrpoc/seed.sh`). Seeding time:
  **native 100x5 in 136s, etcd 100x5 in 211s**, 0 failures each. Scale is
  **100x5, scaled down from the plan's 1k x 10**.
- Load: `hey` for 60s at concurrency 1/10/50 against manifest-by-tag GET,
  manifest-by-digest GET, and blob HEAD, warm cache (a 3s warm-up precedes each
  run). Target `repo50` in each namespace.
- Image-API QPS: `apiserver_request_total{group="image.openshift.io"}` summed
  across verbs, scraped from the **openshift-apiserver** pod metrics
  (`image.openshift.io` is served there, not by the kube-apiserver), sampled
  immediately before and after each 60s run; QPS = delta / 60. A background
  relist floor of ~0.17 qps is visible in the idle (native) arm.
- Fallback rate: `imageregistry_native_read_resolutions_total{resolution=...}`
  from the registry's `/extensions/v2/metrics`, delta per run.

### Perf: manifest-by-tag GET

| Mode | Conc | rps | p50 | p99 | image-API QPS | fallback |
|---|---|---|---|---|---|---|
| native | 1 | 19.9 | 0.043s | 0.153s | 0.17 | 0 |
| native | 10 | 82.7 | 0.106s | 0.357s | 0.02 | 0 |
| native | 50 | 152.7 | 0.294s | 0.913s | 0.02 | 0 |
| etcd | 1 | 10.2 | 0.085s | 0.285s | 20.50 | 0 |
| etcd | 10 | 61.5 | 0.139s | 0.505s | 123.10 | 0 |
| etcd | 50 | 119.6 | 0.372s | 1.308s | 239.75 | 0 |

### Perf: manifest-by-digest GET

| Mode | Conc | rps | p50 | p99 | image-API QPS | fallback |
|---|---|---|---|---|---|---|
| native | 1 | 39.0 | 0.022s | 0.076s | 0.17 | 0 |
| native | 10 | 125.4 | 0.066s | 0.308s | 0.02 | 0 |
| native | 50 | 186.2 | 0.240s | 0.796s | 0.03 | 0 |
| etcd | 1 | 19.8 | 0.045s | 0.133s | 39.83 | 0 |
| etcd | 10 | 96.4 | 0.094s | 0.266s | 192.97 | 0 |
| etcd | 50 | 123.0 | 0.368s | 0.982s | 248.18 | 0 |

### Perf: blob HEAD

| Mode | Conc | rps | p50 | p99 | image-API QPS | fallback |
|---|---|---|---|---|---|---|
| native | 1 | 30.0 | 0.030s | 0.090s | 0.25 | 0 |
| native | 10 | 86.0 | 0.103s | 0.332s | 0.02 | 0 |
| native | 50 | 108.8 | 0.425s | 1.052s | 0.02 | 0 |
| etcd | 1 | 24.8 | 0.035s | 0.103s | 0.02 | 0 |
| etcd | 10 | 116.7 | 0.076s | 0.257s | 0.17 | 0 |
| etcd | 50 | 123.7 | 0.370s | 1.049s | 0.00 | 0 |

Blob HEAD costs ~0 image-API QPS in **both** arms — blob `Stat` is storage-first
regardless of the flag (plan fact 3a). Native read gives no blob-path apiserver
win; the entire H1 win is on tag and manifest resolution, as designed.

Cumulative registry resolution counter over the whole run (native arm): `tag_get`
16440, `manifest_get` 38448, `blob_stat` 172779, all `resolution="native"`;
**`fallback` and `native_error` never incremented (0)**.

### Outage drill (H2, metadata claim only)

**How the API was made unreachable:** the registry's `KUBECONFIG` was copied and
its `server:` rewritten to `https://127.0.0.1:1` (nothing listening), so every
apiserver call fails with `connect: connection refused`. Native read stayed on.

**Finding — the POC binary couples auth and metadata.** With the API down, the
POC `dockerregistry` returns 400 for `nrpoc-native/*` too: the openshift auth
controller performs a per-repo `SelfSubjectAccessReview` on every pull, and that
SAR needs the API. Switching auth to "none" is not an option either — the
openshift repository middleware hard-requires the openshift auth controller to
have run (`openshift.auth.completed missing from context`, 500). So on the POC
binary, auth — not metadata — is the outage blocker. This is precisely the H2
caveat the plan flags: the metadata claim is isolable, but production H2 also
needs the token-service/auth phase, which is out of scope.

**Isolating the metadata claim.** The identical native storage layout (the same
minio contents, synced to a local filesystem) was served through a **plain
`distribution` registry with no apiserver configured at all**, native read on by
construction (it *is* the native layout). Results with the API 100% absent:

| Test | Result |
|---|---|
| `hey` 60s conc=10, native manifest-by-tag | 166047 / 166047 = **100% 200** (2767 rps) |
| full pull (manifest+config+layer), 40 distinct native repo:tag | **40 / 40** |
| `nrpoc-etcd/*` manifest-by-tag (native layout never written) | 404 (cannot serve) |

The metadata claim holds: repos whose native layout is present serve the full
pull hot path with zero apiserver dependency. The only failures are repos that
have no native layout (the etcd-only arm, where `Tag` is a no-op) — the migration
-debt boundary, not a native-path failure.

### Mapping to go/no-go criteria

- **Criterion 1 (>=90% image-API QPS cut + measurable p99 improvement).** MET for
  tag and manifest resolution: image-API QPS drops from 20.5/123.1/239.8 (tag) and
  39.8/193.0/248.2 (digest) to <=0.17 in every native cell — a >99% reduction at
  all three concurrencies. Native p99 is also lower at matching concurrency
  (tag conc50 0.913s vs 1.308s; digest conc50 0.796s vs 0.982s). Blobs are
  excluded from the win by construction (fact 3a). The baseline bar can be fixed
  from the etcd column.
- **Criterion 3 (pulls survive the apiserver outage window).** MET for the
  metadata claim: 100% pull success with the API fully unreachable for native
  -layout repos. Auth phase explicitly out of scope, and the POC binary's
  auth/metadata coupling is recorded above as the reason the drill needed a plain
  distribution registry.
- **Criterion 5 (fallback rate on realistic data).** Measured **0%** — but on
  freshly-pushed data only, so this is NOT the realistic migration number: every
  image was pushed through the registry, so the native layout is complete and
  nothing falls back. A realistic fallback rate needs a cluster with etcd-only
  images (`oc tag`, `oc import-image`, pre-existing streams); this scaled seed
  has none. The written backfill design remains the criterion-5 deliverable.

### What this does not show

- **Single node.** CRC is one node: no HA apiserver, no etcd load spread, no
  cross-node network. The apiserver QPS numbers are relief on one apiserver, not
  a fleet.
- **minio is not real S3.** No cross-AZ latency, no S3 throttling or
  consistency-edge behavior; reached over a local `oc port-forward`, so blob/manifest
  latency includes a loopback hop, not pod-to-service networking.
- **Scaled down (100x5, not 1k x 10).** Working set is ~1 shared tiny blob; this
  does not exercise large manifests, many distinct blobs, or cache pressure.
- **Out-of-cluster registry.** The registry ran on the workstation over
  localhost, not as an in-cluster pod; absolute latencies are not production
  latencies (though the A/B delta is valid — both arms share the setup).
- **Auth dominates single-request latency.** Every request pays a TokenReview +
  SAR round-trip to CRC; that constant cost sets the rps ceiling and is why native
  p50 is tens of ms rather than sub-ms. The metadata win shows up in image-API QPS
  and in the p99/throughput delta, not in a collapse of p50.
- **Blob redirect disabled.** Blobs were proxied through the registry, so blob
  latency includes the registry hop rather than a direct S3 fetch.
- **Fallback rate is 0 only because no etcd-only images were seeded.** The real
  migration fallback rate is unmeasured here.
- **The outage drill bypassed the POC auth path.** It served the native layout
  through a plain distribution registry, because the POC binary's own openshift
  auth needs the API. It proves the storage layout is API-independent, not that
  the POC binary is.

### Appendix: scripts

Seeding is `hack/nrpoc/seed.sh` (committed): `seed.sh <ns> <token-file> [repos]
[tags] [parallel]`, `skopeo copy` of one `oci:` image layout into `repoNNN:tagN`
with a 5x retry (the shared-blob cross-mount HEAD races minio under high
parallelism; retry makes it reliable at `-P 6`). The load + apiserver-sampling
driver is not committed (it hardcodes the run's tokens, digests, and pod name);
its shape:

```bash
# per (mode x endpoint x concurrency): warm 3s, snapshot counters, hey 60s, snapshot again
osapi_image_total() {  # sum image.openshift.io request counter from openshift-apiserver
  oc exec -n openshift-apiserver "$OSPOD" -- curl -sk -H "Authorization: Bearer $TOK" \
    https://localhost:8443/metrics \
    | awk '/^apiserver_request_total\{/ && /group="image.openshift.io"/ {s+=$NF} END{print s+0}'; }
native_res() {         # sum registry native-read resolutions for one resolution label
  curl -s -H "Authorization: Bearer $METRICS_SECRET" $REG/extensions/v2/metrics \
    | awk -v r="resolution=\"$1\"" '/^imageregistry_native_read_resolutions_total\{/ && $0~r {s+=$NF} END{print s+0}'; }
a0=$(osapi_image_total); n0=$(native_res native); f0=$(native_res fallback)
hey -z 60s -c "$C" -m "$METHOD" -H "Authorization: Bearer $TOK" -H "Accept: $ACCEPT" "$URL"
a1=$(osapi_image_total); n1=$(native_res native); f1=$(native_res fallback)
# image-API qps = (a1-a0)/60 ; fallback delta = f1-f0
```

The outage drill served `oci:` layout copied from minio (`aws s3 sync`) through
`./_output/local/bin/distribution serve <fs-config>` (filesystem driver, no
`auth`, no `kubeconfig`), and pulled with `hey`/`skopeo` against `nrpoc-native/*`.

## Open questions / next steps

- **Workstream 2 (correctness harness, cluster).** Extend `test/integration/`:
  v2 + OCI push-then-pull digest/byte equality in both modes; `crossmount`
  (expected first failure); `deletemanifest` ghost check; imagestream-side
  delete/retarget skew measurement (drives the tombstone design).
- **Workstream 3 (perf + outage, cluster).** SNO/CRC with minio and filesystem
  backends; seed ~1k repos × 10 tags; load manifest-by-tag/-by-digest and blob
  HEAD/GET; compare route p99, `apiserver_request_total` (image verbs), etcd op
  rate across the reporegex A/B split; apiserver outage drill with auth switched
  to distribution-native for the drill only.
- **Workstream 4 (projection controller stub, cluster-free start).** A
  level-triggered controller consuming notification webhooks — events enqueue a
  repo, reconcile re-lists the tag store and patches ImageStream status. The
  reconcile logic and chaos tests (drop 30% of events, kill/restart mid-stream,
  verify resync converges) can run against fakes; push→CR lag under real load
  needs a cluster.
- **Backfill design (written, not built).** The fallback rate only drops to zero
  once existing clusters' tags/revisions are written into the native layout. A
  backfill design is a go/no-go deliverable (criterion 5) and is also the real
  fix for the accepted skews if paired with a tombstone/generation mechanism.
- **Tombstone / generation mechanism.** The two accepted skews both need it:
  either a registry-side tag delete that removes the native link, or routing CR
  tag-writes through the registry. Must be designed before the hybrid can serve
  correctness-sensitive repos.
