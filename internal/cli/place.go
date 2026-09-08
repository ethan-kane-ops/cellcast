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
	hub           string
	tokenFile     string
	workload      string
	ttl           string
	kubeconfig    string
	onUnavailable string
	cacheDir      string
	cacheTTL      time.Duration
	dark          bool
	dryRun        bool
	explain       bool
	jsonOut       bool
	timeout       time.Duration
}

// result is one answer, whatever produced it.
//
// The hub, the cache and a pinned cell all end up here so that the output has
// one shape. A pipeline reads `source` to tell them apart; it should not have
// to read three different response formats to work out which it got.
type result struct {
	placement *Placement
	source    string
	// unavailable is why the hub did not answer, set only on a fallback.
	unavailable error
	// age is how long ago the hub made the decision, set only for a cache hit.
	age time.Duration
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
or from --token-file. There is deliberately no --token flag.

A placement is a recommendation, not a command. If the hub cannot be reached,
--on-unavailable says what should happen: fail, reuse the last known cell for
this workload, or use a cell you pinned in advance. The default is to fail. No
fallback ever produces a credential, and no fallback can get you past a refusal.`,
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
	f.StringVar(&opts.onUnavailable, "on-unavailable", stanceFail,
		"what to do when the hub cannot answer: fail, last-known, or a cell name to pin")
	f.StringVar(&opts.cacheDir, "cache-dir", "", "where to cache decisions (or set "+cacheDirEnv+")")
	f.DurationVar(&opts.cacheTTL, "cache-ttl", DefaultCacheTTL, "how long a cached decision stays usable; 0 disables the cache")
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
	// Parsed before anything else, so a stance with a typo in it fails while
	// the hub is still up rather than during the outage it was written for.
	stance, err := parseStance(opts.onUnavailable)
	if err != nil {
		return err
	}

	client, err := NewClient(opts.hub, opts.tokenFile, opts.timeout)
	if err != nil {
		return err
	}
	cache := newDecisionCache(client.Hub(), opts.cacheDir, opts.cacheTTL)

	res, err := place(cmd, opts, client, cache, stance)
	if err != nil {
		return err
	}

	if opts.jsonOut {
		return writeJSONResult(cmd.OutOrStdout(), res, opts)
	}
	return writeTextResult(cmd.OutOrStdout(), res, opts)
}

// place gets a decision from the hub, or applies the declared stance.
func place(cmd *cobra.Command, opts *placeOptions, client *Client, cache *decisionCache, stance stance) (*result, error) {
	// --explain is requested from the hub whenever the caller asked for it in
	// either output mode, and never otherwise: the table names cells this
	// caller may not reach, so it is not something to fetch by default.
	placement, err := client.Place(cmd.Context(), placeRequest{
		Workload:   opts.workload,
		TargetDark: opts.dark,
		TTL:        opts.ttl,
		DryRun:     opts.dryRun,
		Explain:    opts.explain,
	})
	if err != nil {
		return fallback(cmd, opts, cache, stance, err)
	}

	// Recorded before the credential is written, so a decision the hub made is
	// not lost to a kubeconfig the runner had no room for.
	remember(cmd, cache, opts, placement)

	if !opts.dryRun {
		if placement.Credential == nil {
			return nil, fmt.Errorf("hub placed the workload on %s but returned no credential", placement.Cell)
		}
		if err := writeKubeconfig(opts.kubeconfig, placement.Cell, placement.Credential); err != nil {
			return nil, err
		}
	}
	return &result{placement: placement, source: SourceHub}, nil
}

// fallback applies the caller's declared stance to a hub that did not answer.
func fallback(cmd *cobra.Command, opts *placeOptions, cache *decisionCache, stance stance, cause error) (*result, error) {
	if !stance.fallsBack() {
		return nil, cause
	}
	if !fallbackAllowed(cause) {
		return nil, refusedFallback(cause)
	}

	res := &result{unavailable: cause}
	switch {
	case stance.cell != "":
		res.placement = &Placement{
			Cell:         stance.cell,
			Confidence:   ConfidenceNone,
			TargetedDark: opts.dark,
			DryRun:       opts.dryRun,
		}
		res.source = SourcePinned

	default:
		entry, missErr := cache.load(opts.workload, opts.dark)
		if missErr != nil {
			// Both halves, because either one alone sends the reader looking in
			// the wrong place: the hub failure explains why a fallback was
			// needed and the miss explains why there was none to apply.
			return nil, fmt.Errorf("%w, and %s", cause, missErr)
		}
		res.placement = &Placement{
			Cell:         entry.Cell,
			Policy:       entry.Policy,
			Strategy:     entry.Strategy,
			Confidence:   ConfidenceStale,
			DecidedFor:   entry.DecidedFor,
			TargetedDark: entry.TargetedDark,
			DryRun:       opts.dryRun,
		}
		res.source = SourceCache
		res.age = entry.age(time.Now())
	}

	// A kubeconfig from an earlier run holds a credential for whichever cell
	// that run chose, which is not necessarily this one. Left in place it would
	// be picked up by the next step and the deploy would land somewhere nobody
	// selected, which is the exact failure a declared fallback exists to
	// prevent.
	if !opts.dryRun {
		if err := os.Remove(opts.kubeconfig); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing the stale kubeconfig at %s: %w", opts.kubeconfig, err)
		}
	}
	return res, nil
}

// remember stores a decision the hub made.
//
// Only ever called on a fresh response. A cache entry written from a fallback
// would keep renewing its own lifetime, so a decision could outlive its TTL for
// as long as the outage lasted.
//
// A failure to write is reported and does not fail the placement. The hub
// answered; refusing to act on that because a cache file could not be written
// would make the cache a dependency instead of a cushion.
func remember(cmd *cobra.Command, cache *decisionCache, opts *placeOptions, p *Placement) {
	err := cache.save(&cachedDecision{
		Workload:     opts.workload,
		TargetedDark: p.TargetedDark,
		Cell:         p.Cell,
		Policy:       p.Policy,
		Strategy:     p.Strategy,
		DecidedFor:   p.DecidedFor,
	})
	if err != nil {
		// Best effort by definition, including this line about it.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: this decision was not cached: %v\n", err)
	}
}

// writeJSONResult prints the decision without the credential.
//
// The token is stripped rather than omitted by the response type, because the
// same struct is what carries it to the kubeconfig writer. A caller asking for
// JSON is piping this into something, and the one place a credential must never
// go is a pipeline's stdout.
func writeJSONResult(w io.Writer, res *result, opts *placeOptions) error {
	redacted := *res.placement
	if redacted.Credential != nil {
		cred := *redacted.Credential
		cred.Token = ""
		redacted.Credential = &cred
	}

	out := struct {
		*Placement
		Source string `json:"source"`
		// Unavailable is why the hub did not answer. Present only on a
		// fallback, which makes it the second way to detect one.
		Unavailable string `json:"unavailable,omitempty"`
		Kubeconfig  string `json:"kubeconfig,omitempty"`
	}{
		Placement:  &redacted,
		Source:     res.source,
		Kubeconfig: kubeconfigPath(res, opts),
	}
	if res.unavailable != nil {
		out.Unavailable = res.unavailable.Error()
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	return nil
}

func kubeconfigPath(res *result, opts *placeOptions) string {
	if res.placement.DryRun || res.placement.Credential == nil {
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

func writeTextResult(w io.Writer, res *result, opts *placeOptions) error {
	out := &lineWriter{w: w}
	p := res.placement

	if res.source != SourceHub {
		writeFallbackNotice(out, res, opts)
	} else {
		verb := "placed"
		if p.DryRun {
			verb = "would place"
		}
		out.printf("%s %s on %s (policy %s, %s, confidence %s)\n",
			verb, opts.workload, p.Cell, p.Policy, p.Strategy, p.Confidence)
	}

	if p.TTL != nil {
		if p.TTL.Clamped {
			// Said plainly rather than buried: a deploy that assumes it has the
			// lifetime it asked for will fail partway through instead of at the
			// start, which is the more expensive failure.
			out.printf("credential valid for %s (requested %s, capped by policy at %s)\n",
				p.TTL.Granted, p.TTL.Requested, p.TTL.Max)
		} else {
			out.printf("credential valid for %s\n", p.TTL.Granted)
		}
	}

	if p.Credential != nil && !p.DryRun {
		out.printf("kubeconfig written to %s (%s, as %s/%s)\n",
			opts.kubeconfig, kubeconfigMode, p.Credential.Namespace, p.Credential.ServiceAccount)
	}

	if len(p.Candidates) > 0 {
		out.printf("\n")
		writeCandidateTable(out, p.Candidates)
	}
	return out.err
}

// writeFallbackNotice says what happened, which cell was used instead, and that
// no credential came with it.
//
// The last line is the one that matters. A pipeline reading only the cell name
// would otherwise carry on into a deploy step with nothing to authenticate
// with, and find out at the kubectl call rather than here.
func writeFallbackNotice(out *lineWriter, res *result, opts *placeOptions) {
	p := res.placement

	out.printf("the hub did not answer: %v\n", res.unavailable)
	switch res.source {
	case SourceCache:
		out.printf("falling back to the last known cell for %s: %s (policy %s, %s, decided %s ago",
			opts.workload, p.Cell, p.Policy, p.Strategy, res.age)
		if p.DecidedFor != "" {
			out.printf(" for %s", p.DecidedFor)
		}
		out.printf(")\n")
	default:
		out.printf("falling back to the cell declared by --on-unavailable for %s: %s\n", opts.workload, p.Cell)
	}
	out.printf("no credential was minted; cellcast replays a decision and never a credential, " +
		"so this deploy needs one of its own\n")
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
