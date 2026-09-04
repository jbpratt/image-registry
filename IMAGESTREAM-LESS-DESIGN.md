# Design brainstorm: ImageStream-less registry mode

Phase 3 of ir-912. Phases 1–2 (`NATIVE-READ-POC.md`, `NATIVE-READ-DESIGN.md`)
serve the **read** hot path from distribution's native storage while keeping
etcd/ImageStreams authoritative for **writes**. This document asks the next
question: what does it take to run the registry when the ImageStream API is not
there at all — the MicroShift/edge shape. It is a brainstorm of options with a
recommended minimal POC, not a productization plan. No code is changed here.

## Decisions (2026-09-04)

Human decisions on the §2/§4 options. Sections below are the detail; this is the
record.

1. **Auth — PROVISIONAL.** The POC uses **option D** (distribution-native
   `htpasswd`) to prove the data path; the productization target is **option C**
   (virtual resource `registry.openshift.io/repositories` + shipped ClusterRoles,
   kube SAR). The choice is explicitly unsettled, so the POC must **not hard-wire
   either** — the auth seam stays pluggable. *The seam in code terms:* the
   `AccessController` is selected by distribution's `auth:` config block (a map
   with exactly one key), resolved through the `registry/auth` registry. The
   OpenShift SAR controller is registered under `auth.Register("openshift", …)`
   (`supermiddleware/app.go:105`) and taken when `Auth.Type() == "openshift"`
   (`app.go:163`). Setting `auth: { htpasswd: … }` instead selects distribution's
   bundled htpasswd controller (`vendor/.../registry/auth/htpasswd`) — a different
   registered `AccessController`, no OpenShift auth code path, no rebuild. Target C
   is a new/extended registered controller issuing SARs against
   `registry.openshift.io/repositories`. The `openshift.imagestreams.enabled` mode
   flag does **not** pick the auth backend — the `auth:` key does; that
   independence is exactly what keeps the seam pluggable.
2. **Tag storage — option A** (native `_manifests/tags`). Loss of `oc`
   ImageStream tooling in this mode is accepted (§2a).
3. **Pullthrough — option C (none) first**, may move to B later. Concrete list of
   what C gives up in §2c.
4. **Mode — explicit config flag, never auto-detected.** Fail-fast at startup if
   the image API is unreachable and the flag is unset (§2f).
5. **Framing — "Edge / ImageStream-less profile"**, MicroShift-*compatible*, not
   MicroShift-only (§2e).
6. **Signatures — via OCI 1.1 referrers** per `REFERRERS-PLAN.md`, not the Image
   CR; Image-CR config metadata is dropped (§2d). Referrers are **not** in the
   minimal POC critical path — sequenced after push/pull round-trips.

## 1. Scope and assumptions

- **Opt-in, experimental.** A cluster explicitly selects this mode; the default
  OCP profile (ImageStreams present, etcd authoritative) is untouched.
- **No `image.openshift.io` API at all.** No `ImageStream`, `Image`,
  `ImageStreamMapping`, `ImageStreamImage`, `ImageStreamTag`, `ImageSignature`,
  `ImageStreamImport` served. This is stronger than phase 2: phase 2 falls back
  to the imagestream on a native miss; here there is nothing to fall back to.
- **Image CRs also absent.** No cluster-scoped `images` for manifest config /
  layer metadata / signatures. Storage is the only source of truth.
- **Kube core still present.** `authentication.k8s.io` (TokenReview,
  SelfSubjectReview) and `authorization.k8s.io` (SAR/SSAR) are kube-apiserver
  APIs and remain available; the registry can still authenticate and authorize
  against the kube apiserver. MicroShift ships exactly this shape (§2e).
- **Two triggers for the mode** and the registry must behave sanely for each
  (§2f): the API is *disabled* (served surface removed on an otherwise full
  OCP cluster; RBAC ClusterRoles may still exist) vs *absent* (never installed,
  as on MicroShift; the imagestream RBAC roles do not exist either).

### What the ImageStream API does for the registry today

