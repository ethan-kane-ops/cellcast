package oidc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Provider names the claims a CI platform puts in its workload identity token.
//
// Extraction is declarative because every platform supported so far exposes
// flat claims. Adding a third is a map entry rather than a code change. A
// platform that needs real transformation should grow a function field here
// rather than special-casing the extractor.
type Provider struct {
	// Claims are the token claims carried into the caller's identity, and so
	// the only claims a PlacementPolicy can match on. Keeping the list explicit
	// means a token growing a new claim cannot silently widen what policy sees.
	Claims []string
}

// providers is the supported platform set.
//
// The claim names are the ones each platform documents for its OIDC token.
// `generic` extracts nothing beyond the standard `iss` and `sub`, which is the
// right default for an issuer whose claim shape is not known here.
var providers = map[string]Provider{
	"github": {Claims: []string{
		"repository",
		"ref",
		"environment",
		"job_workflow_ref",
	}},
	"buildkite": {Claims: []string{
		"organization_slug",
		"pipeline_slug",
		"build_branch",
	}},
	"generic": {},
}

// providerNames returns the supported provider names in a stable order.
func providerNames() []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// extract pulls the provider's claims out of a verified token payload.
//
// A claim that is absent is omitted rather than recorded as an empty string. A
// policy matching on `environment` must fail to match a token that carries no
// environment, not match it against "".
func (p Provider) extract(payload map[string]json.RawMessage) map[string]string {
	if len(p.Claims) == 0 {
		return nil
	}

	out := make(map[string]string, len(p.Claims))
	for _, name := range p.Claims {
		raw, ok := payload[name]
		if !ok {
			continue
		}
		value, ok := claimString(raw)
		if !ok {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// claimString renders a scalar claim as a string.
//
// Objects and arrays are skipped rather than serialised. A policy comparing
// against a JSON blob is a policy nobody can reason about, and flattening one
// into a string invites a selector that matches on punctuation.
func claimString(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// ProviderFor returns the named provider.
func ProviderFor(name string) (Provider, error) {
	p, ok := providers[name]
	if !ok {
		return Provider{}, fmt.Errorf("unknown provider %q", name)
	}
	return p, nil
}
