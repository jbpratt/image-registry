# Modernization plan: distribution bump, native reads, ImageStream-less mode

Organizes three POC lines of work into one advocacy plan. Organization only —
no new engineering, no re-derived measurements.

## Why

- **etcd/apiserver metadata load scales with pull QPS.** Every tag resolution,
  manifest GET, and blob stat hits the apiserver on the pull hot path today
  (NATIVE-READ-DESIGN.md "Problem and why now").
- **Measured cost is severe, not theoretical.** At concurrency 50, etcd-backed
  tag resolution drives image-API QPS to 239.75 vs 0.02 for native; manifest-
  by-digest drives it to 248.18 vs 0.03 (NATIVE-READ-DESIGN.md, Workstream 3
  perf tables). p99 also improves (tag: 1.308s -> 0.913s at conc50).
- **Pulls die when the apiserver does.** With the apiserver unreachable, a
  registry serving purely from native storage (no imagestream fallback) still
  completed 100% of full pulls (40/40, manifest+config+layer) and answered
  166047/166047 manifest-by-tag GETs at concurrency 10 over 60s; the
  ImageStream-coupled path cannot (NATIVE-READ-DESIGN.md, "Outage drill").
- **No registry at all without `image.openshift.io` on MicroShift/edge.**
  MicroShift ships no openshift-apiserver, no Image/ImageStream CRDs, and no
  in-cluster registry (IMAGESTREAM-LESS-DESIGN.md §2e). The current design
  cannot run there; an edge profile is the only way to offer a registry on
  that shape.
- **The original constraints that put etcd in the read path are gone.**
  Watch-based triggers are deprecated, S3 has been strongly consistent since
  Dec 2020, and etcd is now the most protected resource in large clusters, not
  the cheapest (NATIVE-READ-POC.md "Background").

## Target end state

`distribution` v3.1.1 is the vendored base. Native storage (`_manifests`,
`_layers`, `blobs`) is authoritative for both reads and writes on the pull hot
path; ImageStream/Image CRs are projected from native truth by a level-
triggered controller when the image API is present, so `oc` tooling, triggers,
and image policy keep working unchanged on standard OCP. On the edge/
ImageStream-less profile, the registry runs with no image API at all: tags
live only in native `_manifests/tags`, auth goes through a pluggable
non-openshift backend, and signatures are OCI 1.1 referrers instead of Image
CRs. One codebase, one flag (`openshift.imagestreams.enabled`), two shapes.

## Steps

| # | What | Depends on | Where it exists | Status |
|---|------|-----------|------------------|--------|
| 1 | distribution v3.1.1 bump (go.mod replace, library-go registryclient migration, reference import fix) | — | `fork/bump-distribution-v3.1.1` (4 commits over main) | done (PR ready, blocked on upstream replaces) |
| 2a | Native read path, WS1: config flag + reporegex, native-first tag/manifest read with imagestream fallback, dual-write on push | (1) | `fork/native-read-poc` (`1c5b26d6b`, `aa6568a94`, `8e7cd08c9`, `704b75f90`) | done (POC) |
| 2b | Native read path, WS2: correctness harness on CRC (schema2/OCI, crossmount, delete-ghost, skew, metrics) | (2a) | `fork/native-read-poc` (`618aff2a6`, `af95b715e`); results in `NATIVE-READ-DESIGN.md` | done (POC) |
| 2c | Native read path, WS3: perf (100x5 seed, apiserver QPS A/B) + apiserver outage drill | (2a) | `fork/native-read-poc` (`dd3d0f506`); results in `NATIVE-READ-DESIGN.md` | done (POC) |
| 2d | Native read path, WS4: level-triggered ImageStream projection controller stub (fakes only) | (2a) | `fork/native-read-poc` (`327c16676`, `3b015bb6d`) | done (POC, fakes only) |
| 3a | Write path: native write with ImageStream projection (production) | 2a, 2d | `NATIVE-READ-DESIGN.md` "Open questions" | not started |
| 3b | Native tag-store adapter behind `TagLister` (real cluster, replaces WS4 fakes) | (2d) | `NATIVE-READ-DESIGN.md` "needs a cluster" list | not started |
| 3c | Backfill design for existing clusters' tags/revisions | (2c) | `NATIVE-READ-DESIGN.md` "Open questions" (written design owed, not built) | not started |
| 3d | Tombstone/generation mechanism for the two accepted read skews | (2b) | `NATIVE-READ-DESIGN.md` Findings (accepted skews); design owed | not started |
| 4a | ImageStream-less flag + pluggable auth seam | (2a) | `fork/imagestream-less-poc` (`7244e82c9`) | done (POC) |
| 4b | Native-only tag/manifest services when imagestreams disabled | (4a) | `fork/imagestream-less-poc` (`00b51f43c`) | done (POC) |
| 4c | Unit tests for the disabled path | (4b) | `fork/imagestream-less-poc` (`5a46588ce`) | done (POC) |
| 4d | Startup guard (reject `imagestreams.enabled=false` + `auth: openshift`) + native catalog | (4b) | `fork/imagestream-less-poc` (`aba7c7c40`) | done (POC) |
| 5 | Auth option C: virtual resource `registry.openshift.io/repositories` + shipped ClusterRoles + kube SAR | (4a) | `IMAGESTREAM-LESS-DESIGN.md` §2b (decision: provisional, POC used option D/htpasswd) | not started (design written) |
| 6 | cluster-image-registry-operator changes (expose flag, stop depending on image API discovery, ship option-C ClusterRoles) | (5) | `IMAGESTREAM-LESS-DESIGN.md` §2f, §4 (flagged, not designed) | not started |
| 7 | GC reclaim (`registry garbage-collect` over native `_manifests`/`_layers`) | (4b) | `IMAGESTREAM-LESS-DESIGN.md` §2d; kill criterion 4, "not evaluated" | not started |
| 8 | Referrers (OCI 1.1) for signatures | (1) | `REFERRERS-PLAN.md`; sequenced after push/pull, off critical path per decision record | not started (design written) |