Everything the registry does through `image.openshift.io` (via
`pkg/dockerregistry/server/client`), so we know what must be replaced:

| Purpose | Resource / verb | Anchor |
|---|---|---|
| tag → digest resolution | `imagestreams` GET (`Status.Tags`) | `imagestream.go:387`, `cachedimagestreamgetter.go:37` |
| manifest/image + config metadata | `images` GET | `image.go:55` |
| layer/blob membership | `imagestreams/layers` GET | `imagestreamhasblob.go:16` |
| push (record tag+image) | `imagestreammappings` CREATE, `imagestreams` CREATE | `imagestream.go:424,473` |
| image config on push-by-digest | `images` CREATE | `manifestservice.go:212` |
| pull secrets (pullthrough) | `imagestreams/secrets` GET | `imagestream.go:296` |
| signatures | `imagesignatures` CREATE / `imagestreamimages` GET | `signaturedispatcher.go:123,162` |
| pullthrough remote mapping | `imagestreams` `Status.Tags[].DockerImageReference` | `identifycandidaterepositories.go:52` |
| **authz only** (no data read) | SAR on `imagestreams/layers`, `imagestreams`, `images`, `imagesignatures` | `auth.go:529,538,546,554` |

Note the last row: `auth.go` never reads ImageStream *data* — it only issues
SARs whose `ResourceAttributes` name `image.openshift.io` resource strings. SAR
is evaluated by the RBAC authorizer purely on group/resource/verb strings, so it
does not require the API to be *served* — only that matching RBAC rules exist.

## 2. Options

### 2a. Tag storage

`tagService.Untag` is already a no-op (`tagservice.go:137`) and `Tag` only
writes the native `_manifests/tags` link under `nativeRead` (phase 1). The
question is where a tag lives with no ImageStream.

| Option | Mechanism | Tradeoffs | oc / build tooling impact |
|---|---|---|---|
| **A. Native tag store (distribution `_manifests/tags`)** ✅ | `vendor/.../registry/storage/tagstore.go`; push dual-writes the link, reads/`tags list` serve from it | Zero new components; strongly consistent (§ NATIVE-READ-POC background); the v2 `GET /v2/<name>/tags/list` works natively; already scaffolded in phase 1 | `oc get is/istag`, `oc tag`, `oc import-image` all gone (no CR to read/write); `oc new-app`/build triggers that watch istags stop firing; `oc adm prune images` can't enumerate via CRs |
| B. Registry-owned CRD (`registry.openshift.io/Repository` or `Tag`) | A minimal CRD the registry writes; a thin controller or the registry itself reconciles | Restores a kube-visible tag object for tooling/RBAC scoping, no `image.openshift.io` dependency | Still not an ImageStream: `oc tag`/imports/triggers remain broken; adds a CRD + write path to design, own GC/pruning; largest surface |
| C. None (v2 API only) | Tags live only in registry storage; clients use the Docker/OCI v2 API directly | Smallest; matches "just a registry"; exactly the MicroShift consumption model (CRI-O pulls by ref) | Everything ImageStream-shaped is gone, including `tags/list` semantics tied to istag; no kube object at all for tag RBAC |

**Recommended: A.** It is already half-built (phase 1 dual-write + native
read), needs no new API, and gives `podman push/pull` and `tags/list` for free.
The loss is the entire `oc`-ImageStream workflow (tag, import, triggers,
`oc get istag`) — acceptable and expected for this mode, and the same loss
MicroShift already accepts. B is a real option only if a future requirement
needs a kube-visible per-repo object for RBAC/quota; defer it.

### 2b. Auth

Today (`auth.go`): the bearer token is verified with `SelfSubjectReview`
(`auth.go:399`, `authentication.k8s.io` — kube), then per-repo access is a
`SelfSubjectAccessReview` (`auth.go:441`) with
`{group: image.openshift.io, resource: imagestreams/layers, verb: get|update}`
(`auth.go:508,529`). Catalog → `imagestreams list`; prune → `images delete`;
signature put → `imagesignatures create`. All are SARs; none read istag data.

