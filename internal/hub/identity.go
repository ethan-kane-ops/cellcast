package hub

import (
	"context"
	"net/http"

	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// Authenticator resolves a caller identity from an inbound request.
//
// The seam exists so that authentication has one defined place, and so that no
// state of this repository serves an unauthenticated placement
// API.
type Authenticator interface {
	// Authenticate returns the caller's identity, or an error. An error must
	// never be interpreted as anonymous access.
	Authenticate(ctx context.Context, r *http.Request) (*identity.Identity, error)
}

// denyAll rejects every caller.
//
// This is the default. An unauthenticated API that mints scoped
// cluster credentials is a privilege escalation service, so the failure mode of
// "nobody configured an authenticator" must be refusal, not anonymous access.
type denyAll struct{}

func (denyAll) Authenticate(context.Context, *http.Request) (*identity.Identity, error) {
	return nil, noAuthenticator{}
}

// noAuthenticator is the refusal a hub with no configured authenticator
// produces.
//
// It names itself so that "nobody configured OIDC" is its own bucket in the
// rejection metric. Counted as Unclassified alongside malformed tokens, the one
// misconfiguration that stops every deploy in the estate would look like a
// pipeline problem.
type noAuthenticator struct{}

func (noAuthenticator) Error() string           { return identity.ErrUnauthenticated.Error() }
func (noAuthenticator) RejectionReason() string { return "NoAuthenticator" }
func (noAuthenticator) Unwrap() error           { return identity.ErrUnauthenticated }

type identityContextKey struct{}

// withIdentity returns a context carrying the authenticated caller.
func withIdentity(ctx context.Context, id *identity.Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// IdentityFrom returns the authenticated caller carried by ctx, if any.
func IdentityFrom(ctx context.Context) (*identity.Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(*identity.Identity)
	return id, ok
}
