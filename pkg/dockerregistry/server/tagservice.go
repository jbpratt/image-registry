package server

import (
	"context"

	"github.com/distribution/distribution/v3"
	dcontext "github.com/distribution/distribution/v3/context"
	"github.com/distribution/distribution/v3/registry/api/v2"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/metrics"
	"github.com/openshift/image-registry/pkg/imagestream"
)

type tagService struct {
	distribution.TagService

	imageStream imagestream.ImageStream

	// nativeRead serves Get from the embedded storage-backed tag store
	// first, falling back to the imagestream on miss, and writes the
	// storage tag link on Tag so later Gets can resolve natively. All and
	// Lookup always use the imagestream: the storage tag store only knows
	// about tags pushed while native reads were enabled, so a native
	// listing is incomplete by construction.
	nativeRead bool

	// imageStreamsDisabled is set in the ImageStream-less profile (the zero
	// value keeps the default ImageStream-backed behavior). When set, all
	// operations use the embedded native storage tag store only: Get/All/Lookup
	// never consult the (nil) imagestream, Tag writes the native link as the
	// sole record, and Untag deletes it.
	imageStreamsDisabled bool

	metrics metrics.Metrics
}

func (t tagService) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	if t.imageStreamsDisabled {
		return t.TagService.Get(ctx, tag)
	}

	if !t.nativeRead {
		return t.imageStreamGet(ctx, tag)
	}

	desc, err := t.TagService.Get(ctx, tag)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolutionNative, opTagGet)
		return desc, nil
	}
	resolution := nativeMissResolution(ctx, opTagGet, err)

	desc, err = t.imageStreamGet(ctx, tag)
	if err == nil {
		t.metrics.RecordNativeReadResolution(resolution, opTagGet)
	}
	return desc, err
}

func (t tagService) imageStreamGet(ctx context.Context, tag string) (distribution.Descriptor, error) {
	ok, err := t.imageStream.Exists(ctx)
	if err != nil {
		return distribution.Descriptor{}, err
	}
	if !ok {
		// TODO(dmage): keep the not found error from the master API
		return distribution.Descriptor{}, v2.ErrorCodeNameUnknown.WithDetail(nil)
	}

	tags, err := t.imageStream.Tags(ctx)
	if err != nil {
		return distribution.Descriptor{}, err
	}

	dgst, ok := tags[tag]
	if !ok {
		return distribution.Descriptor{}, distribution.ErrTagUnknown{Tag: tag}
	}

	return distribution.Descriptor{Digest: dgst}, nil
}

func (t tagService) All(ctx context.Context) ([]string, error) {
	if t.imageStreamsDisabled {
		return t.TagService.All(ctx)
	}

	ok, err := t.imageStream.Exists(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	tags, err := t.imageStream.Tags(ctx)
	if err != nil {
		return nil, err
	}

	tagList := []string{}
	for tag := range tags {
		tagList = append(tagList, tag)
	}

	return tagList, nil
}

func (t tagService) Lookup(ctx context.Context, desc distribution.Descriptor) ([]string, error) {
	if t.imageStreamsDisabled {
		return t.TagService.Lookup(ctx, desc)
	}

	ok, err := t.imageStream.Exists(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, distribution.ErrRepositoryUnknown{Name: t.imageStream.Reference()}
	}

	tags, err := t.imageStream.Tags(ctx)
	if err != nil {
		return nil, err
	}

	tagList := []string{}
	for tag, dgst := range tags {
		if dgst != desc.Digest {
			continue
		}

		tagList = append(tagList, tag)
	}

	return tagList, nil
}

// Tag records the tag. The imagestream side is already covered by the
// ImageStreamMapping created by the preceding manifest Put; with native
// reads enabled the storage tag link is written as well so that Get can
// resolve the tag without the apiserver.
func (t tagService) Tag(ctx context.Context, tag string, desc distribution.Descriptor) error {
	if t.imageStreamsDisabled {
		// No ImageStreamMapping backs this tag; the native link is the only
		// record, so a write failure must fail the push.
		return t.TagService.Tag(ctx, tag, desc)
	}

	if t.nativeRead {
		if err := t.TagService.Tag(ctx, tag, desc); err != nil {
			// The ImageStreamMapping from the preceding manifest Put already
			// recorded the tag and is authoritative; a failed native link
			// write must not fail the push (reads fall back to the
			// imagestream). Record it as a storage error, not migration debt.
			dcontext.GetLogger(ctx).Warnf("native read tag write: storage error writing tag link for %q, imagestream remains authoritative: %v", tag, err)
			t.metrics.RecordNativeReadResolution(resolutionNativeError, opTagGet)
		}
	}
	return nil
}

func (t tagService) Untag(ctx context.Context, tag string) error {
	if t.imageStreamsDisabled {
		// There is no etcd copy; delete the native tag link so the tag stops
		// resolving (with imagestreams, Untag is a no-op because the
		// ImageStream is authoritative and updated out of band).
		return t.TagService.Untag(ctx, tag)
	}
	return nil
}