| Option | Change to auth.go | Works when API *disabled* (RBAC present) | Works when API *absent* (MicroShift) | Tradeoffs |
|---|---|---|---|---|
| **A. Keep SAR, keep `imagestreams/layers`** ✅ for the *disabled* case | none | Yes — SAR is string-matched against surviving `system:image-puller/pusher/builder` ClusterRoles | No — those roles don't exist, SAR returns no-opinion → deny | Zero code; but silently breaks in the *absent* case |
| **B. SAR on a resource that always exists** | swap resource string to e.g. `pods`/`get` or `namespaces`/`get` per namespace | Yes | Yes (core kube RBAC exists everywhere) | Semantically wrong (pod access ≠ image access); over/under-grants; poor auditability |
| **C. New virtual resource `registry.openshift.io/repositories`** | swap group+resource; ship ClusterRoles binding it to the registry's service-relevant subjects | Needs roles shipped | Needs roles shipped (operator/manifests) | Clean, image-specific, decoupled from `image.openshift.io`; cost: define + ship RBAC, teach admins the new verb |
| **D. Distribution-native token / htpasswd / none** | select a non-`openshift` auth backend (`vendor/.../registry/auth/{token,htpasswd,silly}`) | n/a (bypasses kube authz) | Yes | Right answer for disconnected edge with no kube RBAC story; but drops per-namespace kube identity/SAR entirely — a policy decision, not a drop-in |

**Decided (PROVISIONAL): D for the POC, C as the productization target; the seam
stays pluggable.** The POC uses distribution-native **`htpasswd`** to prove the
data path end-to-end without any kube-authz dependency: select it with an
`auth: { htpasswd: … }` block, which makes distribution instantiate its bundled
`vendor/.../registry/auth/htpasswd` controller instead of the OpenShift SAR
controller (registered under `auth.Register("openshift", …)`,
`supermiddleware/app.go:105`; taken only when `Auth.Type()=="openshift"`,
`app.go:163`). Because the backend is chosen by the `auth:` config key — not by
the `openshift.imagestreams.enabled` mode flag — **neither auth model is
hard-wired**: the same binary runs htpasswd (POC), the OpenShift SAR controller
(default OCP), or a future C controller by config alone. The productization
target **C** is a new/extended registered `AccessController` that issues SARs
against `registry.openshift.io/repositories` (keeps kube identity + per-namespace
SAR without `image.openshift.io`); its `verifyImageStreamAccess` /
`verifyCatalogAccess` / `verifyPruneAccess` / `verifyImageSignatureAccess`
(`auth.go:523-555`) equivalents name the new group/resource string in one place.
The human is explicitly unsure between C and D; the seam above is what lets that
decision stay open. **A** (keep SAR on `imagestreams/layers`, relying on
surviving RBAC roles) remains viable only in the *disabled* case and is not the
POC choice.

### 2c. Pullthrough

Today pullthrough resolves the remote from istag data: `remoteGet` →
`GetImageOfImageStream` → the tag event's `DockerImageReference`
(`pullthroughmanifestservice.go:54`), candidate remotes from
`Status.Tags[].Items[].DockerImageReference`
(`identifycandidaterepositories.go:52`), creds from `imagestreams/secrets`
(`imagestream.go:296`). **None of this can exist without istags.**

| Option | Mechanism | Tradeoffs |
|---|---|---|
| A. Upstream distribution proxy mode (`proxy.remoteurl`) | `vendor/.../configuration` `Proxy{RemoteURL, Username, Password, Exec, TTL}` turns the *whole registry* into a pull-through cache of *one* upstream | Native, zero new code; but single-remote and registry-wide — incompatible with per-namespace multi-tenant push+pull; only fits a dedicated mirror deployment |
| **B. Explicit config repo→remote mapping** ✅ | new `openshift.pullthrough.mappings` (repo-prefix → remote URL + optional secret ref) consulted where `IdentifyCandidateRepositories` is today | Keeps per-repo pullthrough without istags; admin-declared instead of istag-derived; cost: a resolver + config schema, creds from a k8s Secret ref instead of `imagestreams/secrets` |
| C. Disable pullthrough entirely | `pullthrough.enabled=false` in this mode | Simplest; matches MicroShift (host CRI-O mirrors handle remote pulls, §2e); registry serves only locally-pushed content |

