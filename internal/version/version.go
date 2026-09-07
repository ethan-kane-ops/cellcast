// Package version exposes build metadata stamped in at link time.
package version

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Stamped by the linker. See the justfile's `ldflags` recipe.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Info describes the running build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build metadata for this binary.
func Get() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

// String renders the build metadata on a single line.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s)",
		i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}

// NewCommand returns the `version` subcommand shared by all three binaries.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), Get())
			return err
		},
	}
}
