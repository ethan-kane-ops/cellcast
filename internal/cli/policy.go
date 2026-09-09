package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// policyOptions configures `cellcast policy test`.
type policyOptions struct {
	hub       string
	tokenFile string
	workload  string
	timeout   time.Duration
	jsonOut   bool
}

func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Inspect how policy sees this caller",
	}
	cmd.AddCommand(newPolicyTestCmd())
	return cmd
}

func newPolicyTestCmd() *cobra.Command {
	var opts policyOptions

	cmd := &cobra.Command{
		Use:   "test",
		Short: "Ask the hub whether this caller would be admitted, without deploying",
		Long: `Ask the hub whether this caller would be admitted, and print the identity it
read from the token.

Placement is deny by default, and a refusal carries a machine-readable reason
and nothing else. That is correct for a deploy and useless for writing a policy:
a policy naming a subject in a format the issuer does not actually produce
authenticates perfectly and is then refused, which looks like a broken hub.

This runs the whole decision with no credential minted, and prints the issuer,
subject and claims the hub resolved. Compare those against spec.subjects:

    kubectl -n cellcast-system get placementpolicy -o yaml

The hub does not report policy contents. It answers for the caller in front of
it, and a PlacementPolicy is the operator's to read.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPolicyTest(cmd, &opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.hub, "hub", os.Getenv("CELLCAST_HUB"), "hub base URL (or set CELLCAST_HUB)")
	f.StringVar(&opts.tokenFile, "token-file", "", "file holding the caller's identity token (or set CELLCAST_TOKEN)")
	f.StringVar(&opts.workload, "workload", "", "the workload to test as (required)")
	f.DurationVar(&opts.timeout, "timeout", 30*time.Second, "how long to wait for the hub")
	f.BoolVar(&opts.jsonOut, "json", false, "emit the hub's answer as JSON")

	if err := cmd.MarkFlagRequired("workload"); err != nil {
		panic(err)
	}
	return cmd
}

func runPolicyTest(cmd *cobra.Command, opts *policyOptions) error {
	client, err := NewClient(opts.hub, opts.tokenFile, opts.timeout)
	if err != nil {
		return err
	}

	// dryRun, always. A command whose purpose is to be run repeatedly while
	// editing a policy must not mint a credential each time, and must not fill
	// the audit trail with deploys that never happened.
	res, err := client.Place(cmd.Context(), placeRequest{
		Workload: opts.workload,
		DryRun:   true,
	})

	var ref *Refusal
	switch {
	case errors.As(err, &ref):
		return reportRefusal(cmd.OutOrStdout(), ref, opts.jsonOut)
	case err != nil:
		return err
	}
	return reportAdmitted(cmd.OutOrStdout(), res, opts.jsonOut)
}

// reportRefusal prints why the caller was refused, and who the hub thought they
// were. It returns the refusal so the exit code is non-zero: this is a check,
// and a check that fails silently in a pipeline is worse than no check.
func reportRefusal(w io.Writer, ref *Refusal, jsonOut bool) error {
	if jsonOut {
		if err := writeIndentedJSON(w, ref); err != nil {
			return err
		}
		return ref
	}

	out := &lineWriter{w: w}
	out.printf("refused: %s\n", ref.Reason)
	if ref.Message != "" {
		out.printf("  %s\n", ref.Message)
	}
	writeIdentity(out, ref.Identity)

	if ref.Reason == refusal.NoPolicy {
		out.printf("\nno policy names that caller. Compare the values above against spec.subjects:\n" +
			"  kubectl -n cellcast-system get placementpolicy -o yaml\n")
	}

	// The write error wins. A broken pipe means the diagnostic this command
	// exists to produce never arrived, and reporting only the refusal would
	// send somebody back to read output that is not there.
	if out.err != nil {
		return out.err
	}
	return ref
}

func reportAdmitted(w io.Writer, res *Placement, jsonOut bool) error {
	if jsonOut {
		return writeIndentedJSON(w, res)
	}

	out := &lineWriter{w: w}
	out.printf("admitted by policy %s\n", res.Policy)
	writeIdentity(out, res.Identity)
	out.printf("\nwould place in %s, scored by %s\n", res.Cell, res.Strategy)
	return out.err
}

// writeIdentity prints the caller as the hub read them.
//
// Claims are sorted so that two runs of this command can be diffed. A map
// printed in Go's iteration order changes between runs and makes the one thing
// this command exists for, comparing two strings, harder than it was.
func writeIdentity(out *lineWriter, id *Identity) {
	if id == nil {
		out.printf("\nthe hub did not get as far as reading the token.\n")
		return
	}

	out.printf("\nthe hub read this token as:\n")
	out.printf("  issuer   %s\n", id.Issuer)
	out.printf("  subject  %s\n", id.Subject)

	keys := make([]string, 0, len(id.Claims))
	for k := range id.Claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		label := "claims  "
		if i > 0 {
			label = "        "
		}
		out.printf("  %s %s=%s\n", label, k, id.Claims[k])
	}
}

func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