**Decided: C for the POC, B as the follow-on.** MicroShift pushes remote
mirroring down to CRI-O `registries.conf.d` (§2e), so a registry in this mode
does not need pullthrough at all — disable it and let the node runtime mirror.
If in-registry pullthrough is later required, B (explicit config mapping) is the
only per-repo option once istags are gone; A only suits a single-upstream mirror
appliance. Note `Pullthrough.Enabled` is currently *forced true*
(`configuration.go:445`), so "disable" needs that migration relaxed under the
mode flag.

#### What choosing C gives up

Every in-registry pullthrough behavior below reads istag data and disappears with
it. Each row: the code it lives in, what breaks for the user, and the CRI-O /
node-mirror alternative (or "none" if there is no node-runtime equivalent).

| # | Feature (code anchor) | What breaks | CRI-O / node-mirror alternative |
|---|---|---|---|
| 1 | Manifest pullthrough (`pullthroughmanifestservice.go:54` `remoteGet` → `GetImageOfImageStream`) | Pulling an **imported** image (istag present, manifest never pushed locally) — the manifest is only reachable via the istag's `DockerImageReference` | Node pulls the source ref directly via `registries.conf.d` mirror; no registry indirection |
| 2 | Blob pullthrough `Stat`/`ServeBlob` (`pullthroughblobstore.go:33,57`) | Serving layers of imported images not in local storage | Same — node pulls layers from upstream/mirror directly |
| 3 | Mirror-on-pull (`pullthroughblobstore.go:74,184` `storeLocalInBackground`; `pullthroughmanifestservice.go:107` `mirrorManifest`) | First pull no longer warms local storage from the remote | None in-registry; CRI-O keeps its own node image cache |
| 4 | Candidate-remote resolution (`identifycandidaterepositories.go:52`, from `Status.Tags[].Items[].DockerImageReference`) | Multi-source / failover remote resolution per istag (primary + secondary tag events) | `ImageDigestMirrorSet` / `registries.conf.d` mirror lists give per-node failover |
| 5 | Pullthrough pull secrets (`imagestream.go:296` `GetSecrets`, used `pullthroughmanifestservice.go:130`) | Authenticated pullthrough from private upstreams via the namespace's `imagestreams/secrets` | Node-level pull secrets in `/etc/containers` / kubelet pull secrets |
| 6 | Per-tag insecure transport (`identifycandidaterepositories.go:92-101`; `TagIsInsecure` at `pullthroughmanifestservice.go:150`) | `importPolicy.insecure` / `insecureRepository` annotation selecting insecure remote transport per istag | `insecure = true` on the mirror in `registries.conf` |
| 7 | Registry-side ICSP/IDMS/ITMS rewrite (`getImportContext` icsp/idms/itms at `pullthroughmanifestservice.go:135`) | The registry applying cluster mirror sets while resolving a remote pull | **Native to CRI-O** — the node already consumes IDMS/ICSP (or their transformed form on MicroShift); cleanest handoff |
| 8 | `reference-policy=local` istags (`oc tag --reference-policy=local`, `oc import-image`) | The whole local-reference-policy workflow: the istag's `DockerImageReference` is rewritten to the internal registry so every pull flows through it and pullthroughs to source | Pull the source ref directly with node mirror config; no "local" indirection exists |
| 9 | Cross-namespace istag refs / remote blob mounting (cross-mount auth `auth.go:346`; cross-repo blob descriptor) | Sharing an image across namespaces via istags, and cross-repo blob mount that resolves through pullthrough | **None** — a cross-namespace istag is an ImageStream construct with no node-runtime equivalent; images must be pushed/pulled by explicit ref |
| 10 | Builds pulling from istags (S2I/docker build `from`/`to` `ImageStreamTag`) | Build inputs/outputs that reference istags and rely on istag→registry resolution + pullthrough for imported base images | Reference images by registry ref directly; no istag inputs/outputs |

