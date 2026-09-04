# POC plan: native-read / etcd-write hybrid registry

Target repo: `openshift/image-registry`. Goal: evaluate serving the **pull hot
path** (tag resolution, manifest GET, blob stat) from distribution's native
storage layout instead of the ImageStream/etcd path, while leaving the **push
path unchanged** (synchronous ImageStreamMapping). This is an experiment plan
with kill criteria, not a productization plan.

## Background (why this is plausible now, not in 2015)

The original design put etcd in the pull path because three constraints held:

1. ImageStream triggers required etcd **watch** semantics; you can't watch an
   S3 prefix.
2. S3 was **eventually consistent** (until Dec 2020) — tags are overwritten
   files in the native layout, so native reads meant unbounded staleness.
3. etcd was the only consistent store already on the payload; adding no new
   stateful component was the whole operational argument.

All three have expired: S3 is strongly consistent, DeploymentConfigs are
deprecated and trigger criticality has collapsed (MicroShift ships without
imagestreams), and etcd went from "free" to the most protected resource in
large clusters. Precedent: the 3.7–3.10 manifest migration already moved
manifest payloads out of etcd for size reasons — this is the second
installment of the same retreat.

## Verified facts (grounded in this tree)

1. The hot path hits the apiserver for **two distinct reasons**: authz
   (`server/auth.go`, SubjectAccessReviews per repo scope) and metadata
   (`server/tagservice.go`, `server/manifestservice.go`,
   `server/blobdescriptorservice.go`, all resolving through
   `pkg/imagestream/`). This POC isolates **metadata only**; auth is held
   constant across every A/B measurement so it cannot confound results.
2. The seam is clean: `supermiddleware/app.go` hands `App.Repository`
   (`server/repository.go:60`) the **upstream distribution repository** before
   the OpenShift services are substituted. The native `Tags()`, `Manifests()`,
   and linked-blob-store services are reachable there — the core change is
   *conditionally not replacing them*, not building new ones.
3. Upstream distribution's storage layout is metadata-complete for
   locally-pushed **manifests and layers** (`_manifests/revisions/`,
   `_layers/.../link`, `blobs/`) but **not tags**: this registry's
   `tagService.Tag` is a no-op (the tag is recorded only via the
   ImageStreamMapping), so `_manifests/tags/` is empty on every existing
   cluster. A native tag read can only ever hit for tags pushed *after* the
   registry starts dual-writing the tag link (workstream 1 does this). What
   the layout cannot express at all: pullthrough (tag → remote mapping),
   cross-namespace istag refs, images created only via API (`oc tag`,
   `oc import-image`) — those exist **only in etcd**. Native reads therefore
   require a fallback path, and the fallback rate is a first-class POC
   measurement (it quantifies real-world migration debt).
3a. Blob descriptor `Stat` is **already storage-first** in production
   (`blobdescriptorservice.go`: repo layer link → global blob store →
   imagestream `HasBlob`). The apiserver is only consulted on a link miss.
   H1's apiserver savings therefore come from tag and manifest resolution;
   for blobs the POC only adds measurement of the existing link-miss rate.
4. Push (manifest PUT) creates the ImageStreamMapping synchronously today.
   The hybrid keeps this byte-for-byte, so push-then-read consistency
   (triggers, image policy admission, CI doing `oc get istag` after push) is
   unchanged. Only the *read source* moves.
5. Registry notification events are at-least-once and unordered across
   replicas. Any projection controller must be **level-triggered** (events are
   hints; reconcile re-reads storage as truth). Edge-triggered application of
   events produces permanent skew on the first lost/reordered event.
6. Two-masters hazard: ImageStreams accept writes (`oc tag`, istag delete,
   scheduled imports). A projection is only coherent one-directionally. The
   POC sidesteps this by keeping etcd authoritative for writes; the follow-on
   design must route CR tag-writes through the registry or drop those verbs
   for native repos. Out of scope here, but the report must restate it.
   **Concrete consequence in the POC (flagged as blocking by review, accepted
   as a known finding):** with native reads on, content removed from an
   imagestream is still served from storage until pruning removes the link.
   `oc tag -d` (or istag delete) makes the imagestream path return 404 for the
   manifest digest, but the native path keeps serving it; a tag moved with
   `oc tag` keeps resolving to the old digest natively. The review's proposed
   fix (verify imagestream membership before returning native results) puts
   etcd back in the hot path and makes H1 unmeasurable, so the POC does not
   adopt it. Workstream 2 measures the window; the follow-on design owes a
   tombstone/generation mechanism (registry-side tag delete that removes the
   native link, or CR tag-writes routed through the registry).

## Hypotheses

- **H1 (perf)**: native reads materially cut pull p99 and remove
  apiserver/etcd load proportional to pull QPS.
- **H2 (availability)**: pulls succeed with openshift-apiserver down
  (metadata claim only; full story also needs token-service auth, out of
  scope).
- **H3 (correctness)**: native reads are byte-identical to
  imagestream-mediated reads for registry-pushed images.
- **H4 (projection)**: a level-triggered controller converges ImageStream CRs
  with p99 lag < 2s and survives dropped/reordered events and controller
  restarts with no permanent skew.

## Non-goals

Pullthrough, pruning/GC, quota semantics, token-service auth, referrers,
migration/backfill of existing clusters. Anything these touch that breaks in
testing is a *finding to report*, not a bug to fix in the POC.

## Workstream 1 — read-path flag (core, ~3–4 days)

