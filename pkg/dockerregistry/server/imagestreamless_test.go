package server

import (
	"context"
	"testing"
	"time"

	"github.com/distribution/distribution/v3"
	dockercfg "github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	kubecache "k8s.io/apimachinery/pkg/util/cache"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/cache"
	registryclient "github.com/openshift/image-registry/pkg/dockerregistry/server/client"
	"github.com/openshift/image-registry/pkg/dockerregistry/server/configuration"
	"github.com/openshift/image-registry/pkg/dockerregistry/server/metrics"
	"github.com/openshift/image-registry/pkg/dockerregistry/server/supermiddleware"
	"github.com/openshift/image-registry/pkg/testutil"
)

// panicRegistryClient fails loudly if the registry tries to build an
// image.openshift.io client, which must never happen in the ImageStream-less
// profile.
type panicRegistryClient struct{}

func (panicRegistryClient) Client() (registryclient.Interface, error) {
	panic("registryClient.Client() must not be called when imagestreams are disabled")
}

func (panicRegistryClient) ClientFromToken(string) (registryclient.Interface, error) {
	panic("registryClient.ClientFromToken() must not be called when imagestreams are disabled")
}

// TestManifestServicePutImageStreamsDisabledSkipsISM checks that a manifest Put
// in the ImageStream-less profile writes only the native revision and makes no
// image.openshift.io calls (no Image / ImageStreamMapping create).
func TestManifestServicePutImageStreamsDisabledSkipsISM(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)

	_, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)
	osClient := registryclient.NewFakeRegistryAPIClient(nil, imageClient)

	bs := newTestBlobStore(nil, blobContents{
		"testblob:1":   []byte("{}"),
		"testconfig:2": []byte("{}"),
	})
	tms := newTestManifestService("user/app", nil)

	ms := &manifestService{
		serverAddr:           "localhost",
		manifests:            tms,
		blobStore:            bs,
		registryOSClient:     osClient,
		imageStreamsDisabled: true,
		acceptSchema2:        true,
		metrics:              metrics.NewNoopMetrics(),
	}

	manifest, err := testutil.MakeSchema2Manifest(
		distribution.Descriptor{Digest: "testconfig:2", Size: 2},
		[]distribution.Descriptor{{Digest: "testblob:1", Size: 2}},
	)
	if err != nil {
		t.Fatalf("could not make schema 2 manifest: %s", err)
	}

	// No withUserClient / withAuthPerformed on the context: the disabled path
	// must not depend on the openshift access controller.
	dgst, err := ms.Put(ctx, manifest, distribution.WithTag("latest"))
	if err != nil {
		t.Fatalf("Put returned error: %s", err)
	}
	if dgst == "" {
		t.Fatal("Put returned empty digest")
	}
	if tms.calls["Put"] != 1 {
		t.Errorf("native manifest Put calls = %d, want 1", tms.calls["Put"])
	}
	if actions := imageClient.Actions(); len(actions) != 0 {
		t.Errorf("expected zero image.openshift.io calls, got %#v", actions)
	}
}

// TestManifestServiceImageStreamsDisabledReadDelete checks that Get is
// native-only with no imagestream fallback and Delete goes to native storage,
// neither dereferencing the nil imageStream.
func TestManifestServiceImageStreamsDisabledReadDelete(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)

	manifest, err := testutil.MakeSchema2Manifest(
		distribution.Descriptor{Digest: "testconfig:2", Size: 2},
		[]distribution.Descriptor{{Digest: "testblob:1", Size: 2}},
	)
	if err != nil {
		t.Fatalf("could not make schema 2 manifest: %s", err)
	}
	_, payload, err := manifest.Payload()
	if err != nil {
		t.Fatal(err)
	}
	dgst := digest.FromBytes(payload)

	tms := newTestManifestService("user/app", map[digest.Digest]distribution.Manifest{dgst: manifest})
	ms := &manifestService{
		manifests:            tms,
		nativeRead:           true,
		imageStreamsDisabled: true,
		metrics:              metrics.NewNoopMetrics(),
	}

	if _, err := ms.Get(ctx, dgst); err != nil {
		t.Fatalf("Get returned error for stored manifest: %s", err)
	}

	// A native miss is a miss: no fallback to the (nil) imagestream, no panic.
	if _, err := ms.Get(ctx, unknownBlobDigest); err == nil {
		t.Fatal("Get for unknown digest returned no error, want a miss")
	}

	if err := ms.Delete(ctx, dgst); err == nil {
		t.Fatal("Delete returned nil, want the native store's error")
	}
	if tms.calls["Delete"] != 1 {
		t.Errorf("native manifest Delete calls = %d, want 1", tms.calls["Delete"])
	}
}

