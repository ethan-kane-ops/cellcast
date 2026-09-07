package hub

import (
	"context"
	"time"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
)

// Minter issues a short-lived credential for a cell that placement has already
// chosen.
//
// The seam exists so the placement route (ENG-114) can be tested without a
// spoke cluster behind it, and so that the only implementation, the broker, is
// reachable from exactly one place.
//
// There is deliberately no route that mints without placing first. A standalone
// mint endpoint would let a caller name its own cell and skip the permission
// filter entirely, which is the confused-deputy path the placement engine
// exists to close (docs/threat-model.md T-03).
type Minter interface {
	Mint(
		ctx context.Context,
		cluster *cellcastv1alpha1.Cluster,
		ttlPolicy *cellcastv1alpha1.TokenTTLPolicy,
		requested time.Duration,
		subject string,
	) (*broker.Credential, broker.Resolution, error)
}

// WithMinter supplies the credential broker.
//
// Absent, the placement route reports that minting is unavailable rather than
// returning a placement with no credential. A caller that receives a cell name
// and no way to reach it has been given something worse than an error: it looks
// like success.
func WithMinter(m Minter) Option {
	return func(s *Server) { s.minter = m }
}
