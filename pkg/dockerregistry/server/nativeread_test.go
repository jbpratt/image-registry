package server

import (
	"context"
	"errors"
	"testing"

	"github.com/distribution/distribution/v3"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/opencontainers/go-digest"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/metrics"
	metricstesting "github.com/openshift/image-registry/pkg/dockerregistry/server/metrics/testing"
	"github.com/openshift/image-registry/pkg/testutil"
	"github.com/openshift/image-registry/pkg/testutil/counter"
)

func TestNativeReadMatcher(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		repo    string
		want    bool
		wantErr bool
	}{
		{pattern: "", repo: "ns/app", want: true},
		{pattern: "ns/app", repo: "ns/app", want: true},
		{pattern: "ns/app", repo: "ns/app-fork", want: false},
		{pattern: "ns/app", repo: "other-ns/app", want: false},
		{pattern: "ns/.*", repo: "ns/anything", want: true},
		{pattern: "ns/.*", repo: "other/anything", want: false},
		{pattern: "a|b", repo: "a", want: true},
		{pattern: "a|b", repo: "ab", want: false},
		{pattern: "(", wantErr: true},
	} {
		m, err := newNativeReadMatcher(tc.pattern)
		if tc.wantErr {
			if err == nil {
				t.Errorf("pattern %q: expected error", tc.pattern)
			}
			continue
		}
		if err != nil {
			t.Fatalf("pattern %q: %v", tc.pattern, err)
		}
		if got := m.matches(tc.repo); got != tc.want {
			t.Errorf("pattern %q repo %q: got %v, want %v", tc.pattern, tc.repo, got, tc.want)
		}
	}
}

func TestIsNativeMiss(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{distribution.ErrBlobUnknown, true},
		{distribution.ErrTagUnknown{Tag: "x"}, true},
		{distribution.ErrManifestUnknown{Name: "n", Tag: "t"}, true},
		{distribution.ErrManifestUnknownRevision{Name: "n", Revision: "sha256:0"}, true},
		{distribution.ErrRepositoryUnknown{Name: "n"}, true},
		{storagedriver.PathNotFoundError{Path: "/p"}, true},
		{errors.New("s3: timeout"), false},
		{distribution.ErrAccessDenied, false},
	} {
		if got := isNativeMiss(tc.err); got != tc.want {
			t.Errorf("%T %v: got %v, want %v", tc.err, tc.err, got, tc.want)
		}
	}
}

// failingTagService returns err from Get. Other methods are not implemented.
type failingTagService struct {
	distribution.TagService
	err error
}

func (f failingTagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	return distribution.Descriptor{}, f.err
}

func TestNativeTagServiceGet(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)
	nativeDesc := distribution.Descriptor{Digest: "sha256:native"}
	fallbackDesc := distribution.Descriptor{Digest: "sha256:fallback"}
	storageErr := errors.New("s3: timeout")

	for _, tc := range []struct {
		name        string
		native      distribution.TagService
		fallback    distribution.TagService
		wantDigest  digest.Digest
		wantErr     error
		wantMetrics counter.M
	}{
		{
			name:        "native hit",
			native:      newTestTagService(map[string]distribution.Descriptor{"latest": nativeDesc}),
			fallback:    newTestTagService(map[string]distribution.Descriptor{"latest": fallbackDesc}),
			wantDigest:  nativeDesc.Digest,
			wantMetrics: counter.M{"native_read_resolutions:native:tag_get": 1},
		},
		{
			name:        "native miss, fallback hit",
			native:      newTestTagService(nil),
			fallback:    newTestTagService(map[string]distribution.Descriptor{"latest": fallbackDesc}),
			wantDigest:  fallbackDesc.Digest,
			wantMetrics: counter.M{"native_read_resolutions:fallback:tag_get": 1},
		},
		{
			name:        "native miss, fallback miss",
			native:      newTestTagService(nil),
			fallback:    newTestTagService(nil),
			wantErr:     distribution.ErrTagUnknown{Tag: "latest"},
			wantMetrics: counter.M{},
		},
		{
			name:        "native storage error, fallback hit",
			native:      failingTagService{err: storageErr},
			fallback:    newTestTagService(map[string]distribution.Descriptor{"latest": fallbackDesc}),
			wantDigest:  fallbackDesc.Digest,
			wantMetrics: counter.M{"native_read_resolutions:native_error:tag_get": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := metricstesting.NewCounterSink()
			ts := &nativeTagService{native: tc.native, fallback: tc.fallback, metrics: metrics.NewMetrics(sink)}

			desc, err := ts.Get(ctx, "latest")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got err %v, want %v", err, tc.wantErr)
			}
			if desc.Digest != tc.wantDigest {
				t.Errorf("got digest %q, want %q", desc.Digest, tc.wantDigest)
			}
			if diff := c.Diff(tc.wantMetrics); diff != nil {
				t.Errorf("unexpected metrics: %v", diff)
			}
		})
	}
}