- Config knob `openshift.nativereadpath.enabled` plus `reporegex`, anchored
  as a full-string match on `namespace/name`, so one cluster runs both
  modes side by side in a single benchmark run.
- No new services. `tagService` and `manifestService` already hold the
  upstream storage-backed service as a field; a `nativeRead` flag on each
  makes them try that field first:
  - `tagService.Get` → upstream tag store first, imagestream on miss.
    `tagService.Tag` writes the native tag link (today it is a no-op; the
    ImageStreamMapping is created by the preceding manifest `Put`). Without
    this the native tag store is empty and tag resolution never hits
    (fact 3). `All` and `Lookup` stay imagestream-only: a native listing is
    incomplete by construction and neither is on the pull hot path.
  - `manifestService.Get` → upstream storage manifest store first,
    imagestream on miss. Pullthrough ordering is unchanged because
    `pullthroughManifestService` calls the local service first and only
    goes remote on `ErrManifestUnknownRevision`. `Exists` is untouched: no
    distribution handler calls it.
  - `BlobDescriptorService.Stat` → **unchanged** (fact 3a: already
    storage-first); the existing branches are instrumented instead.
  - **All writes and every read miss fall through to the existing
    imagestream path unchanged** (fact 3: etcd-only images must still
    resolve). Accepted cost: on a manifest miss the imagestream path
    re-checks storage, so a fallback pays one extra storage read. The
    fallback-rate metric bounds this; backfill is the real fix.
- Metrics: `imageregistry_native_read_resolutions_total{resolution,operation}`
  with `resolution=native|fallback|native_error`. Native lookups are
  classified before falling back: not-found errors count as `fallback`,
  anything else (S3 timeout, permission, I/O) is logged at warn and counts as
  `native_error`, so storage degradation is not mistaken for migration debt.
  A resolution is recorded only when a read is served; a native miss that
  the imagestream path also cannot serve counts nowhere. Reuse route-level
  p50/p99 histograms.
- Status: implemented on branch `worktree-native-read-poc`. Refined after
  an ultrareview on 2026-09-02 (blob wrapper removed, tag dual-write added,
  error classification, anchored regex), then folded into the existing
  services on 2026-09-02: the decorator types, the `Exists` override and
  the matcher struct were deleted, and the tests moved into
  `tagservice_test.go` and `manifestservice_test.go`.

## Workstream 2 — correctness harness (~1–2 days)

Extend `test/integration/`:

- `v2/` + `oci/`: push via registry API, pull via both modes, assert
  identical digests and bytes; tag overwrite then immediate GET.
- `crossmount/`: **expected first failure** — cross-repo blob mounts are
  authorized via imagestream metadata and native layer links may disagree.
  Whatever surfaces is a finding.
- `deletemanifest/`: delete via API, confirm the native path serves no ghost
  (rehearses the tombstone question for the eventual projection design).
- **Imagestream-side deletion (expected failure, fact 6)**: push by tag,
  then `oc tag -d` and, separately, `oc tag` to move the tag. GET by tag and
  by digest in both modes. Record the skew; this is the number the tombstone
  design must drive to zero.

## Workstream 3 — perf + outage evaluation (~2–3 days)

- **Environment**: SNO or CRC with minio S3 backend (the integration
  framework's fake control plane would understate the apiserver win);
  filesystem driver as a second data point.
- **Load**: seed ~1k repos × 10 tags. vegeta/`hey` against manifest-by-tag
  GET, manifest-by-digest GET, blob HEAD/GET; 1/10/50 concurrent pullers;
  cold and warm cache.
- **Measure**: registry route p50/p99; `apiserver_request_total` filtered to
  image-API verbs; etcd op rate. A/B via the repo-regex split or flag flip on
  one cluster.
- **Outage drill**: stop openshift-apiserver, run pull load with auth
  switched to distribution-native (htpasswd/none) *for this drill only* —
  isolates the metadata claim; document that production H2 additionally
  requires the auth phase.

## Workstream 4 — projection controller stub (~3–4 days, parallel)

- Standalone binary (`cmd/` or `hack/`): consumes notification webhooks,
  level-triggered per fact 5 — an event only enqueues the repo; reconcile
  re-lists the tag store and patches ImageStream status.
- Measure push→CR lag under load. Chaos: drop 30% of events, kill/restart
  the controller mid-stream, verify periodic resync converges.

## Go/no-go criteria

1. ≥90% reduction in image-API apiserver QPS under pure pull load;
   measurable p99 improvement (bar fixed after baseline run).
2. Zero content diffs in workstream 2; crossmount behavior understood and
   explainable, even if broken.
3. Pulls succeed for the full apiserver outage window.
4. Projection converges after every chaos scenario; lag p99 within bound.
5. Fallback rate measured on realistic data, with a written (not built)
   backfill design for driving it down.

## Open questions to carry into the POC

- `server/cache/` (repo-scoped blob descriptor cache): stat already comes
  from link files (fact 3a), so no invalidation change is needed. Note it
  does not cache negative results, so every link miss re-stats storage.
- Interplay with `REFERRERS-PLAN.md`: that design serves referrers as a view
  over imagestream-backed index tags; a native read path must resolve the
  same referrers tags from the tag store. Keep the two designs aligned
  before either hardens.
- Cross-repo blob mount authorization model for native repos (workstream 2
  will produce the data).

## Sequencing

Workstreams 1 → 2 → 3 are sequential-ish; 4 is fully parallel. Rough total:
~2 weeks focused. Riskiest unknown: crossmount. Most valuable single number:
the fallback rate.
