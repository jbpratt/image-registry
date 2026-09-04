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
| 1. ≥90% apiserver QPS cut, p99 improvement | `apiserver_request_total` (image verbs) + route p99, A/B via reporegex | needs cluster |
| 2. Zero content diffs; crossmount explained | workstream 2 harness (digest/byte asserts) | not yet measured |
| 3. Pulls survive apiserver outage window | outage drill under pull load | needs cluster |
| 4. Projection converges after chaos; lag p99 bound | workstream 4 controller + chaos | needs cluster |
| 5. Fallback rate on realistic data + backfill design | `native_read_resolutions_total{resolution=fallback}` | not yet measured |
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