func TestNativeTagServiceWritesAndListing(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)
	desc := distribution.Descriptor{Digest: "sha256:abc"}
	native := newTestTagService(nil)
	fallback := newTestTagService(map[string]distribution.Descriptor{"etcd-only": desc})
	_, sink := metricstesting.NewCounterSink()
	ts := &nativeTagService{native: native, fallback: fallback, metrics: metrics.NewMetrics(sink)}

	if err := ts.Tag(ctx, "latest", desc); err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if native.calls["Tag"] != 1 || fallback.calls["Tag"] != 1 {
		t.Errorf("Tag should dual-write: native=%d fallback=%d", native.calls["Tag"], fallback.calls["Tag"])
	}
	if got, err := native.Get(ctx, "latest"); err != nil || got.Digest != desc.Digest {
		t.Errorf("native tag link not written: %v %v", got, err)
	}

	// Listing must come from the imagestream: the native store never sees
	// tags created via oc tag / imports.
	tags, err := ts.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(tags) != 2 || native.calls["All"] != 0 {
		t.Errorf("All should list from fallback only, got %v (native calls=%d)", tags, native.calls["All"])
	}
	if _, err := ts.Lookup(ctx, desc); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if native.calls["Lookup"] != 0 || fallback.calls["Lookup"] != 1 {
		t.Errorf("Lookup should use fallback only: native=%d fallback=%d", native.calls["Lookup"], fallback.calls["Lookup"])
	}
	if err := ts.Untag(ctx, "latest"); err != nil {
		t.Fatalf("Untag: %v", err)
	}
	if native.calls["Untag"] != 0 || fallback.calls["Untag"] != 1 {
		t.Errorf("Untag should use fallback only: native=%d fallback=%d", native.calls["Untag"], fallback.calls["Untag"])
	}
}

// failingManifestService returns err from Get and Exists.
type failingManifestService struct {
	distribution.ManifestService
	err error
}

func (f failingManifestService) Exists(ctx context.Context, dgst digest.Digest) (bool, error) {
	return false, f.err
}

func (f failingManifestService) Get(ctx context.Context, dgst digest.Digest, options ...distribution.ManifestServiceOption) (distribution.Manifest, error) {
	return nil, f.err
}

func makeTestManifest(t *testing.T) (distribution.Manifest, digest.Digest) {
	t.Helper()
	_, cfgDesc, err := testutil.MakeManifestConfig()
	if err != nil {
		t.Fatal(err)
	}
	_, layerDesc, err := testutil.MakeRandomLayer()
	if err != nil {
		t.Fatal(err)
	}
	m, err := testutil.MakeSchema2Manifest(cfgDesc, []distribution.Descriptor{layerDesc})
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := m.Payload()
	if err != nil {
		t.Fatal(err)
	}
	return m, digest.FromBytes(payload)
}

