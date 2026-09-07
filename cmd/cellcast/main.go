// Command cellcast is the client pipelines invoke to resolve a placement and
// obtain a credential for it.
package main

import (
	"fmt"
	"os"

	"github.com/ethan-kane-ops/cellcast/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
