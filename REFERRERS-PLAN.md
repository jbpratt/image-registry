# Implementation plan: OCI Distribution Spec 1.1 Referrers API

Target repo: `openshift/image-registry`. Assumes the vendored fork
(`openshift/docker-distribution/v3`) has been bumped to upstream v3.1.1.

## Verified facts (do not re-litigate these)

1. **Distribution v3.1.1 provides nothing for referrers.** Verified against the
   v3.1.1 source tree: no referrers endpoint, no `subject`/`artifactType` fields
   in `manifest/ocischema` or `manifest/manifestlist`. Everything in this plan is
   implemented in this repo. **No fork changes are required.**
2. Manifests containing a `subject` field are already accepted today: the
   deserializers ignore unknown JSON fields but preserve the exact canonical
   bytes (`DeserializedManifest.canonical`), so `subject`/`artifactType` survive
   storage round-trips. **Rule: always read/write index content as raw payload
   bytes, never through the typed structs** — the vendored
   `distribution.Descriptor` has no `ArtifactType` field and would silently drop
   it.
3. A manifest pushed **by digest with no tag** creates a cluster `Image` object
   (`manifestservice.go:183`) but is **not** a member of the imagestream —
   `GetImageOfImageStream` (`pkg/imagestream/imagestream.go:201`) only finds
   images in tag history or listed as children of a tagged manifest list via the
   imagestream layers API. So a bare OCI-1.1 referrer push is stored but
   unreadable. The design below fixes this as a side effect.
4. Custom routes get the full openshift middleware stack for free:
   `app.RegisterRoute` (fork addition, `registry/handlers/app.go:477`) with
   `handlers.NameRequired` makes the dispatcher populate `ctx.Repository` with
   the wrapped tag/manifest services (auth, imagestream backing, pullthrough).
   Precedent: `signaturehandler.go`.
5. `app.context()` does **not** put the `http.ResponseWriter` in the context, so
   `manifestService.Put` cannot set response headers. The `OCI-Subject` header
   is done with an HTTP middleware instead (step 6).
6. Tag names `sha256-<64 hex>` (71 chars) are valid imagestream tags — cosign
   already pushes longer ones (`sha256-<hex>.sig`) today.

## Design (read this twice)

**Serve the referrers API as a view over the OCI "referrers tag schema", with
the registry maintaining the index tag server-side.**

Per subject digest `sha256:abc…`, the referrers index is an OCI image index
stored as a normal tagged manifest at tag `sha256-abc…` (the spec's fallback
tag: `digest.String()` with `:` → `-`). On every manifest PUT whose payload
contains `subject`, the registry appends a descriptor to that index and pushes
the updated index through the **existing** `manifestService.Put` path with
`distribution.WithTag`. `GET /v2/<name>/referrers/<digest>` resolves the tag,
loads the index payload, optionally filters, and returns it.

Why this is the right design here (do not substitute another):

- **One source of truth, zero new storage schema.** No link files, no
  annotations, no new API objects. The index is an ordinary tagged image, so
  quota, auth, audit, mirroring, and `oc adm prune images` all treat it
  correctly with no changes.
- **Fixes referrer readability.** The referrer manifest becomes a child of a
  tagged index, so `GetImageOfImageStream` finds it via the layers API — the
  exact mechanism manifest-list children use today.
- **Interoperable both ways.** Artifacts pushed via the client-side fallback
  (old ORAS, cosign) land in the same tag; enabling/disabling the feature never
  strands data. When the API is enabled we return `OCI-Subject` on PUT, so
  conformant clients stop maintaining the tag themselves — no double writes.
- **Termination guarantee:** the index we push has no `subject`, so the PUT
  hook recurses exactly once.

Feature is config-gated, **default off**. Disabled = today's behavior (GET
route not registered → 404 → clients use their fallback, which already works).

### Non-goals for v1 (write these in the PR description; do not implement)

- No pullthrough/proxy of referrers from remote registries (imported images
  return an empty index — spec-conformant).
- No pagination (`Link` header). The index is bounded by etcd object size;
  acceptable for v1.
- No referrer removal on manifest delete (this registry rejects deletes of
  stream-member manifests anyway; pruning is a documented follow-up).
- Known race: concurrent referrer pushes to the same subject do
  read-modify-write on one tag; last writer wins. The spec's client-side
  fallback has the identical race. Document it, don't engineer around it.

---

## Step 0 — Preflight (after the fork bump lands)

Re-verify fact 2 against the new vendor drop:

```
rg -n "subject" vendor/github.com/distribution/distribution/v3/manifest/{ocischema,manifestlist}/*.go
```

