// Package cli implements the cellcast client command tree.
//
// The client is what a pipeline invokes. It ships as a static archive and as a
// container image, and does not link controller-runtime.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/cellcast/internal/version"
)

// NewRootCmd builds the cellcast client command tree.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cellcast",
		Short: "Ask cellcast where to deploy, and get a short-lived credential",
		Long: `cellcast asks the hub where a workload should be deployed and obtains a
short-lived, scoped credential for the chosen cell.

It authenticates with the workload identity token your CI platform already
issues. There is no secret to configure.`,
		SilenceUsage: true,
		// main prints the error and sets the exit code. Without this cobra
		// prints it as well, so every failed command reports itself twice.
		SilenceErrors: true,
	}

	cmd.AddCommand(newPlaceCmd())
	cmd.AddCommand(newPolicyCmd())
	cmd.AddCommand(newCellCmd())
	cmd.AddCommand(version.NewCommand())
	return cmd
}
