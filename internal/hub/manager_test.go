package hub

import (
	"strings"
	"testing"
)

func TestTheManagerElectsForTheControllersOnly(t *testing.T) {
	// The lease covers the reconcilers, which write. It must not gate the
	// cache, because placement reads from it on every request and a follower
	// that stopped syncing would answer from a fleet frozen at the moment it
	// lost the election.
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	opts := ManagerOptions{
		Namespace:               "cellcast-system",
		MetricsAddr:             ":8082",
		LeaderElection:          true,
		LeaderElectionNamespace: "cellcast-system",
	}

	got := opts.controllerOptions(scheme)

	if !got.LeaderElection {
		t.Error("LeaderElection = false, want it carried through")
	}
	// Without this a rolling update leaves the fleet with no reconciler for a
	// full lease duration per pod, because the outgoing leader never says so.
	if !got.LeaderElectionReleaseOnCancel {
		t.Error("LeaderElectionReleaseOnCancel = false, want the lease handed back on a clean stop")
	}
	if got.LeaderElectionID != leaderElectionID {
		t.Errorf("LeaderElectionID = %q, want %q", got.LeaderElectionID, leaderElectionID)
	}
	if _, ok := got.Cache.DefaultNamespaces[opts.Namespace]; !ok {
		t.Errorf("cache namespaces = %v, want the hub's own namespace scoped in", got.Cache.DefaultNamespaces)
	}
	if got.Metrics.BindAddress != opts.MetricsAddr {
		t.Errorf("metrics bind = %q, want %q", got.Metrics.BindAddress, opts.MetricsAddr)
	}
}

func TestTheShippedHubElectsALeader(t *testing.T) {
	// Read off the binary's own help rather than the struct, because the
	// default that matters is the one an operator gets by running it. The
	// chart runs more than one replica, and three replicas each writing
	// Cluster status and each emitting the same Event is worse than one.
	out := hubHelp(t)

	if !strings.Contains(out, "--leader-election") {
		t.Fatalf("--leader-election is not in the help output:\n%s", out)
	}
	line := flagLine(t, out, "leader-election")
	if !strings.Contains(line, "(default true)") {
		t.Errorf("--leader-election line is %q, want it to default to true", line)
	}
}

// flagLine returns the help text for one flag, joined onto a single line.
func flagLine(t *testing.T, help, name string) string {
	t.Helper()
	var out []string
	var capturing bool
	for _, raw := range strings.Split(help, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "--"+name+" ") || line == "--"+name:
			capturing = true
			out = append(out, line)
		case capturing && strings.HasPrefix(line, "--"):
			return strings.Join(out, " ")
		case capturing:
			out = append(out, line)
		}
	}
	if !capturing {
		t.Fatalf("flag --%s is not in the help output:\n%s", name, help)
	}
	return strings.Join(out, " ")
}