// TestTagServiceImageStreamsDisabledNative checks that every tag operation uses
// the native storage tag store only, never the (nil) imagestream, and that
// Untag deletes the native link.
func TestTagServiceImageStreamsDisabledNative(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)

	desc := distribution.Descriptor{Digest: "sha256:aaaa"}
	store := newTestTagService(map[string]distribution.Descriptor{"latest": desc})

	ts := tagService{TagService: store, imageStreamsDisabled: true}

	got, err := ts.Get(ctx, "latest")
	if err != nil {
		t.Fatalf("Get: %s", err)
	}
	if got.Digest != desc.Digest {
		t.Errorf("Get digest = %q, want %q", got.Digest, desc.Digest)
	}

	all, err := ts.All(ctx)
	if err != nil {
		t.Fatalf("All: %s", err)
	}
	if len(all) != 1 || all[0] != "latest" {
		t.Errorf("All = %v, want [latest]", all)
	}

	names, err := ts.Lookup(ctx, desc)
	if err != nil {
		t.Fatalf("Lookup: %s", err)
	}
	if len(names) != 1 || names[0] != "latest" {
		t.Errorf("Lookup = %v, want [latest]", names)
	}

	if err := ts.Tag(ctx, "v2", distribution.Descriptor{Digest: "sha256:bbbb"}); err != nil {
		t.Fatalf("Tag: %s", err)
	}
	if store.data["v2"].Digest != "sha256:bbbb" {
		t.Errorf("Tag did not write native link, got %#v", store.data["v2"])
	}

	if err := ts.Untag(ctx, "latest"); err != nil {
		t.Fatalf("Untag: %s", err)
	}
	if _, ok := store.data["latest"]; ok {
		t.Error("Untag did not delete the native tag link")
	}
	if store.calls["Untag"] != 1 {
		t.Errorf("native Untag calls = %d, want 1", store.calls["Untag"])
	}
}

// TestTagServiceImageStreamsDisabledTagFailsPush checks that a failed native
// tag write fails the push: with no ImageStreamMapping behind it, the native
// link is the only record.
func TestTagServiceImageStreamsDisabledTagFailsPush(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)

	ts := tagService{
		TagService:           failingTagService{err: distribution.ErrTagUnknown{Tag: "x"}},
		imageStreamsDisabled: true,
	}
	if err := ts.Tag(ctx, "latest", distribution.Descriptor{Digest: "sha256:abc"}); err == nil {
		t.Fatal("Tag returned nil on native write failure, want the error propagated")
	}
}

// TestRepositoryImageStreamsDisabledNoClient checks that constructing a
// repository and its services in the ImageStream-less profile builds no
// image.openshift.io client (panicRegistryClient would panic) and accepts a
// non-namespaced repository name.
func TestRepositoryImageStreamsDisabledNoClient(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)

	cfg := &configuration.Configuration{
		Server:       &configuration.Server{Addr: "localhost:5000"},
		Cache:        &configuration.Cache{},
		ImageStreams: &configuration.ImageStreams{Enabled: false},
	}
	if err := configuration.InitExtraConfig(&dockercfg.Configuration{}, cfg); err != nil {
		t.Fatal(err)
	}

	digestCache, err := cache.NewBlobDigest(
		defaultDescriptorCacheSize,
		defaultDigestToRepositoryCacheSize,
		24*time.Hour,
		metrics.NewNoopMetrics(),
	)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{
		registryClient:       panicRegistryClient{},
		config:               cfg,
		cache:                digestCache,
		quotaEnforcing:       &quotaEnforcingConfig{enforcementEnabled: false},
		metrics:              metrics.NewNoopMetrics(),
		paginationCache:      kubecache.NewLRUExpireCache(128),
		imageStreamsDisabled: true,
	}

	reg, err := supermiddleware.NewRegistry(ctx, app, inmemory.New(), storage.EnableDelete)
	if err != nil {
		t.Fatal(err)
	}

	// A single-segment (non-namespaced) name must be accepted: getNamespaceName
	// is only reached when imagestreams are enabled.
	named, err := reference.WithName("edge-app")
	if err != nil {
		t.Fatal(err)
	}

	repo, err := reg.Repository(ctx, named)
	if err != nil {
		t.Fatalf("Repository: %s", err)
	}
	if _, err := repo.Manifests(ctx); err != nil {
		t.Fatalf("Manifests: %s", err)
	}
	if repo.Tags(ctx) == nil {
		t.Fatal("Tags returned nil")
	}
	if repo.Blobs(ctx) == nil {
		t.Fatal("Blobs returned nil")
	}
}
