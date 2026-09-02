package server

import (
	"context"
	"net/http"
	"testing"

	dcontext "github.com/distribution/distribution/v3/context"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/audit"
)

func withAppMiddleware(parent context.Context, am appMiddleware) context.Context {
	return context.WithValue(parent, appMiddlewareKey, am)
}

func TestWithAuthContextFallsThrough(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://registry/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}

	authCtx := WithUserInfoLogger(context.Background(), "alice", "42")
	authCtx = audit.WithLogger(authCtx, audit.GetLogger(authCtx))
	storeAuthContext(req, authCtx)

	// The handler context carries only the request, as distribution provides it.
	ctx := withAuthContext(dcontext.WithRequest(context.Background(), req))

	if !audit.LoggerExists(ctx) {
		t.Error("expected the audit logger to be visible through the auth context")
	}
	if got := ctx.Value(audit.AuditUserEntry); got != "alice" {
		t.Errorf("expected user entry %q, got %#v", "alice", got)
	}
}

func TestWithAuthContextNoStoredContext(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://registry/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := withAuthContext(dcontext.WithRequest(context.Background(), req))

	if audit.LoggerExists(ctx) {
		t.Error("expected no audit logger without a stored auth context")
	}
	if got := ctx.Value(audit.AuditUserEntry); got != nil {
		t.Errorf("expected nil user entry, got %#v", got)
	}
}