Expect no hits (or, if 3.1.1+patches added typed fields, the raw-bytes rule
still applies — do not switch to typed structs). Also confirm
`RegisterRoute` still exists in `vendor/.../registry/handlers/app.go`.

## Step 1 — Config flag

File: `pkg/dockerregistry/server/configuration/configuration.go`.

Mirror the `Compatibility` struct pattern exactly:

```go
type Referrers struct {
	Enabled bool `yaml:"enabled"`
}
```

- Add `Referrers *Referrers \`yaml:"referrers"\`` to `Configuration`.
- In the defaulting/migration code (where `Pullthrough`/`Compatibility` are
  defaulted around line 408), default to `&Referrers{Enabled: false}` when nil.
- No new env-var constant needed; the openshift config section already maps
  `REGISTRY_OPENSHIFT_REFERRERS_ENABLED` through the same parser the other
  sections use — follow whatever the `Compatibility` section does and copy it.
- Add a case to the existing config unit tests (same file's `_test.go`):
  unset → false; yaml `referrers: {enabled: true}` → true.

## Step 2 — Route constant

File: `pkg/dockerregistry/server/api/urls.go`. The referrers route lives under
`/v2/` (spec), not under the extensions prefix:

```go
ReferrersPath = "/v2/{name:" + reference.NameRegexp.String() + "}/referrers/{digest:" + reference.DigestRegexp.String() + "}"
```

Note the mux `{digest:…}` var only matches syntactically valid digests; a GET
with garbage after `/referrers/` will 404 from mux. That is acceptable — but
still call `digest.Parse` in the handler and return 400 on error (belt and
suspenders; the spec requires 400 for invalid digests).

## Step 3 — Shared referrers helpers

New file: `pkg/dockerregistry/server/referrers.go`. Everything below in
package `server`.

```go
const mediaTypeImageIndex = "application/vnd.oci.image.index.v1+json"

// referrersTag returns the OCI referrers fallback tag for a subject digest:
// "sha256:abc" -> "sha256-abc".
func referrersTag(dgst digest.Digest) string {
	return strings.Replace(dgst.String(), ":", "-", 1)
}

// referrerDescriptor is a raw OCI descriptor. Do NOT replace with
// distribution.Descriptor: it lacks ArtifactType and would drop it on marshal.
type referrerDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       digest.Digest     `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// referrersIndex is the raw OCI image index used both as the stored fallback
// index and as the GET response body.
type referrersIndex struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Manifests     []referrerDescriptor `json:"manifests"`
}

