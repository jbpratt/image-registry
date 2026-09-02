package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/registry/api/errcode"
	"github.com/distribution/distribution/v3/registry/api/v2"
	"github.com/opencontainers/go-digest"

	"github.com/openshift/image-registry/pkg/imagestream"

	registryclient "github.com/openshift/image-registry/pkg/dockerregistry/server/client"
	"github.com/openshift/image-registry/pkg/dockerregistry/server/metrics"
	metricstesting "github.com/openshift/image-registry/pkg/dockerregistry/server/metrics/testing"
	"github.com/openshift/image-registry/pkg/testutil"
	"github.com/openshift/image-registry/pkg/testutil/counter"
)

func TestTagGet(t *testing.T) {
	namespace := "user"
	repo := "app"
	tag := "latest"

	backgroundCtx := context.Background()
	backgroundCtx = testutil.WithTestLogger(backgroundCtx, t)

	fos, imageClient := testutil.NewFakeOpenShiftWithClient(backgroundCtx)
	testImage := testutil.AddRandomImage(t, fos, namespace, repo, tag)

	testcases := []struct {
		title                 string
		tagName               string
		tagValue              distribution.Descriptor
		expectedError         bool
		expectedNotFoundError bool
	}{
		{
			title:    "get valid tag from managed image",
			tagName:  tag,
			tagValue: distribution.Descriptor{Digest: digest.Digest(testImage.Name)},
		},
		{
			title:                 "get missing tag",
			tagName:               tag + "-no-found",
			expectedError:         true,
			expectedNotFoundError: true,
		},
	}

	for _, tc := range testcases {
		imageStream := imagestream.New(backgroundCtx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient))

		ts := &tagService{
			TagService:  newTestTagService(nil),
			imageStream: imageStream,
		}

		resultDesc, err := ts.Get(backgroundCtx, tc.tagName)

		switch err.(type) {
		case distribution.ErrTagUnknown:
			if !tc.expectedNotFoundError {
				t.Fatalf("[%s] unexpected error: %#+v", tc.title, err)
			}
		case nil:
			if tc.expectedError || tc.expectedNotFoundError {
				t.Fatalf("[%s] unexpected successful response", tc.title)
			}
		default:
			if tc.expectedError {
				break
			}
			t.Fatalf("[%s] unexpected error: %#+v", tc.title, err)
		}

		if resultDesc.Digest != tc.tagValue.Digest {
			t.Fatalf("[%s] unexpected result returned", tc.title)
		}
	}
}

func TestTagGetWithoutImageStream(t *testing.T) {
	namespace := "user"
	repo := "app"
	tag := "latest"

	ctx := context.Background()
	ctx = testutil.WithTestLogger(ctx, t)

	_, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)

	imageStream := imagestream.New(ctx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient))

	ts := &tagService{
		TagService:  newTestTagService(nil),
		imageStream: imageStream,
	}

	_, err := ts.Get(ctx, tag)
	if err == nil {
		t.Fatalf("error expected")
	}

	e, ok := err.(errcode.Error)
	if !ok || e.Code != v2.ErrorCodeNameUnknown {
		t.Fatalf("unexpected error: %#+v", err)
	}
}

func TestTagGetAll(t *testing.T) {
	namespace := "user"
	repo := "app"
	tag := "latest"

	ctx := context.Background()
	ctx = testutil.WithTestLogger(ctx, t)

	fos, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)
	testutil.AddRandomImage(t, fos, namespace, repo, tag)

	testcases := []struct {
		title         string
		expectResult  []string
		expectedError bool
	}{
		{
			title:        "get all tags",
			expectResult: []string{tag},
		},
	}

	for _, tc := range testcases {
		imageStream := imagestream.New(ctx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient))

		ts := &tagService{
			TagService:  newTestTagService(nil),
			imageStream: imageStream,
		}

		result, err := ts.All(ctx)

		if err != nil && !tc.expectedError {
			t.Fatalf("[%s] unexpected error: %#+v", tc.title, err)
		}

		if !reflect.DeepEqual(result, tc.expectResult) {
			t.Fatalf("[%s] unexpected result: %#+v", tc.title, result)
		}
	}
}

