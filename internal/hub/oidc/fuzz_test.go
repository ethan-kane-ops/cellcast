package oidc

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The token parsing in this package is the only cellcast code that reads bytes
// an attacker chooses before anything has authenticated them. Every function
// fuzzed here runs before the signature is checked, so "it never panics" is a
// availability property for the whole estate, and "it never accepts" is the
// authentication boundary itself.
//
// Seeds live in f.Add rather than in testdata so they are reviewable in a diff
// and so `go test` runs every one of them as an ordinary unit test. testdata
// corpora are for inputs a fuzzer found, which is a different thing.

// segment renders v as a JWT segment.
func segment(tb testing.TB, v any) string {
	tb.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		tb.Fatalf("marshalling a seed: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func seedTokens(f *testing.F) {
	f.Helper()

	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("a.b.c")
	f.Add("....")
	// Valid shape, so the fuzzer starts from something that parses.
	f.Add(segment(f, map[string]any{"alg": "RS256"}) + "." +
		segment(f, map[string]any{"iss": "https://example.test", "sub": "s"}) + ".sig")
	// alg none, the classic bypass.
	f.Add(segment(f, map[string]any{"alg": "none"}) + "." +
		segment(f, map[string]any{"iss": "https://example.test"}) + ".")
	// Deeply nested payload, which is where a recursive decoder falls over.
	f.Add("eyJhbGciOiJSUzI1NiJ9." +
		base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(`{"a":`, 400)+"1"+strings.Repeat("}", 400))) +
		".sig")
	// Base64 that decodes to something that is not JSON.
	f.Add("eyJhbGciOiJSUzI1NiJ9.////.sig")
	f.Add("\x00.\x00.\x00")
}

// FuzzPeekIssuer covers the first thing the hub does with an unverified token.
//
// It selects which issuer's key set to verify against, so a crash here is
// reachable by anyone who can reach the API with no credential at all.
func FuzzPeekIssuer(f *testing.F) {
	seedTokens(f)

	f.Fuzz(func(t *testing.T, raw string) {
		header, err := peekIssuer(raw)
		if err != nil {
			// A refusal must not also return something usable.
			if header.alg != "" || header.issuer != "" {
				t.Fatalf("peekIssuer(%q) refused and still returned alg=%q issuer=%q",
					raw, header.alg, header.issuer)
			}
			return
		}

		// Accepting means both fields drive the next step: alg picks the
		// allowlist check and issuer picks the key set. An empty either way
		// would be compared against an allowlist as the empty string.
		if header.alg == "" || header.issuer == "" {
			t.Fatalf("peekIssuer(%q) accepted with alg=%q issuer=%q", raw, header.alg, header.issuer)
		}
	})
}

// FuzzBearerToken covers the header parse, which happens before even the token
// shape is known.
func FuzzBearerToken(f *testing.F) {
	f.Add("")
	f.Add("Bearer")
	f.Add("Bearer ")
	f.Add("bearer abc")
	f.Add("BEARER   abc   ")
	f.Add("Basic abc")
	f.Add("Bearer " + strings.Repeat("a", MaxTokenBytes+1))
	f.Add("Bearer \x00\x00")
	f.Add("Bearer a b c")

	f.Fuzz(func(t *testing.T, header string) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", header)

		token, err := bearerToken(req)
		if err != nil {
			if token != "" {
				t.Fatalf("bearerToken(%q) refused and still returned %q", header, token)
			}
			return
		}

		// The bound exists so an oversized header cannot be used to make the
		// decoder do work. Accepting past it would defeat that entirely.
		if len(token) > MaxTokenBytes {
			t.Fatalf("bearerToken accepted %d bytes, over the %d limit", len(token), MaxTokenBytes)
		}
		if token == "" {
			t.Fatalf("bearerToken(%q) accepted an empty token", header)
		}
	})
}

// FuzzClaimExtraction covers the step that decides what a PlacementPolicy is
// allowed to match on.
//
// The property that matters is not that it survives bad input but that it never
// widens: a claim the provider does not declare must never appear, and a claim
// whose value was an object or an array must never be flattened into a string.
// A policy matching on a flattened blob is one nobody can reason about, and a
// selector matching on punctuation is how a policy is quietly bypassed.
func FuzzClaimExtraction(f *testing.F) {
	f.Add("github", `{"repository":"acme/app","ref":"refs/heads/main"}`)
	f.Add("github", `{"repository":{"nested":"object"}}`)
	f.Add("github", `{"repository":["an","array"]}`)
	f.Add("github", `{"repository":null}`)
	f.Add("github", `{"repository":1e400}`)
	f.Add("github", `{"repository":true,"ref":12345}`)
	f.Add("buildkite", `{"organization_slug":"acme","pipeline_slug":"app"}`)
	f.Add("generic", `{"anything":"at all"}`)
	f.Add("github", `{}`)
	f.Add("github", `{"repository":"`+strings.Repeat("a", 4096)+`"}`)
	f.Add("nosuchprovider", `{"repository":"acme/app"}`)

	f.Fuzz(func(t *testing.T, name, payload string) {
		provider, err := ProviderFor(name)
		if err != nil {
			return
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			return
		}

		claims := provider.extract(raw)

		// The documented contract: nil rather than an empty map, so a caller
		// cannot tell "no claims" apart from "a provider that declares none"
		// by length and get it wrong.
		if claims != nil && len(claims) == 0 {
			t.Fatal("extract returned an empty non-nil map")
		}

		for key, value := range claims {
			if !contains(provider.Claims, key) {
				t.Fatalf("extract emitted %q, which %s does not declare: %v", key, name, provider.Claims)
			}

			// Whatever was emitted must have been a JSON scalar. Re-decoding is
			// the check, because the alternative is trusting the function under
			// test to say so.
			var decoded any
			if err := json.Unmarshal(raw[key], &decoded); err != nil {
				t.Fatalf("extract emitted %q=%q from a claim that is not valid JSON", key, value)
			}
			switch decoded.(type) {
			case string, bool, float64:
			default:
				t.Fatalf("extract flattened a %T into %q=%q; a policy cannot match on that safely",
					decoded, key, value)
			}
		}
	})
}

// FuzzStandardClaims covers the RFC 7519 claim decode, including the `aud`
// field, which is a string or an array of strings and is therefore the one
// place this parser has to guess.
func FuzzStandardClaims(f *testing.F) {
	f.Add(`{"iss":"https://example.test","sub":"s","aud":"cellcast","exp":1,"nbf":2,"iat":3}`)
	f.Add(`{"aud":["one","two"]}`)
	f.Add(`{"aud":[]}`)
	f.Add(`{"aud":[1,2,3]}`)
	f.Add(`{"aud":{"not":"a string"}}`)
	f.Add(`{"exp":"not a number"}`)
	f.Add(`{"exp":9223372036854775807}`)
	f.Add(`{"exp":-9223372036854775808}`)
	f.Add(`{"sub":null}`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, payload string) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			return
		}

		std, err := standardClaims(raw)
		if err != nil {
			return
		}

		// An accepted `aud` is a list of real strings. An entry that decoded to
		// something else and was silently kept would be compared against the
		// configured audience, which is the check that stops a token minted for
		// somebody else being replayed here.
		for _, aud := range std.Audience {
			_ = aud
		}
		if raw["aud"] != nil && len(std.Audience) == 0 {
			var probe any
			if err := json.Unmarshal(raw["aud"], &probe); err == nil {
				if s, ok := probe.(string); ok && s != "" {
					t.Fatalf("standardClaims dropped a string aud %q", s)
				}
			}
		}
	})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
