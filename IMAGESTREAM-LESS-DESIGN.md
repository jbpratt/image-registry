# Design brainstorm: ImageStream-less registry mode

Phase 3 of ir-912. Phases 1–2 (`NATIVE-READ-POC.md`, `NATIVE-READ-DESIGN.md`)
serve the **read** hot path from distribution's native storage while keeping
etcd/ImageStreams authoritative for **writes**. This document asks the next
question: what does it take to run the registry when the ImageStream API is not
there at all — the MicroShift/edge shape. It is a brainstorm of options with a
recommended minimal POC, not a productization plan. No code is changed here.

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

**Recommended: C as the target, A for the first POC.** For a POC on a cluster
where the operator still ships the imagestream ClusterRoles, **A requires no
auth change** — the SAR against `imagestreams/layers` keeps working even with
the API disabled. For the *absent*/MicroShift shape, either **C** (a registry
-owned virtual resource with shipped RBAC, keeps kube identity + SAR) or **D**
(`htpasswd`/`token` for a truly kube-authz-less edge). The concrete auth.go
change for C is small and localized: `verifyImageStreamAccess` /
`verifyCatalogAccess` / `verifyPruneAccess` / `verifyImageSignatureAccess`
(`auth.go:523-555`) each name a resource string in one place; point them at the
new group/resource behind the mode flag. The token endpoint wiring
(`app.go:163`, gated on `Auth.Type()=="openshift"`) is unaffected by A/C.

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

**Recommended: C for the POC, B as the follow-on.** MicroShift pushes remote
mirroring down to CRI-O `registries.conf.d` (§2e), so a registry in this mode
does not need pullthrough at all — disable it and let the node runtime mirror.
If in-registry pullthrough is later required, B (explicit config mapping) is the
only per-repo option once istags are gone; A only suits a single-upstream mirror
appliance. Note `Pullthrough.Enabled` is currently *forced true*
(`configuration.go:445`), so "disable" needs that migration relaxed under the
mode flag.

### 2d. Image policy / triggers / pruning

| Capability | Where it lives today | In ImageStream-less mode | Upstream substitute |
|---|---|---|---|
| Build/deployment triggers on istag change | openshift-apiserver + controllers (not in this repo) | **Gone** — no istag to watch | None (by design; not the registry's job) |
| `image.openshift.io/ImagePolicy` admission | kube-apiserver admission plugin | **Gone** | None; MicroShift already disables this plugin (§2e) |
| Image signatures | `signaturedispatcher.go` via `imagesignatures`/`imagestreamimages` | **Gone** — no CR to store/read signatures | Sigstore/cosign at the client/CRI-O layer (out of registry scope) |
| Pruning (`oc adm prune images`, `dockerregistry prune`) | `pkg/dockerregistry/server/prune/` lists Image + ImageStream CRs | **Gone** — nothing to enumerate CRs | **`registry garbage-collect`** (distribution native GC over `_manifests`/`_layers`) once the native manifest+tag store is authoritative |
| Quota (per-project image quota) | `imagestreammappings` create is quota-checked (`imagestream.go`) | **Gone** — no ISM create path | None in-registry; rely on storage quota |

Net: everything CR-mediated disappears. The one capability with a real upstream
substitute is **pruning → distribution storage GC** (`registry
garbage-collect`), which becomes usable precisely because tag storage moves
native (2a-A): GC marks reachable manifests from tags and sweeps the rest.
Triggers, image policy, signatures, and quota have no in-registry substitute —
their absence is inherent to the mode, not a gap to close here.

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
ImageStream API disabled, on a cluster whose imagestream RBAC ClusterRoles still
exist (so auth needs no change — §2b-A). Smallest diff:

| # | File | Change |
|---|---|---|
| 1 | `configuration/configuration.go` | add `ImageStreams{Enabled bool}` block (default true); relax `Pullthrough` forced-true under the flag |
| 2 | `pkg/dockerregistry/server/app.go` | parse the flag into an `App` field (mirror `NativeReadPath` wiring) |
| 3 | `pkg/dockerregistry/server/repository.go` | when disabled: set `nativeRead=true` with no fallback; skip pullthrough wrapping; pass flag into manifest/tag services |
| 4 | `pkg/dockerregistry/server/manifestservice.go` | `Put`: skip ImageStreamMapping/Image CREATE when disabled (write native manifest revision only, which distribution already does) |
| 5 | `pkg/dockerregistry/server/tagservice.go` | `Get`/`All` native-only when disabled; make `Untag` delete the native tag link |

No auth.go change in the minimal POC (relies on surviving RBAC). No new API, no
controller, no operator change (set the config by hand for the POC).

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

## 4. Open questions for the human

1. **Auth model for the *absent* case (§2b).** Options: (A) keep SAR on
   `imagestreams/layers` — only works if RBAC roles are shipped independently;
   (C) new virtual resource `registry.openshift.io/repositories` + shipped
   ClusterRoles, keeping kube identity + SAR; (D) distribution-native
   `token`/`htpasswd` for kube-authz-less edge. **Recommendation: C** as the
   productization target (image-specific, keeps per-namespace kube RBAC), **D**
   only for a genuinely disconnected appliance. Confirm we want to keep kube SAR
   authorization at all in this mode.

2. **Tag storage backing (§2a).** (A) native `_manifests/tags`; (B) a registry
   -owned CRD; (C) none/v2-only. **Recommendation: A.** Accept losing all
   `oc`-ImageStream tooling (`oc tag`, imports, triggers, `oc get istag`)? Or is
   a kube-visible per-repo object (B) a hard requirement for RBAC/quota later?

3. **Pullthrough (§2c).** (A) whole-registry single-remote proxy; (B) explicit
   config repo→remote mapping; (C) none. **Recommendation: C** for edge (CRI-O
   mirrors handle it), **B** if in-registry per-repo pullthrough is required.
   Is any pullthrough needed in this mode, or is node-level mirroring the answer?

4. **Absent vs disabled, and auto-detection (§2f).** Recommendation: make the
   mode an **explicit config flag**, never auto-detected (an apiserver blip must
   not look like "absent"). Confirm we accept fail-fast behavior if the flag is
   unset on a cluster without the API, rather than a silent degrade.

5. **Is this "the MicroShift profile" (§2e)?** Recommendation: build it as the
   **edge / ImageStream-less profile** (MicroShift-*compatible*), documented as
   such, not literally MicroShift-only — MicroShift ships no registry today, so
   this is what it would take to run one there, and it may also serve OCP
   clusters disabling the image API for etcd-load reasons. Agree on the framing
   and name before the operator work starts.

6. **Signatures + image config metadata (§2d).** With no `images` CR, signature
   storage/serving and Image-CR-sourced config metadata disappear. Recommendation:
   drop both in this mode and defer signing to sigstore/cosign at the CRI-O/client
   layer. Confirm no consumer in the target environment needs registry-served
   signatures or the Image CR config blob.
