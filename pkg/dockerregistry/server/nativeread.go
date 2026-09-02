package server

import (
	"context"
	"errors"
	"regexp"

	"github.com/distribution/distribution/v3"
	dcontext "github.com/distribution/distribution/v3/context"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/opencontainers/go-digest"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/metrics"
)

// Resolution label values for the native read metric.
//
//   - native: served from distribution's storage layout, no apiserver call.
//   - fallback: native lookup missed (content absent from storage), served
//     via the imagestream path.
//   - native_error: native lookup failed for a reason other than a miss
//     (storage outage, permission, I/O); served via the imagestream path.
//     A rising native_error rate means storage is degraded, not that the
//     content lives only in etcd.
const (
	resolutionNative      = "native"
	resolutionFallback    = "fallback"
	resolutionNativeError = "native_error"

	opTagGet        = "tag_get"
	opManifestGet   = "manifest_get"
	opManifestExist = "manifest_exists"
	opBlobStat      = "blob_stat"
)

// nativeReadMatcher decides whether a repository should use the native read path.
type nativeReadMatcher struct {
	regex *regexp.Regexp
}

// newNativeReadMatcher compiles pattern as a full-string match against the
// repository name (namespace/name). An empty pattern matches every repository.
func newNativeReadMatcher(pattern string) (*nativeReadMatcher, error) {
	if pattern == "" {
		return &nativeReadMatcher{}, nil
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
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

// isNativeMiss reports whether err from the storage-backed services means
// "not present in storage" as opposed to a storage failure.
func isNativeMiss(err error) bool {
	var (
		tagUnknown      distribution.ErrTagUnknown
		manifestUnknown distribution.ErrManifestUnknown
		revisionUnknown distribution.ErrManifestUnknownRevision
		repoUnknown     distribution.ErrRepositoryUnknown
		pathNotFound    storagedriver.PathNotFoundError
	)
	switch {
	case errors.Is(err, distribution.ErrBlobUnknown),
		errors.As(err, &tagUnknown),
		errors.As(err, &manifestUnknown),
		errors.As(err, &revisionUnknown),
		errors.As(err, &repoUnknown),
		errors.As(err, &pathNotFound):
		return true
	}
	return false
}

// recordNativeFailure classifies a native lookup failure, logs it, and
// returns the resolution label to record if the fallback then succeeds.
func recordNativeFailure(ctx context.Context, op string, err error) string {
	if isNativeMiss(err) {
		dcontext.GetLogger(ctx).Debugf("native read %s: miss, falling back to imagestream: %v", op, err)
		return resolutionFallback
	}
	dcontext.GetLogger(ctx).Warnf("native read %s: storage error, falling back to imagestream: %v", op, err)
	return resolutionNativeError
}

// nativeTagService serves Get from the storage-backed tag store first and
// falls back to the imagestream tag service on miss.
//
// All and Lookup always use the imagestream: the storage tag store only
// knows about tags pushed through this registry while native reads were
// enabled, so a native listing is incomplete by construction. Neither is on
// the pull hot path.
//
// Tag is dual-written: the imagestream mapping is created by the manifest
// Put that precedes it, and the storage tag link is written here so that
// subsequent Get calls can resolve natively. Untag stays a no-op on both
// sides, matching the existing imagestream behaviour.
type nativeTagService struct {
	native   distribution.TagService
	fallback distribution.TagService
	metrics  metrics.Metrics
}

var _ distribution.TagService = &nativeTagService{}

func (t *nativeTagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	desc, err := t.native.Get(ctx, tag)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolutionNative, opTagGet)
		return desc, nil
	}
	resolution := recordNativeFailure(ctx, opTagGet, err)
	desc, err = t.fallback.Get(ctx, tag)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolution, opTagGet)
	}
	return desc, err
}

func (t *nativeTagService) All(ctx context.Context) ([]string, error) {
	return t.fallback.All(ctx)
}

func (t *nativeTagService) Lookup(ctx context.Context, desc distribution.Descriptor) ([]string, error) {
	return t.fallback.Lookup(ctx, desc)
}

func (t *nativeTagService) Tag(ctx context.Context, tag string, desc distribution.Descriptor) error {
	if err := t.fallback.Tag(ctx, tag, desc); err != nil {
		return err
	}
	return t.native.Tag(ctx, tag, desc)
}

func (t *nativeTagService) Untag(ctx context.Context, tag string) error {
	return t.fallback.Untag(ctx, tag)
}

// nativeManifestService serves Get and Exists from the storage-backed
// manifest store first and falls back to the imagestream manifest service on
// miss. Writes always go through the fallback path.
//
// Known consequence: a manifest that has a revision link in this repository
// is served natively even after every imagestream reference to it has been
// removed (oc tag -d, istag delete) and until pruning removes the link. The
// imagestream path would return 404 in that window. See NATIVE-READ-POC.md.
type nativeManifestService struct {
	native   distribution.ManifestService
	fallback distribution.ManifestService
	metrics  metrics.Metrics
}

var _ distribution.ManifestService = &nativeManifestService{}

func (m *nativeManifestService) Exists(ctx context.Context, dgst digest.Digest) (bool, error) {
	exists, err := m.native.Exists(ctx, dgst)
	if err == nil && exists {
		m.metrics.RecordNativeReadResolution(resolutionNative, opManifestExist)
		return true, nil
	}
	resolution := resolutionFallback
	if err != nil {
		resolution = recordNativeFailure(ctx, opManifestExist, err)
	}
	exists, err = m.fallback.Exists(ctx, dgst)
	if err == nil && exists {
		m.metrics.RecordNativeReadResolution(resolution, opManifestExist)
	}
	return exists, err
}

func (m *nativeManifestService) Get(ctx context.Context, dgst digest.Digest, options ...distribution.ManifestServiceOption) (distribution.Manifest, error) {
	manifest, err := m.native.Get(ctx, dgst, options...)
	if err == nil {
		m.metrics.RecordNativeReadResolution(resolutionNative, opManifestGet)
		return manifest, nil
	}
	resolution := recordNativeFailure(ctx, opManifestGet, err)
	manifest, err = m.fallback.Get(ctx, dgst, options...)
	if err == nil {
		m.metrics.RecordNativeReadResolution(resolution, opManifestGet)
	}
	return manifest, err
}

func (m *nativeManifestService) Put(ctx context.Context, manifest distribution.Manifest, options ...distribution.ManifestServiceOption) (digest.Digest, error) {
	return m.fallback.Put(ctx, manifest, options...)
}

func (m *nativeManifestService) Delete(ctx context.Context, dgst digest.Digest) error {
	return m.fallback.Delete(ctx, dgst)
}
