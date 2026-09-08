## What changed

<!-- One or two sentences, or a short bullet list of concrete changes. -->

## Why

<!-- The problem being solved. Not the journey. -->

## Notes

<!-- Only non-obvious decisions or trade-offs a reviewer needs. Delete if none. -->

## Testing

<!-- What was run. `just check` at minimum. -->

---

- [ ] `just check` passes
- [ ] Generated output is current (`just generate manifests`) if `api/` changed
- [ ] `docs/metrics.md`, the dashboard and the alerting rules are updated if a metric was added
- [ ] If this touches the minting path, the policy engine, or the authenticator: the **Notes** section says what an attacker gains if it is wrong