Rows 8–10 are workflow-level and partly originate outside this repo (import is
openshift-apiserver), but their **serving** side depends on the in-registry
pullthrough above, so they break with C.

### 2d. Image policy / triggers / pruning

| Capability | Where it lives today | In ImageStream-less mode | Upstream substitute |
|---|---|---|---|
| Build/deployment triggers on istag change | openshift-apiserver + controllers (not in this repo) | **Gone** — no istag to watch | None (by design; not the registry's job) |
| `image.openshift.io/ImagePolicy` admission | kube-apiserver admission plugin | **Gone** | None; MicroShift already disables this plugin (§2e) |
| Image signatures | `signaturedispatcher.go` via `imagesignatures`/`imagestreamimages` | **Gone** — no CR to store/read signatures | **OCI 1.1 referrers** per `REFERRERS-PLAN.md` (signatures stored/served as referrer artifacts in native storage, not the Image CR); sigstore/cosign at the client/CRI-O layer remains the verification path |
| Pruning (`oc adm prune images`, `dockerregistry prune`) | `pkg/dockerregistry/server/prune/` lists Image + ImageStream CRs | **Gone** — nothing to enumerate CRs | **`registry garbage-collect`** (distribution native GC over `_manifests`/`_layers`) once the native manifest+tag store is authoritative |
| Quota (per-project image quota) | `imagestreammappings` create is quota-checked (`imagestream.go`) | **Gone** — no ISM create path | None in-registry; rely on storage quota |

Net: everything CR-mediated disappears. Two capabilities have a real in-registry
substitute: **pruning → distribution storage GC** (`registry garbage-collect`),
usable precisely because tag storage moves native (2a-A) so GC marks reachable
manifests from tags and sweeps the rest; and **signatures → OCI 1.1 referrers**
(`REFERRERS-PLAN.md`) stored/served natively instead of via the `images` CR.
Referrers are **sequenced after** the push/pull round-trip — they are not on the
minimal POC critical path (§3). Image-CR-sourced config metadata is dropped
(storage is the only source of truth, §1). Triggers, image policy, and quota
have no in-registry substitute — their absence is inherent to the mode, not a gap
to close here.

#### Reconciling `REFERRERS-PLAN.md` with this mode

`REFERRERS-PLAN.md` is written against the ImageStream-backed registry: it makes a
pushed referrer readable by having `GetImageOfImageStream` find it as a child of
the tagged index via the imagestream layers API, and it stores the index through
`manifestService.Put` with `WithTag`, which creates an `ImageStreamMapping`.
Neither mechanism exists in the ImageStream-less profile, but the plan's core
design is unchanged — the index is an ordinary manifest stored at the OCI fallback
tag `sha256-<digest>`. Here that index is written and read through the same native
manifest revision store and native `_manifests/tags` store this mode already uses,
and `GET /referrers/<digest>` resolves the tag natively. So the signatures
decision (referrers, not the Image CR) holds as written; only the index's
storage/read path is native rather than imagestream-backed. Referrers stay
sequenced after the push/pull POC and are not implemented here.

### 2e. What MicroShift already does (and whether this is "the MicroShift profile")

Verified against `openshift/microshift` and `openshift/openshift-docs` source
(WebSearch was unavailable; citations are to the AsciiDoc/asset sources upstream
of the rendered docs):

- **No in-cluster registry.** The shipped image set is
  `cli, coredns, csi-snapshot-controller, haproxy-router, kube-rbac-proxy,
  lvms_operator, ovn-kubernetes-microshift, pod, service-ca-operator` — no
  registry (`assets/release/release-x86_64.json`). No registry controller under
  `pkg/controllers/`.
- **No openshift-apiserver ⇒ no `image.openshift.io`.** MicroShift embeds a
  patched kube-apiserver plus route-controller-manager, cluster-policy
  -controller, SCC manager only; it registers CRDs for route, SCC/rangeallocation,
  storage-version-migration, LVMS/CSI — **no Image/ImageStream** (`pkg/assets/crd.go`,
  `pkg/controllers/`). It explicitly disables the `image.openshift.io/ImagePolicy`
  admission plugin (`pkg/controllers/kube-apiserver.go`).
- **Images come from CRI-O mirror config, not ImageStreams.** Users write
  `/etc/containers/registries.conf.d/999-microshift-mirror.conf`
  (`modules/microshift-configuring-hosts-for-mirror.adoc`); `ImageDigestMirrorSet`
  from `oc-mirror` must be *transformed* into CRI-O format — MicroShift does not
  consume IDMS/ICSP live (`modules/microshift-oc-mirror-transform-imageset-to-crio.adoc`).
  Disconnected nodes embed images via osbuild/Containerfile
  (`modules/microshift-olm-deploy-ops-con.adoc`).
- **Excluded broadly:** openshift-apiserver, openshift-controller-manager,
  Cluster Operators, OLM catalogs / console (`modules/microshift-olm-considerations.adoc`).

*Caveat:* no single MicroShift doc sentence says "the image.openshift.io group is
excluded"; that conclusion is grounded in the source above (no openshift
-apiserver, no imagestream CRD/assets, ImagePolicy disabled, no registry image),
not a doc statement.

