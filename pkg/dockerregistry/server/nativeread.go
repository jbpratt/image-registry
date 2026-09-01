package server

import (
	"context"
	"regexp"

	"github.com/distribution/distribution/v3"
	dcontext "github.com/distribution/distribution/v3/context"
	"github.com/opencontainers/go-digest"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/metrics"
)

const (
	resolutionNative   = "native"
	resolutionFallback = "fallback"

	opTagGet        = "tag_get"
	opTagAll        = "tag_all"
	opManifestGet   = "manifest_get"
	opManifestExist = "manifest_exists"
	opBlobStat      = "blob_stat"
)

// nativeReadMatcher decides whether a repository should use the native read path.
type nativeReadMatcher struct {
	regex *regexp.Regexp
}

func newNativeReadMatcher(pattern string) (*nativeReadMatcher, error) {
	if pattern == "" {
		return &nativeReadMatcher{}, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	return &nativeReadMatcher{regex: re}, nil
}

func (m *nativeReadMatcher) matches(repoName string) bool {
	if m.regex == nil {
		return true
	}
	return m.regex.MatchString(repoName)
}

// nativeTagService tries the upstream (storage-backed) tag service first,
// falling back to the imagestream tag service on miss.
type nativeTagService struct {
	native   distribution.TagService
	fallback distribution.TagService
	metrics  metrics.Metrics
}

func (t *nativeTagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	desc, err := t.native.Get(ctx, tag)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolutionNative, opTagGet)
		return desc, nil
	}
	dcontext.GetLogger(ctx).Debugf("nativeTagService.Get: native miss for tag %q, falling back to imagestream: %v", tag, err)
	desc, err = t.fallback.Get(ctx, tag)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolutionFallback, opTagGet)
	}
	return desc, err
}

func (t *nativeTagService) All(ctx context.Context) ([]string, error) {
	tags, err := t.native.All(ctx)
	if err == nil && len(tags) > 0 {
		t.metrics.RecordNativeReadResolution(resolutionNative, opTagAll)
		return tags, nil
	}
	dcontext.GetLogger(ctx).Debugf("nativeTagService.All: native returned %d tags (err=%v), falling back to imagestream", len(tags), err)
	tags, err = t.fallback.All(ctx)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolutionFallback, opTagAll)
	}
	return tags, err
}

func (t *nativeTagService) Lookup(ctx context.Context, desc distribution.Descriptor) ([]string, error) {
	return t.fallback.Lookup(ctx, desc)
}

func (t *nativeTagService) Tag(ctx context.Context, tag string, desc distribution.Descriptor) error {
	return t.fallback.Tag(ctx, tag, desc)
}

func (t *nativeTagService) Untag(ctx context.Context, tag string) error {
	return t.fallback.Untag(ctx, tag)
}

// nativeManifestService tries the upstream (storage-backed) manifest service first
// for reads, falling back to the imagestream manifest service on miss.
// Writes always go through the fallback (imagestream) path.
type nativeManifestService struct {
	native   distribution.ManifestService
	fallback distribution.ManifestService
	metrics  metrics.Metrics
}

func (m *nativeManifestService) Exists(ctx context.Context, dgst digest.Digest) (bool, error) {
	exists, err := m.native.Exists(ctx, dgst)
	if err == nil && exists {
		m.metrics.RecordNativeReadResolution(resolutionNative, opManifestExist)
		return true, nil
	}
	dcontext.GetLogger(ctx).Debugf("nativeManifestService.Exists: native miss for %s, falling back to imagestream: %v", dgst, err)
	exists, err = m.fallback.Exists(ctx, dgst)
	if err == nil && exists {
		m.metrics.RecordNativeReadResolution(resolutionFallback, opManifestExist)
	}
	return exists, err
}

func (m *nativeManifestService) Get(ctx context.Context, dgst digest.Digest, options ...distribution.ManifestServiceOption) (distribution.Manifest, error) {
	manifest, err := m.native.Get(ctx, dgst, options...)
	if err == nil {
		m.metrics.RecordNativeReadResolution(resolutionNative, opManifestGet)
		return manifest, nil
	}
	dcontext.GetLogger(ctx).Debugf("nativeManifestService.Get: native miss for %s, falling back to imagestream: %v", dgst, err)
	manifest, err = m.fallback.Get(ctx, dgst, options...)
	if err == nil {
		m.metrics.RecordNativeReadResolution(resolutionFallback, opManifestGet)
	}
	return manifest, err
}

func (m *nativeManifestService) Put(ctx context.Context, manifest distribution.Manifest, options ...distribution.ManifestServiceOption) (digest.Digest, error) {
	return m.fallback.Put(ctx, manifest, options...)
}

func (m *nativeManifestService) Delete(ctx context.Context, dgst digest.Digest) error {
	return m.fallback.Delete(ctx, dgst)
}

// nativeBlobDescriptorService tries the native layer link stat first,
// falling back to the imagestream-backed stat on miss.
type nativeBlobDescriptorService struct {
	native   distribution.BlobDescriptorService
	fallback distribution.BlobDescriptorService
	metrics  metrics.Metrics
}

func (bs *nativeBlobDescriptorService) Stat(ctx context.Context, dgst digest.Digest) (distribution.Descriptor, error) {
	desc, err := bs.native.Stat(ctx, dgst)
	if err == nil {
		bs.metrics.RecordNativeReadResolution(resolutionNative, opBlobStat)
		return desc, nil
	}
	dcontext.GetLogger(ctx).Debugf("nativeBlobDescriptorService.Stat: native miss for %s, falling back: %v", dgst, err)
	desc, err = bs.fallback.Stat(ctx, dgst)
	if err == nil {
		bs.metrics.RecordNativeReadResolution(resolutionFallback, opBlobStat)
	}
	return desc, err
}

func (bs *nativeBlobDescriptorService) SetDescriptor(ctx context.Context, dgst digest.Digest, desc distribution.Descriptor) error {
	return bs.fallback.SetDescriptor(ctx, dgst, desc)
}

func (bs *nativeBlobDescriptorService) Clear(ctx context.Context, dgst digest.Digest) error {
	return bs.fallback.Clear(ctx, dgst)
}