// manifestSubjectFields are the OCI 1.1 fields we extract from a raw manifest
// payload (typed structs ignore them).
type manifestSubjectFields struct {
	ArtifactType string `json:"artifactType"`
	Config       *struct {
		MediaType string `json:"mediaType"`
	} `json:"config"`
	Subject *struct {
		Digest digest.Digest `json:"digest"`
	} `json:"subject"`
	Annotations map[string]string `json:"annotations"`
}
```

Plus a helper that builds the descriptor for a just-pushed referrer, per spec:
`artifactType` = manifest's `artifactType` if set, else `config.mediaType` if
present (image manifests), else empty (indexes have no config).

## Step 4 — GET handler + registration

New file: `pkg/dockerregistry/server/referrershandler.go`, modeled directly on
`signaturedispatcher.go`/`signaturehandler.go`.

```go
func RegisterReferrersHandler(app *handlers.App) {
	getReferrersAccess := func(r *http.Request) []auth.Access {
		return []auth.Access{{
			Resource: auth.Resource{
				Type: "repository",
				Name: dcontext.GetStringValue(dcontext.WithVars(app, r), "vars.name"),
			},
			Action: "pull",
		}}
	}
	app.RegisterRoute(
		"referrers-get",
		app.NewRoute().Path(api.ReferrersPath).Methods("GET"),
		NewReferrersDispatcher(),
		handlers.NameRequired,
		getReferrersAccess,
	)
}
```

Handler logic (the dispatcher receives `*handlers.Context`; use
`ctx.Repository` — it is fully wired, per verified fact 4):

1. `dgst, err := digest.Parse(vars.digest)` → on error, serve
   `v2.ErrorCodeDigestInvalid` (400) via `errcode.ServeJSON` (copy the
   `handleError` helper from `signaturedispatcher.go:195`).
2. `desc, err := ctx.Repository.Tags(ctx).Get(ctx, referrersTag(dgst))`
   - `distribution.ErrTagUnknown` → **empty index, HTTP 200** (spec requires
     200 even when the subject doesn't exist).
   - `v2.ErrorCodeNameUnknown` (repo/imagestream missing) → serve it (404).
   - other errors → `errcode.ErrorCodeUnknown` (500).
3. `ms, _ := ctx.Repository.Manifests(ctx)`; `mfst, err := ms.Get(ctx, desc.Digest)`;
   errors → 500. `_, payload, _ := mfst.Payload()`.
4. `json.Unmarshal(payload, &idx)` into `referrersIndex`. If it doesn't parse
   or isn't an index (no `manifests` key), log a warning and serve an empty
   index — never 500 on corrupt tag content.
5. If query param `artifactType` is set: keep only descriptors with equal
   `ArtifactType`, and set header `OCI-Filters-Applied: artifactType`.
6. Respond 200 with body = marshaled `referrersIndex`
   (`SchemaVersion: 2`, `MediaType: mediaTypeImageIndex`, `Manifests` never
   nil — use `[]referrerDescriptor{}`), header
   `Content-Type: application/vnd.oci.image.index.v1+json`.

Registration in `pkg/dockerregistry/server/app.go`, next to
`RegisterSignatureHandler` (line ~168):

```go
if extraConfig.Referrers != nil && extraConfig.Referrers.Enabled {
	RegisterReferrersHandler(dockerApp)
}
```

## Step 5 — PUT hook: maintain the index

File: `pkg/dockerregistry/server/manifestservice.go` (+ helper in
`referrers.go`).

Wiring: add `referrersEnabled bool` to `manifestService`; set it in
`repository.go` `Manifests()` from `r.app.config.Referrers` (nil-safe), next to
`acceptSchema2`.

Changes to `Put` (`manifestservice.go:106`):

1. **Early, right after `mh.Payload()`:** parse `payload` into
   `manifestSubjectFields`. If `referrersEnabled` and `Subject != nil` and
   `Subject.Digest.Validate() != nil`, return
   `regapi.ErrorCodeManifestInvalid.WithDetail(...)` — reject before storing.
2. **At the end:** both exit paths (the `pushByDigest` branch at line ~182 and
   the `CreateImageStreamMapping` branch) currently `return dgst, nil`
   separately. Restructure so both fall through to one shared tail:

```go
if m.referrersEnabled && subject != nil {
	if err := m.indexReferrer(ctx, dgst, mediaType, payload, subject); err != nil {
		return "", errcode.ErrorCodeUnknown.WithDetail(fmt.Sprintf("failed to index referrer for subject %s: %v", subject.Digest, err))
	}
}
return dgst, nil
```

   Failing the push when indexing fails is deliberate: a 201 must mean "the
   referrer is listable" (the client will not run its fallback).
   Note: in the `pushByDigest` branch, `kapierrors.IsAlreadyExists` must also
   fall through to the hook (idempotent re-push of an existing referrer must
   still ensure it is indexed).

`indexReferrer(ctx, dgst, mediaType, payload, parsed)`:

1. `tag := referrersTag(parsed.Subject.Digest)`.
2. Read current index: `tags, err := m.imageStream.Tags(ctx)`. If the stream
   doesn't exist yet, or `tags[tag]` is absent → start with an empty
   `referrersIndex`. Otherwise `m.Get(ctx, tags[tag])`, take `.Payload()`,
   unmarshal into `referrersIndex` (unparseable → treat as empty, log warning).
3. If any existing descriptor has `Digest == dgst` → return nil (idempotent).
4. Append `referrerDescriptor{MediaType: mediaType, Digest: dgst,
   Size: int64(len(payload)), ArtifactType: <per step-3 helper>,
   Annotations: parsed.Annotations}`.
5. Marshal the index, then wrap it for storage:

```go
idx := &manifestlist.DeserializedManifestList{}
if err := idx.UnmarshalJSON(raw); err != nil { return err }
_, err = m.Put(ctx, idx, distribution.WithTag(tag))
```

   `UnmarshalJSON` stores `raw` as the canonical payload, so `artifactType`
   fields survive even though the typed struct drops them (verified fact 2).
   The raw JSON must set `"mediaType": "application/vnd.oci.image.index.v1+json"`
   or the manifestlist deserializer will reject it. The recursive `m.Put`
   terminates because the index has no `subject` (step 1 parse finds nil).

## Step 6 — `OCI-Subject` response header

The spec: when the registry processes a subject-bearing manifest PUT, the 201
response MUST carry `OCI-Subject: <subject digest>`; clients use it to skip
their fallback maintenance. `manifestService.Put` can't reach the
`ResponseWriter` (verified fact 5), so do this as a plain HTTP middleware.

New file: `pkg/dockerregistry/server/referrersmiddleware.go`:

- `func referrersSubjectMiddleware(next http.Handler) http.Handler`.
- Only act when: method is PUT and path matches
  `^/v2/.+/manifests/[^/]+$` (compile the regexp once, package-level).
- Read the body up to 4 MiB + 1 (distribution's own manifest cap is 4 MiB).
  If it exceeded the cap, pass through with
  `r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))` and do
  nothing else (distribution will reject it anyway). Otherwise restore
  `r.Body = io.NopCloser(bytes.NewReader(buf))`.
- Parse `buf` into `manifestSubjectFields`. If `Subject` is nil or the digest
  doesn't validate → plain `next.ServeHTTP`.
- Else wrap `w` in a tiny struct implementing `http.ResponseWriter` (pass
  through `Flusher` if the inner writer supports it) whose `WriteHeader`
  sets `OCI-Subject: <subject digest>` first **iff** status == 201, then
  delegates. Implicit-200 writes don't matter here (PUT manifest always calls
  WriteHeader explicitly), but have `Write` call `WriteHeader(200)` first if
  the header wasn't written, for correctness.

Hook it up in `app.go` `NewApp`, guarded by the same flag, wrapping `h`
*after* `h := http.Handler(dockerApp)` (line ~178):

```go
if extraConfig.Referrers != nil && extraConfig.Referrers.Enabled {
	h = referrersSubjectMiddleware(h)
}
```

The 201-only condition keeps the invariant: header present ⇒ `Put` succeeded ⇒
`indexReferrer` succeeded (step 5 fails the push otherwise).

## Step 7 — Tests

Follow existing patterns; `pkg/dockerregistry/server/manifestservice_test.go`
and `signaturedispatcher_test.go` show how to build a `manifestService` /
handler against `pkg/testutil` + `fakeopenshift`.

Unit tests (required):

1. `referrers.go`: `referrersTag`, descriptor `artifactType` derivation
   (artifactType set / config fallback / index with neither), filter logic.
2. `manifestservice_test.go` additions:
   - push manifest with subject, referrers enabled → index image exists at the
     fallback tag, contains exactly one descriptor with correct
     mediaType/digest/size/artifactType/annotations (assert on the **raw
     payload** of the stored index).
   - second referrer, same subject → two descriptors; re-push of the first →
     still two (idempotency).
   - subject with malformed digest → `ErrorCodeManifestInvalid`, nothing
     stored.
   - referrers disabled → subject-bearing push succeeds, no index tag created.
   - manifest without subject → hook not invoked (no extra API calls).
3. Handler test (`referrershandler_test.go`): 200+empty index for unknown
   subject; 200+filtered index with `?artifactType=`, asserting
   `OCI-Filters-Applied` and `Content-Type` headers; 400 for an invalid digest.
4. Middleware test: PUT body with subject → 201 response gains `OCI-Subject`;
   without subject → no header; non-manifest paths untouched; body reaches the
   inner handler intact (compare bytes).

Commands (must pass before the PR):

```
make verify
make test-unit
make test-integration
```

Manual acceptance (kind of check a reviewer will do; document results in PR):

```
# against a cluster/local registry with REGISTRY_OPENSHIFT_REFERRERS_ENABLED=true
oras attach --artifact-type application/example <repo>@<digest> ./file.txt
oras discover <repo>@<digest>            # must list the attachment, via API not tag
curl .../v2/<ns>/<repo>/referrers/<digest>              # 200, index
curl '.../v2/<ns>/<repo>/referrers/<digest>?artifactType=application/example'
cosign sign --registry-referrers-mode=oci-1-1 <repo>@<digest>
```

Optional follow-up (separate PR): wire the OCI distribution-spec conformance
suite (referrers workflow) into CI.

## Edge-case reference table (GET /v2/<name>/referrers/<digest>)

| Case | Response |
|---|---|
| valid digest, referrers exist | 200, OCI index, `Content-Type: application/vnd.oci.image.index.v1+json` |
| valid digest, none exist (or subject never pushed) | 200, index with empty `manifests` array |
| `artifactType` filter applied | 200 + `OCI-Filters-Applied: artifactType` header |
| syntactically invalid digest | 400 `DIGEST_INVALID` |
| repo / imagestream unknown | 404 `NAME_UNKNOWN` |
| unauthenticated / unauthorized | 401 / 403 (existing auth layer, untouched) |
| feature disabled | route not registered → 404 (clients fall back to tag schema) |

## Hard "do not" list for the implementer

- Do not modify the vendored fork or `go.mod` for this feature.
- Do not use `distribution.Descriptor` / typed manifestlist descriptors for
  index *content* — raw JSON structs only (typed carrier only for storage Put).
- Do not create the index anywhere except through `manifestService.Put` with
  `WithTag` (that is what makes quota/auth/prune/readability work).
- Do not return 404 from the referrers GET for a missing subject — 200 + empty.
- Do not set `OCI-Subject` on any status other than 201.
- Do not add pagination, remote pullthrough of referrers, or delete handling.
- Do not touch signature, admin, or metrics routes.
