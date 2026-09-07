package hub

import (
	"context"
	"net/http"

	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// Authenticator resolves a caller identity from an inbound request.
//
// This is the seam ENG-172 implements. It exists so that the OIDC work drops
// into a defined place rather than rewriting the server, and so that no
// intermediate state of this repository serves an unauthenticated placement
// API.
type Authenticator interface {
	// Authenticate returns the caller's identity, or an error. An error must
	// never be interpreted as anonymous access.
	Authenticate(ctx context.Context, r *http.Request) (*identity.Identity, error)
}

// denyAll rejects every caller.
//
// This is the default deliberately. An unauthenticated API that mints scoped
// cluster credentials is a privilege escalation service, so the failure mode of
// "nobody configured an authenticator" must be refusal, not anonymous access.
type denyAll struct{}

func (denyAll) Authenticate(context.Context, *http.Request) (*identity.Identity, error) {
	return nil, identity.ErrUnauthenticated
}

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