func TestTagGetAllWithoutImageStream(t *testing.T) {
	namespace := "user"
	repo := "app"

	ctx := context.Background()
	ctx = testutil.WithTestLogger(ctx, t)

	_, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)

	imageStream := imagestream.New(ctx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient))

	ts := &tagService{
		TagService:  newTestTagService(nil),
		imageStream: imageStream,
	}

	_, err := ts.All(ctx)
	if err == nil {
		t.Fatalf("error expected")
	}

	_, ok := err.(distribution.ErrRepositoryUnknown)
	if !ok {
		t.Fatalf("unexpected error: %#+v", err)
	}
}

func TestTagLookup(t *testing.T) {
	namespace := "user"
	repo := "app"
	tag := "latest"

	ctx := context.Background()
	ctx = testutil.WithTestLogger(ctx, t)

	fos, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)
	testImage := testutil.AddRandomImage(t, fos, namespace, repo, tag)

	testcases := []struct {
		title         string
		tagValue      distribution.Descriptor
		expectResult  []string
		expectedError bool
	}{
		{
			title:        "lookup tags with pullthrough",
			tagValue:     distribution.Descriptor{Digest: digest.Digest(testImage.Name)},
			expectResult: []string{tag},
		},
		{
			title:        "lookup tags by missing digest",
			tagValue:     distribution.Descriptor{Digest: digest.Digest(etcdDigest)},
			expectResult: []string{},
		},
	}

	for _, tc := range testcases {
		imageStream := imagestream.New(ctx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient))

		ts := &tagService{
			TagService:  newTestTagService(nil),
			imageStream: imageStream,
		}

		result, err := ts.Lookup(ctx, tc.tagValue)

		if err != nil {
			if !tc.expectedError {
				t.Fatalf("[%s] unexpected error: %#+v", tc.title, err)
			}
			continue
		} else {
			if tc.expectedError {
				t.Fatalf("[%s] error expected", tc.title)
			}
		}

		if !reflect.DeepEqual(result, tc.expectResult) {
			t.Fatalf("[%s] unexpected result: %#+v", tc.title, result)
		}
	}
}

func TestTagLookupWithoutImageStream(t *testing.T) {
	namespace := "user"
	repo := "app"
	tag := "latest"

	ctx := context.Background()
	ctx = testutil.WithTestLogger(ctx, t)

	fos, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)
	anotherImage := testutil.AddRandomImage(t, fos, namespace, repo+"-another", tag)

	imageStream := imagestream.New(ctx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient))

	ts := &tagService{
		TagService:  newTestTagService(nil),
		imageStream: imageStream,
	}

	_, err := ts.Lookup(ctx, distribution.Descriptor{
		Digest: digest.Digest(anotherImage.Name),
	})
	if err == nil {
		t.Fatalf("error expected")
	}

	_, ok := err.(distribution.ErrRepositoryUnknown)
	if !ok {
		t.Fatalf("unexpected error: %#+v", err)
	}
}

type testTagService struct {
	data  map[string]distribution.Descriptor
	calls map[string]int
}

func newTestTagService(data map[string]distribution.Descriptor) *testTagService {
	b := make(map[string]distribution.Descriptor)
	for d, content := range data {
		b[d] = content
	}
	return &testTagService{
		data:  b,
		calls: make(map[string]int),
	}
}

func (t *testTagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	t.calls["Get"]++
	desc, exists := t.data[tag]
	if !exists {
		return distribution.Descriptor{}, distribution.ErrTagUnknown{Tag: tag}
	}
	return desc, nil
}

func (t *testTagService) Tag(ctx context.Context, tag string, desc distribution.Descriptor) error {
	t.calls["Tag"]++
	t.data[tag] = desc
	return nil
}

