package cli

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
