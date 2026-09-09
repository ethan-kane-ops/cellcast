# Extending cellcast

Two things are pluggable, and they sit at opposite ends of one request:

| Seam | Answers | Interface | Shipped |
|---|---|---|---|
| **Caller identity** | who is asking, and what may a policy match on | `oidc.Provider` | `github`, `buildkite`, `generic` |
| **Trust provider** | how a credential is minted for the chosen cell | `broker.Provider` | `kubernetes` |

Neither is a plugin system. Both are compiled in, because a broker that loads a
third-party authenticator at runtime has given away the thing it exists to
protect.

## Caller identity

### Most issuers need no code

The hub verifies any OIDC issuer with a discovery document and a JWKS. Trusting
one is configuration:

```bash
cellcast-hub --oidc-issuer https://gitlab.example.com=generic --oidc-audience cellcast
```

Or in the chart:

```yaml
hub:
  oidc:
    audience: cellcast
    issuers:
      - url: https://gitlab.example.com
        provider: generic
```

`generic` carries `iss` and `sub` and nothing else, so a `PlacementPolicy` under
that issuer matches on `subject`:

```yaml
spec:
  subjects:
    - issuer: https://gitlab.example.com
      subject: project_path:acme/checkout:ref_type:branch:ref:main
```

That is enough for most platforms. GitLab, Tekton, Spacelift, CircleCI and any
Kubernetes cluster issuer all work this way today, unchanged.

!!! warning "An issuer-only selector is broader than it looks"

    Naming an issuer and no subject permits every workload that issuer can
    vouch for: every repository on the platform, or every pod in the cluster.
    Name a `subject`.

### When a named provider earns its place

Add one when matching on `sub` alone is too coarse or too brittle. GitHub's
`sub` is a single string encoding repository, ref type and ref, so pinning a
repository across every branch means matching a claim rather than a prefix of
`sub`. cellcast has no pattern matching by design, so the claim has to be
extracted.

The whole change is a map entry in `internal/hub/oidc/claims.go`:

```go
var providers = map[string]Provider{
	"gitlab": {Claims: []string{
		"project_path",
		"ref",
		"ref_type",
		"environment",
	}},
}
```

A policy can then match those claims directly:

```yaml
spec:
  subjects:
    - issuer: https://gitlab.example.com
      claims:
        project_path: acme/checkout
        ref: main
```

The provider is not named in the policy. It is a property of the issuer, set
where the issuer is trusted, so a policy cannot select its own claim extractor.

### Three rules the extractor holds

Read `extract` and `claimString` before changing either. Each rule exists
because breaking it fails open.

**The claim list is an allowlist, not a hint.** Only listed claims reach the
identity, so a platform adding a claim to its token cannot silently widen what
a policy sees. Extracting everything would mean an issuer choosing which claims
cellcast authorizes on.

**Only scalars.** Objects and arrays are skipped rather than serialised. A
policy comparing against a JSON blob is one nobody can reason about, and
flattening one into a string invites a selector that matches on punctuation.

**Absent is not empty.** A missing claim is omitted, never recorded as `""`. A
policy requiring `environment: production` must fail to match a token carrying
no environment, rather than matching it against the empty string.

A fuzz target asserts all three (`just fuzz`). A platform that needs real
transformation rather than selection should grow a function field on `Provider`
instead of special-casing the extractor.

### What a provider does not do

Naming an issuer in a policy does not trust it. The hub verifies signatures only
against issuers it was started with, so a policy naming an unconfigured issuer
matches nothing. The two lists are separate on purpose: adding a provider is a
code change that ships in a binary, and trusting an issuer is a configuration
change an operator makes.

### Alongside the code

- A case in `TestPolicyClaimKeysMatchWhatTheAuthenticatorProduces`. It compares
  the authenticator's vocabulary against the policy engine's, because a policy
  constraining `repo` against an authenticator emitting `repository` matches
  nothing, and a policy that matches nothing is a deny-all that reads as correct
  in review.
- A row in the issuers table in [Pipeline integration](pipeline-integration.md).
- The claim names must be the ones the platform documents for its OIDC token,
  not the ones observed in one token.

## Trust providers

A trust provider mints the credential for a cell that placement has already
chosen. Kubernetes `TokenRequest` is the only implementation; the interface was
written for two, so that a second does not reopen the broker's semantics
([ADR-004](architecture.md#adr-004-downstream-credentials-are-minted-never-stored)).

```go
type Provider interface {
	Kind() cellcastv1alpha1.TrustProvider
	MinTTL() time.Duration
	Mint(ctx context.Context, req MintRequest) (*Credential, error)
}
```

### The contract

**`MinTTL` is the floor the mechanism refuses to go below**, and it is on the
interface rather than discovered in the deploy path. Kubernetes `TokenRequest`
refuses anything under ten minutes; AWS STS refuses anything under fifteen.
Neither is negotiable.

The hub checks `--token-max-ttl` against the floor at startup and refuses to
start below it. A policy `max` below the floor is caught later, at mint, with
`ErrTTLBelowProviderFloor`. A request shorter than the floor is rounded up,
because the operator's ceiling still holds; rounding the ceiling up would break
it.

**`Mint` returns the expiry the issuing authority reported**, never the one that
was requested. A provider that echoes the request produces an audit trail
describing credentials that do not exist, and a client that stops using a
credential at the wrong moment.

**Never put token material anywhere but `Credential.Token`.** The type redacts
itself under `%v`, `String()` and `slog`, and `audit.Record` has no field that
can hold a token. A provider that returns the token in an error message routes
around both.

**Fail fast.** Minting is in the deploy critical path, so an unreachable cell
must error quickly and let the caller act on its declared fallback stance,
rather than holding the pipeline open until something else times out.

### Wiring one in

1. Add the value to `TrustProvider` in `api/v1alpha1/trustconfig_types.go` and
   to its `+kubebuilder:validation:Enum` marker, then `just generate manifests`.
2. Add a provider-specific block to `TrustConfigSpec` if the mechanism needs
   parameters, with a CEL rule pairing it to the provider value. `kubernetes`
   is the worked example.
3. Implement `Provider` in `internal/hub/broker/`.
4. Extend `ValidateTrust` with a case for it. Until then the value returns
   `ErrProviderNotImplemented`, which is the honest state.
5. Register it in `cmd/cellcast-hub/main.go`, in the `broker.New` call.

Step 4 is why `provider: aws` is already in the CRD enum with no implementation
behind it. A `TrustConfig` naming it is accepted by the API server and reports
`READY: False` with reason `ProviderNotImplemented` and the message *"provider
"aws" is not implemented in this build"*. An operator finds out when they write
it, not when a deploy needs it.

### Credential source

`credentialSource` takes exactly one of `secretRef` or `inCluster`, enforced by a
CEL rule on the CRD. Whatever a new provider needs to authenticate to its target
belongs behind the same seam, and it must be read uncached at the moment of use.
The hub does not hold a resident map of spoke credentials, which is why it needs
no `watch` on Secrets (`docs/threat-model.md` T-08).

## What neither seam changes

Both extension points sit *inside* the security model rather than beside it. A
new provider of either kind cannot:

- make placement anything other than deny-by-default,
- let a caller pick its own scoring strategy or reach a cell no policy permits,
- raise a TTL above the policy ceiling or the hub's `--token-max-ttl`,
- put a credential on the audit record, in a log line, or in a metric label,
- run in the agent, which links neither package.

If a change needs one of those, it is not an extension. It is a change to the
[threat model](threat-model.md), and it starts there.
