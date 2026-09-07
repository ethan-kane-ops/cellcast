// Package identity describes an authenticated caller.
//
// It is a leaf: it imports nothing from the rest of cellcast, so the packages
// that authenticate a caller (oidc), the package that decides what that caller
// may reach (placement), and the HTTP layer that carries it between them can
// all depend on this without depending on each other.
//
// It was previously part of the hub package, which meant the placement engine
// imported the HTTP server in order to name its own input. That is backwards,
// and it made the placement route impossible to mount without a cycle.
package identity

import "errors"

// ErrUnauthenticated is returned when a caller cannot be identified. It is the
// only outcome the hub's default authenticator ever produces.
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
