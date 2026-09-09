# Demo

The scenario behind the terminal recording on
[ethankane.net](https://ethankane.net/projects/cellcast). It is committed so the
recording can be made again when output formats move, rather than being a video
of something nobody can reproduce.

Everything in it is real. Three kind clusters, each with its own service account
issuer, three agents publishing capacity read from real API servers, and a hub
that verifies every token against the cluster that signed it. Nothing is stubbed
and no output is retyped.

## Running it

```bash
just demo-setup   # three clusters, the registry, the hub and the agents (~6 min)
just demo         # the scenario, in this terminal
just demo-cast    # the scenario, recorded to cellcast.cast
just demo-stop    # stop the processes, keep the clusters for another take
just demo-down    # delete everything
```

`just demo-setup` builds a clean fleet every time. Iterating on the scenario
does not need that: `just demo-stop` then `just demo-up` restarts the hub and
agents against the clusters already running.

Needs `docker`, `kind`, `kubectl` and `helm`, plus `asciinema` to record.

## The fleet

Shaped so the answer is not the obvious one:

| Cell | Parked load | Label | Permitted |
| --- | --- | --- | --- |
| `euw1` | 4 pods | `env=prod` | yes, and it is the answer |
| `use1` | 8 pods | `env=prod` | yes |
| `apse1` | none | `env=dev` | no |

`apse1` is the emptiest cell in the fleet and the policy does not permit it, so
a scoring pass that ran before the permission filter would return it. That is
the argument the recording exists to make, and it is the same fixture
`just verify-e2e` asserts against.

## Why the hub runs on the host

The agents and the hub run as host processes rather than from the charts, so the
scenario can stop one and show what happens. A hub inside a cluster cannot be
killed mid-recording without the deployment restarting it, which is exactly the
behaviour you want in production and the wrong thing here.

## Why a CA bundle is involved

The hub fetches each issuer's discovery document and keys over HTTPS and
verifies that certificate. A kind cluster signs its own, so the hub is started
with `--oidc-ca-file` pointing at a bundle of all three cluster CAs. A managed
CI platform chains to a public root and needs none of this; a self-hosted issuer
needs exactly this. See [getting started](../docs/getting-started.md).

## Two things that will bite

**`/readyz` is not a signal that the fleet is reporting.** Readiness is bounded
by `--warmup-timeout` on purpose, so a hub that has heard from nobody reports
ready anyway and refuses placements with `PlacementUnavailable`, logging
`becoming ready before capacity covers the fleet`. `up.sh` waits for a placement
to succeed instead, which is the only check that means what it needs to mean.

**The credential cannot be shown expiring in a short take.** The TokenRequest
API refuses any lifetime under ten minutes, so a recording of a token expiring
costs ten minutes of real time. The scenario shows the credential is bounded by
what it can do rather than by waiting for it to die.