Status legend: **done** = merged to a listed branch and verified there; **done
(POC)** = implemented and measured on a POC branch, not productized (a further
parenthetical, e.g. "fakes only", notes what was stubbed); **not started** =
no branch, no doc beyond a flagged TODO (parenthetical "design written" when a
design doc exists with no code).

## Risks and open decisions

| Risk / decision | Recommended answer | Alternative |
|---|---|---|
| Bump PR (step 1) go.mod replaces point at temporary `jbpratt` forks, not upstream: distribution -> `github.com/jbpratt/openshift-docker-distribution/v3 v3.1.2-0.20260902182351-73f42b4a6360` (go.mod:174), library-go -> `github.com/jbpratt/library-go v0.0.0-20260904180027-24d7e91e6f81` (go.mod:184); be10d43c3 says both are temporary until the carries land in `openshift/docker-distribution` and the library-go PR merges into `openshift/library-go` | Land the two upstream PRs first, then re-point the replaces at `openshift/docker-distribution` and `openshift/library-go` | None — the bump PR cannot merge to main with fork replaces in go.mod |
| Auth: option C (virtual resource + SAR) vs D (distribution-native htpasswd) | **C** for productization — keeps kube identity and per-namespace SAR without `image.openshift.io`; ship the new ClusterRoles | D only for disconnected edge with no kube RBAC story; POC used D to prove the data path (IMAGESTREAM-LESS-DESIGN.md §2b, human explicitly unsure) |
| Accepted skew: stale tag/manifest links after `oc tag -d`/`oc tag` move, until pruning | Design and build the tombstone/generation mechanism (step 3d) before serving correctness-sensitive repos | Ship without it only for repos where staleness is tolerable; not recommended (NATIVE-READ-DESIGN.md Findings) |
| Backfill fallback rate on real clusters is unmeasured (POC measured 0% on freshly-pushed-only data) | Write the backfill design (step 3c) and measure fallback rate on a cluster with pre-existing `oc tag`/`oc import-image` images before go/no-go on criterion 5 | Ship the hybrid without backfill and accept an unknown, possibly high, fallback-driven perf regression; not recommended |
| Pullthrough loss under ImageStream-less mode | **C now** (disable pullthrough; MicroShift pushes mirroring to CRI-O `registries.conf.d`) | **B later** (explicit `openshift.pullthrough.mappings` config) if in-registry per-repo pullthrough is required (IMAGESTREAM-LESS-DESIGN.md §2c) |
| Auth+metadata coupling found in the outage drill: POC binary's openshift auth controller needs the apiserver even when native reads don't | Track as a productization blocker for the metadata-availability claim; the drill isolated the metadata claim by bypassing POC auth with a plain distribution registry, so the win is proven for storage, not yet for the shipped binary (NATIVE-READ-DESIGN.md "Outage drill") | None — auth must be solved (steps 5/6) before H2 is a shippable claim |
| POC measurements are single-node CRC + minio, not production scale (1 apiserver, no cross-AZ, 100x5 not 1k x 10) | Treat WS3 numbers as directional (>99% QPS cut, real); re-measure at scale before committing an SLO (NATIVE-READ-DESIGN.md "What this does not show") | None — this is a measurement gap, not a design choice |
| `oc` ImageStream tooling (`oc tag`, `oc import-image`, `oc get istag`, triggers) is lost under ImageStream-less mode | Accept the loss as scoped and expected for the edge/ImageStream-less profile; not applicable to the default OCP profile, which is untouched (IMAGESTREAM-LESS-DESIGN.md §2a, decision 2) | None proposed; option B (registry-owned CRD) deferred, not recommended, unless a future RBAC/quota requirement forces it |

## Ask

- Review the distribution v3.1.1 bump PR (step 1) and the two upstream
  carries it depends on (`openshift/docker-distribution`,
  `openshift/library-go`); merge the bump once those land and the replaces
  are re-pointed at upstream.
- Fund productizing the native read path behind the flag (steps 3a-3d): write
  path projection, tag-store adapter, backfill design, tombstone mechanism.
- Endorse the edge/ImageStream-less profile direction (step 4) and the auth
  option C target (step 5) so `cluster-image-registry-operator` work (step 6)
  can be scoped.
- Decide whether GC reclaim (step 7) and referrers (step 8) are scheduled
  alongside steps 3-6 or deferred until the write path lands.