func (t *testTagService) Untag(ctx context.Context, tag string) error {
	t.calls["Untag"]++
	_, exists := t.data[tag]
	if !exists {
		return distribution.ErrTagUnknown{Tag: tag}
	}
	delete(t.data, tag)
	return nil
}

func (t *testTagService) All(ctx context.Context) (tags []string, err error) {
	t.calls["All"]++
	for tag := range t.data {
		tags = append(tags, tag)
	}
	return
}

func (t *testTagService) List(ctx context.Context, limit int, last string) (tags []string, err error) {
	t.calls["List"]++
	for tag := range t.data {
		tags = append(tags, tag)
	}
	return
}

func (t *testTagService) Lookup(ctx context.Context, desc distribution.Descriptor) (tags []string, err error) {
	t.calls["Lookup"]++
	for tag := range t.data {
		if t.data[tag].Digest == desc.Digest {
			tags = append(tags, tag)
		}
	}
	return
}

type failingTagService struct {
	distribution.TagService
	err error
}

func (f failingTagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	return distribution.Descriptor{}, f.err
}

func TestTagGetNativeRead(t *testing.T) {
	ctx := testutil.WithTestLogger(context.Background(), t)
	namespace, repo, tag := "user", "app", "latest"

	fos, imageClient := testutil.NewFakeOpenShiftWithClient(ctx)
	testImage := testutil.AddRandomImage(t, fos, namespace, repo, tag)
	imageStreamDigest := digest.Digest(testImage.Name)
	nativeDesc := distribution.Descriptor{Digest: "sha256:native"}
	storageErr := errors.New("s3: timeout")

	for _, tc := range []struct {
		name        string
		native      distribution.TagService
		tagName     string
		wantDigest  digest.Digest
		wantErr     error
		wantMetrics counter.M
	}{
		{
			name:        "native hit",
			native:      newTestTagService(map[string]distribution.Descriptor{tag: nativeDesc}),
			tagName:     tag,
			wantDigest:  nativeDesc.Digest,
			wantMetrics: counter.M{"native_read_resolutions:native:tag_get": 1},
		},
		{
			name:        "native miss, imagestream hit",
			native:      newTestTagService(nil),
			tagName:     tag,
			wantDigest:  imageStreamDigest,
			wantMetrics: counter.M{"native_read_resolutions:fallback:tag_get": 1},
		},
		{
			name:        "native miss, imagestream miss",
			native:      newTestTagService(nil),
			tagName:     "missing",
			wantErr:     distribution.ErrTagUnknown{Tag: "missing"},
			wantMetrics: counter.M{},
		},
		{
			name:        "native storage error, imagestream hit",
			native:      failingTagService{err: storageErr},
			tagName:     tag,
			wantDigest:  imageStreamDigest,
			wantMetrics: counter.M{"native_read_resolutions:native_error:tag_get": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := metricstesting.NewCounterSink()
			ts := tagService{
				TagService:  tc.native,
				imageStream: imagestream.New(ctx, namespace, repo, registryclient.NewFakeRegistryAPIClient(nil, imageClient)),
				nativeRead:  true,
				metrics:     metrics.NewMetrics(sink),
			}

			desc, err := ts.Get(ctx, tc.tagName)
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

func TestTagNativeReadWritesLink(t *testing.T) {
	ctx := context.Background()
	desc := distribution.Descriptor{Digest: "sha256:abc"}

	for _, nativeRead := range []bool{false, true} {
		store := newTestTagService(nil)
		ts := tagService{TagService: store, nativeRead: nativeRead}

		if err := ts.Tag(ctx, "latest", desc); err != nil {
			t.Fatalf("nativeRead=%v: unexpected error: %v", nativeRead, err)
		}
		if _, linked := store.data["latest"]; linked != nativeRead {
			t.Errorf("nativeRead=%v: storage tag link written=%v", nativeRead, linked)
		}
	}
}