**Should this mode just be "the MicroShift profile"?** Largely yes — the target
shape (native tags, no pullthrough / CRI-O mirrors, GC-based pruning, no CR
tooling) *is* the MicroShift environment. But MicroShift ships **no registry at
all**, so "the MicroShift profile" is really "the profile that would let this
registry run *on a MicroShift-like cluster*." Recommend naming and documenting
it as the **edge / ImageStream-less profile** (MicroShift-compatible), not
literally "MicroShift mode," because it could also run on a full OCP cluster
with the image API disabled for load reasons.

### 2f. Opt-in switch

Config key: `openshift.imagestreams.enabled` (default **true**). Add an
`ImageStreams *ImageStreams{ Enabled bool }` block to `Configuration`
(`configuration/configuration.go:114`), parsed in `app.go` like `NativeReadPath`
(`app.go:124`). When `false`:

- **Reads:** force the native path unconditionally (reuse phase-1 `nativeRead`,
  but with no imagestream fallback — a miss is a 404, not a fallback).
- **Writes:** `manifestService.Put` skips `imagestreammappings`/`images` CREATE
  (`manifestservice.go:212`, `imagestream.go:424`); tag link written natively
  (`tagservice.go` `Tag`). `Untag` must actually delete the native link (today a
  no-op) since there is no etcd copy.
- **Pullthrough:** off (§2c-C); relax the forced-true migration
  (`configuration.go:445`) under the flag.
- **Auth:** unchanged for the *disabled* case; §2b-C/D for the *absent* case.

**Absent vs disabled behavior:**

| | API *disabled* (OCP, RBAC roles survive) | API *absent* (MicroShift) |
|---|---|---|
| Reads/writes | native only, flag set explicitly | native only, flag set explicitly |
| Auth | SAR on `imagestreams/layers` still resolves (unchanged) | SAR fails → needs §2b-C or D |
| Failure if flag *not* set | every istag call 404/errors → all pulls fail loudly | same, plus client construction may fail building image clients |

The registry cannot safely *auto-detect* absence and silently switch modes:
discovery races and a transient apiserver outage would look identical to
"absent." The mode must be an **explicit config choice**, and the registry
should fail fast with a clear error if istag calls are attempted while the flag
is unset on a cluster without the API.

**cluster-image-registry-operator** (out of this repo) would need to: expose the
new config knob, stop reconciling/depending on ImageStream RBAC + the image API
being present, ship the §2b-C ClusterRoles if that auth option is chosen, and
not gate the registry Deployment on `image.openshift.io` discovery. That is the
larger productization cost and is flagged, not designed, here.

## 3. Recommended minimal POC

Goal: `podman push` then `podman pull` succeed against the registry with the
ImageStream API disabled, using distribution-native **`htpasswd`** auth (§2b-D)
so the POC proves the native data path with no kube-authz dependency. Smallest
diff:

