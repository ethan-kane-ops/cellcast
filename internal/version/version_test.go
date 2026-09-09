package version

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

// TestStringNamesEveryStampedField covers the line a bug report carries.
//
// A field that quietly stops being rendered makes the report unanswerable:
// "dev" with no commit and no date describes every build ever made from this
// tree, including the one the reporter is not running.
func TestStringNamesEveryStampedField(t *testing.T) {
	info := Info{
		Version:   "v1.2.3",
		Commit:    "0f1e2d3",
		Date:      "2026-09-09T09:00:00Z",
		GoVersion: "go1.26.0",
		Platform:  "linux/arm64",
	}

	got := info.String()
	for _, want := range []string{info.Version, info.Commit, info.Date, info.GoVersion, info.Platform} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

func TestGetDescribesTheRunningBinary(t *testing.T) {
	// The two fields the linker does not stamp. They come from the runtime, so
	// a cross-compiled binary reports the platform it was built for rather than
	// the one it was built on.
	got := Get()
	if got.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; got.Platform != want {
		t.Errorf("Platform = %q, want %q", got.Platform, want)
	}
	if got.Version == "" || got.Commit == "" || got.Date == "" {
		t.Errorf("Get() = %+v, and an unstamped field is empty rather than a default", got)
	}
}

func TestVersionCommandWritesToStdout(t *testing.T) {
	// Not to stderr. `cellcast version` is read by scripts as often as by
	// people, and a version on stderr is a version a pipe does not see.
	cmd := NewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version = %v, want nil", err)
	}
	if !strings.Contains(out.String(), Get().String()) {
		t.Errorf("version printed %q, want the build metadata", out.String())
	}
}
