package server

import (
	"context"
	"errors"
	"regexp"

	"github.com/distribution/distribution/v3"
	dcontext "github.com/distribution/distribution/v3/context"
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

	opTagGet      = "tag_get"
	opManifestGet = "manifest_get"
	opBlobStat    = "blob_stat"
)

// compileNativeReadRegex compiles pattern as a full-string match against the
// repository name (namespace/name). An empty pattern matches every repository.
func compileNativeReadRegex(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		pattern = ".*"
	}
	return regexp.Compile("^(?:" + pattern + ")$")
}

// nativeMissResolution classifies a failed native lookup, logs it, and
// returns the resolution label to record if the imagestream fallback then
// succeeds. Upstream's tag and manifest stores report a missing link as
// ErrTagUnknown and ErrManifestUnknownRevision respectively; anything else
// is a storage failure.
func nativeMissResolution(ctx context.Context, op string, err error) string {
	var (
		tagUnknown      distribution.ErrTagUnknown
		revisionUnknown distribution.ErrManifestUnknownRevision
	)
	if errors.As(err, &tagUnknown) || errors.As(err, &revisionUnknown) {
		dcontext.GetLogger(ctx).Debugf("native read %s: miss, falling back to imagestream: %v", op, err)
		return resolutionFallback
	}
	dcontext.GetLogger(ctx).Warnf("native read %s: storage error, falling back to imagestream: %v", op, err)
	return resolutionNativeError
}