| # | File | Change |
|---|---|---|
| 1 | `configuration/configuration.go` | add `ImageStreams{Enabled bool}` block (default true); relax `Pullthrough` forced-true under the flag |
| 2 | `pkg/dockerregistry/server/app.go` | parse the flag into an `App` field (mirror `NativeReadPath` wiring); the OpenShift token endpoint stays gated on `Auth.Type()=="openshift"` (`app.go:163`), so it is inert under htpasswd |
| 3 | `pkg/dockerregistry/server/repository.go` | when disabled: set `nativeRead=true` with no fallback; skip pullthrough wrapping; pass flag into manifest/tag services |
| 4 | `pkg/dockerregistry/server/manifestservice.go` | `Put`: skip ImageStreamMapping/Image CREATE when disabled (write native manifest revision only, which distribution already does) |
| 5 | `pkg/dockerregistry/server/tagservice.go` | `Get`/`All` native-only when disabled; make `Untag` delete the native tag link |

**Auth for the POC is selected by config, not code:** an `auth: { htpasswd: … }`
block picks distribution's bundled htpasswd `AccessController` over the OpenShift
SAR controller (`Auth.Type()` in `app.go:163`; registration at
`supermiddleware/app.go:105`), so **no `auth.go` change** is needed and the SAR
path is simply not selected. No new API, no controller, no operator change (set
the config by hand for the POC). Productization auth (§2b-C) is out of the POC.

**Estimate: 5–8 focused days** (2–3 to wire the flag + write path, 2–3 to test
push/pull/list/delete on CRC with the image API disabled, 1–2 slack).

**Kill criteria:**

1. `podman push`+`pull`+`tags/list` do not round-trip byte-for-byte natively
   without any `image.openshift.io` call (verify via apiserver audit = zero image
   verbs on the hot path).
2. Deleting a tag/manifest natively leaves a servable ghost (the `Untag`/GC story
   is unsound) that cannot be closed without reintroducing a CR.
3. Auth cannot be made to work in the *absent* case with any of §2b-C/D within a
   few days (i.e., no viable kube-authz path off ImageStreams).
4. `registry garbage-collect` cannot reclaim space because the native
   manifest/tag store is not authoritative enough to compute reachability.

## 4. Decisions taken and productization follow-ons

The §2/§4 questions are resolved — see **Decisions (2026-09-04)** at the top.
Summary and the work that remains open after the decisions:

1. **Auth (§2b) — decided PROVISIONAL: D for POC, C as target, seam pluggable.**
   *Open:* pick C vs D for productization (human explicitly unsure); if C, define
   and ship the `registry.openshift.io/repositories` ClusterRoles and the new
   `AccessController`. The POC does not settle this.
2. **Tag storage (§2a) — decided: A** (native `_manifests/tags`), accepting the
   loss of `oc`-ImageStream tooling. No open item.
3. **Pullthrough (§2c) — decided: C for POC, B later.** *Open:* if in-registry
   per-repo pullthrough is later required, design the B config schema + resolver.
   The concrete C loss list is in §2c.
4. **Mode switch (§2f) — decided: explicit config flag, never auto-detected;
   fail-fast when unset on a cluster without the API.** No open item.
5. **Framing (§2e) — decided: "edge / ImageStream-less profile",
   MicroShift-*compatible*, not MicroShift-only.** No open item.
6. **Signatures + image config metadata (§2d) — decided: OCI 1.1 referrers per
   `REFERRERS-PLAN.md` (not the Image CR); Image-CR config metadata dropped;
   referrers sequenced after push/pull, off the POC critical path.** *Open:* the
   referrers store/serve design lives in `REFERRERS-PLAN.md`; keep the two docs
   consistent.

**Productization cost carried by cluster-image-registry-operator** (out of this
repo, unchanged from §2f): expose the config knob, stop depending on ImageStream
RBAC + the image API being present, ship the §2b-C ClusterRoles if C is chosen,
and not gate the registry Deployment on `image.openshift.io` discovery.
