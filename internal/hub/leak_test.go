package hub

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
//
// Live runs (CELLCAST_LIVE=1) talk to a real API server through client-go,
// which caches its transports process-wide and never closes their HTTP/2
// connections. Their read loops are ignored there, and only there.
func TestMain(m *testing.M) {
	var opts []goleak.Option
	if os.Getenv("CELLCAST_LIVE") == "1" {
		opts = append(opts, goleak.IgnoreAnyFunction("golang.org/x/net/http2.(*ClientConn).readLoop"))
	}
	goleak.VerifyTestMain(m, opts...)
}
