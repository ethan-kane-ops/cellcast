package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// placeOptions are the flags of `cellcast place`.
type placeOptions struct {
	hub        string
	tokenFile  string
	workload   string
	ttl        string
	kubeconfig string
	dark       bool
	dryRun     bool
	explain    bool
	jsonOut    bool
	timeout    time.Duration
}

func newPlaceCmd() *cobra.Command {
	opts := placeOptions{}

	cmd := &cobra.Command{
		Use:   "place",
		Short: "Ask the hub where to deploy and write a kubeconfig for the chosen cell",
		Long: `place asks the hub which cell should receive a workload, obtains a
short-lived credential for it, and writes a kubeconfig your deploy step can use.

The credential is written to a file readable only by you. It is never printed
and never passed as a command argument, because a shared CI runner exposes both
to every other job on the machine.

The caller's identity token is read from the CELLCAST_TOKEN environment variable
or from --token-file. There is deliberately no --token flag.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPlace(cmd, &opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.hub, "hub", os.Getenv("CELLCAST_HUB"), "hub base URL (or set CELLCAST_HUB)")
	f.StringVar(&opts.tokenFile, "token-file", "", "file holding the caller's identity token (or set CELLCAST_TOKEN)")
	f.StringVar(&opts.workload, "workload", "", "the workload being deployed (required)")
	f.StringVar(&opts.ttl, "ttl", "", "requested credential lifetime, for example 15m; policy may grant less")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "cellcast.kubeconfig", "where to write the kubeconfig")
	f.BoolVar(&opts.dark, "dark", false, "target a dark cell, if policy permits it")
	f.BoolVar(&opts.dryRun, "dry-run", false, "run the whole decision without minting a credential")
	f.BoolVar(&opts.explain, "explain", false, "show every candidate cell and why it was or was not chosen")
	f.BoolVar(&opts.jsonOut, "json", false, "emit the decision as JSON")
	f.DurationVar(&opts.timeout, "timeout", 30*time.Second, "how long to wait for the hub")

	if err := cmd.MarkFlagRequired("workload"); err != nil {
		panic(err)
	}
	return cmd
}

func runPlace(cmd *cobra.Command, opts *placeOptions) error {
	client, err := NewClient(opts.hub, opts.tokenFile, opts.timeout)
	if err != nil {
		return err
	}

	// --explain is requested from the hub whenever the caller asked for it in
	// either output mode, and never otherwise: the table names cells this
	// caller may not reach, so it is not something to fetch by default.
	result, err := client.Place(cmd.Context(), placeRequest{
		Workload:   opts.workload,
		TargetDark: opts.dark,
		TTL:        opts.ttl,
		DryRun:     opts.dryRun,
		Explain:    opts.explain,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	if !opts.dryRun {
		if result.Credential == nil {
			return fmt.Errorf("hub placed the workload on %s but returned no credential", result.Cell)
		}
		if err := writeKubeconfig(opts.kubeconfig, result.Cell, result.Credential); err != nil {
			return err
		}
	}

	if opts.jsonOut {
		return writeJSONResult(out, result, opts)
	}
	return writeTextResult(out, result, opts)
}

// writeJSONResult prints the decision without the credential.
//
// The token is stripped rather than omitted by the response type, because the
// same struct is what carries it to the kubeconfig writer. A caller asking for
// JSON is piping this into something, and the one place a credential must never
// go is a pipeline's stdout.
func writeJSONResult(w io.Writer, result *Placement, opts *placeOptions) error {
	redacted := *result
	if result.Credential != nil {
		cred := *result.Credential
		cred.Token = ""
		redacted.Credential = &cred
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		*Placement
		Kubeconfig string `json:"kubeconfig,omitempty"`
	}{Placement: &redacted, Kubeconfig: kubeconfigPath(result, opts)}); err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	return nil
}

func kubeconfigPath(result *Placement, opts *placeOptions) string {
	if result.DryRun || result.Credential == nil {
		return ""
	}
	return opts.kubeconfig
}

// lineWriter accumulates the first write error.
//
// Output goes to a pipeline's stdout, which can be closed under us: `cellcast
// place --explain | head` is an ordinary thing to type. Discarding these errors
// would turn a broken pipe into a command that reports success having printed
// nothing.
type lineWriter struct {
	w   io.Writer
	err error
}

func (l *lineWriter) printf(format string, a ...any) {
	if l.err != nil {
		return
	}
	_, l.err = fmt.Fprintf(l.w, format, a...)
}

func writeTextResult(w io.Writer, result *Placement, opts *placeOptions) error {
	out := &lineWriter{w: w}

	verb := "placed"
	if result.DryRun {
		verb = "would place"
	}
	out.printf("%s %s on %s (policy %s, %s)\n",
		verb, opts.workload, result.Cell, result.Policy, result.Strategy)

	if result.TTL != nil {
		if result.TTL.Clamped {
			// Said plainly rather than buried: a deploy that assumes it has the
			// lifetime it asked for will fail partway through instead of at the
			// start, which is the more expensive failure.
			out.printf("credential valid for %s (requested %s, capped by policy at %s)\n",
				result.TTL.Granted, result.TTL.Requested, result.TTL.Max)
		} else {
			out.printf("credential valid for %s\n", result.TTL.Granted)
		}
	}

	if result.Credential != nil && !result.DryRun {
		out.printf("kubeconfig written to %s (%s, as %s/%s)\n",
			opts.kubeconfig, kubeconfigMode, result.Credential.Namespace, result.Credential.ServiceAccount)
	}

	if len(result.Candidates) > 0 {
		out.printf("\n")
		writeCandidateTable(out, result.Candidates)
	}
	return out.err
}

// writeCandidateTable renders why each cell was or was not chosen.
//
// This is the answer to "why did my deploy land there", and it is the reason
// --explain exists. A rejection nobody can read is a rejection somebody works
// around.
func writeCandidateTable(out *lineWriter, candidates []Candidate) {
	tw := tabwriter.NewWriter(out.w, 0, 0, 2, ' ', 0)
	table := &lineWriter{w: tw}
	table.printf("CELL\tVERDICT\tSTAGE\tUTILISATION\tDETAIL\n")

	for _, c := range candidates {
		verdict, util := "refused", ""
		switch {
		case c.Chosen:
			verdict = "chosen"
		case c.Admitted:
			verdict = "eligible"
		}
		if c.Admitted {
			util = fmt.Sprintf("%.0f%%", c.Utilisation*100)
		}
		table.printf("%s\t%s\t%s\t%s\t%s\n", c.Cell, verdict, c.Stage, util, c.Reason)
	}

	if err := tw.Flush(); err != nil && table.err == nil {
		table.err = err
	}
	out.err = table.err
}