func TestNativeManifestServiceGet(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)
	manifest, dgst := makeTestManifest(t)
	storageErr := errors.New("s3: timeout")
	withManifest := func() *testManifestService {
		return newTestManifestService("ns/app", map[digest.Digest]distribution.Manifest{dgst: manifest})
	}

	for _, tc := range []struct {
		name        string
		native      distribution.ManifestService
		fallback    distribution.ManifestService
		wantFound   bool
		wantMetrics counter.M
	}{
		{
			name:        "native hit",
			native:      withManifest(),
			fallback:    newTestManifestService("ns/app", nil),
			wantFound:   true,
			wantMetrics: counter.M{"native_read_resolutions:native:manifest_get": 1},
		},
		{
			name:        "native miss, fallback hit",
			native:      newTestManifestService("ns/app", nil),
			fallback:    withManifest(),
			wantFound:   true,
			wantMetrics: counter.M{"native_read_resolutions:fallback:manifest_get": 1},
		},
		{
			name:        "native miss, fallback miss",
			native:      newTestManifestService("ns/app", nil),
			fallback:    newTestManifestService("ns/app", nil),
			wantMetrics: counter.M{},
		},
		{
			name:        "native storage error, fallback hit",
			native:      failingManifestService{err: storageErr},
			fallback:    withManifest(),
			wantFound:   true,
			wantMetrics: counter.M{"native_read_resolutions:native_error:manifest_get": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := metricstesting.NewCounterSink()
			ms := &nativeManifestService{native: tc.native, fallback: tc.fallback, metrics: metrics.NewMetrics(sink)}

			got, err := ms.Get(ctx, dgst)
			if tc.wantFound {
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				testutil.AssertManifestsEqual(t, tc.name, got, manifest)
			} else {
				var unknown distribution.ErrManifestUnknownRevision
				if !errors.As(err, &unknown) {
					t.Fatalf("got err %v, want ErrManifestUnknownRevision", err)
				}
			}
			if diff := c.Diff(tc.wantMetrics); diff != nil {
				t.Errorf("unexpected metrics: %v", diff)
			}
		})
	}
}

func TestNativeManifestServiceExists(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)
	manifest, dgst := makeTestManifest(t)
	storageErr := errors.New("s3: timeout")
	withManifest := func() *testManifestService {
		return newTestManifestService("ns/app", map[digest.Digest]distribution.Manifest{dgst: manifest})
	}

	for _, tc := range []struct {
		name        string
		native      distribution.ManifestService
		fallback    distribution.ManifestService
		wantExists  bool
		wantErr     error
		wantMetrics counter.M
	}{
		{
			name:        "native hit",
			native:      withManifest(),
			fallback:    newTestManifestService("ns/app", nil),
			wantExists:  true,
			wantMetrics: counter.M{"native_read_resolutions:native:manifest_exists": 1},
		},
		{
			name:        "native false, fallback true",
			native:      newTestManifestService("ns/app", nil),
			fallback:    withManifest(),
			wantExists:  true,
			wantMetrics: counter.M{"native_read_resolutions:fallback:manifest_exists": 1},
		},
		{
			name:        "native false, fallback false",
			native:      newTestManifestService("ns/app", nil),
			fallback:    newTestManifestService("ns/app", nil),
			wantMetrics: counter.M{},
		},
		{
			name:        "native storage error, fallback true",
			native:      failingManifestService{err: storageErr},
			fallback:    withManifest(),
			wantExists:  true,
			wantMetrics: counter.M{"native_read_resolutions:native_error:manifest_exists": 1},
		},
		{
			name:        "native storage error, fallback error",
			native:      failingManifestService{err: storageErr},
			fallback:    failingManifestService{err: distribution.ErrBlobUnknown},
			wantErr:     distribution.ErrBlobUnknown,
			wantMetrics: counter.M{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := metricstesting.NewCounterSink()
			ms := &nativeManifestService{native: tc.native, fallback: tc.fallback, metrics: metrics.NewMetrics(sink)}

			exists, err := ms.Exists(ctx, dgst)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got err %v, want %v", err, tc.wantErr)
			}
			if exists != tc.wantExists {
				t.Errorf("got exists=%v, want %v", exists, tc.wantExists)
			}
			if diff := c.Diff(tc.wantMetrics); diff != nil {
				t.Errorf("unexpected metrics: %v", diff)
			}
		})
	}
}

func TestNativeManifestServiceWritesUseFallback(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)
	manifest, dgst := makeTestManifest(t)
	native := newTestManifestService("ns/app", nil)
	fallback := newTestManifestService("ns/app", nil)
	_, sink := metricstesting.NewCounterSink()
	ms := &nativeManifestService{native: native, fallback: fallback, metrics: metrics.NewMetrics(sink)}

	if _, err := ms.Put(ctx, manifest); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if native.calls["Put"] != 0 || fallback.calls["Put"] != 1 {
		t.Errorf("Put should use fallback only: native=%d fallback=%d", native.calls["Put"], fallback.calls["Put"])
	}
	_ = ms.Delete(ctx, dgst)
	if native.calls["Delete"] != 0 || fallback.calls["Delete"] != 1 {
		t.Errorf("Delete should use fallback only: native=%d fallback=%d", native.calls["Delete"], fallback.calls["Delete"])
	}
}
