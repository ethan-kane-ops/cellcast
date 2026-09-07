package hub

import (
	"context"
	"errors"
	"net/http"
)

// ErrUnauthenticated is returned when a caller cannot be identified. It is the
// only outcome the default authenticator ever produces.
var ErrUnauthenticated = errors.New("caller could not be authenticated")

// Identity is an authenticated caller.
//
// It is the output of authentication and the input to authorization. The claims
// carried here are what a PlacementPolicy matches against; nothing downstream
// may consult the raw token.
type Identity struct {
	// Issuer is the OIDC issuer that vouched for this caller.
	Issuer string
	// Subject is the token's `sub` claim.
	Subject string
	// Claims are the provider-specific claims extracted from the token, for
	// example `repository` and `ref` for GitHub Actions.
	Claims map[string]string
}

// Authenticator resolves a caller identity from an inbound request.
//
// This is the seam ENG-172 implements. It exists now, with a fail-closed
// default, so that the OIDC work drops into a defined place rather than
// rewriting the server, and so that no intermediate state of this repository
// serves an unauthenticated placement API.
type Authenticator interface {
	// Authenticate returns the caller's identity, or an error. An error must
	// never be interpreted as anonymous access.
	Authenticate(ctx context.Context, r *http.Request) (*Identity, error)
}

// denyAll rejects every caller.
//
// This is the default deliberately. An unauthenticated API that mints scoped
// cluster credentials is a privilege escalation service, so the failure mode of
// "nobody configured an authenticator" must be refusal, not anonymous access.
type denyAll struct{}

func (denyAll) Authenticate(context.Context, *http.Request) (*Identity, error) {
	return nil, ErrUnauthenticated
}

type identityContextKey struct{}

// withIdentity returns a context carrying the authenticated caller.
func withIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// IdentityFrom returns the authenticated caller carried by ctx, if any.
func IdentityFrom(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(*Identity)
	return id, ok
}
