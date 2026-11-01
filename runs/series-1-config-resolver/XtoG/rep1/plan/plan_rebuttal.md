# Response to adversary findings

1. **Blocker — contradictory type-check policy: fixed.** The plan now requires checking the exact dynamic type of every present occurrence of every supported key in every layer before selecting a winner. A wrong type panics even when shadowed. Effective-value validation remains a separate later step and applies only to the selected LogLevel.

2. **Major — extensibility: fixed within the fixed API.** Resolution uses an ordered layer slice and a single field descriptor table containing key, exact type, assignment, and optional validation. Adding a field is centralized in the table plus its `Config` member. The public four-map signature cannot accept an added layer without an API revision because the specification fixes that signature; the plan states this limitation explicitly and keeps internal precedence traversal data-driven.

3. **Minor — unknown-key policy: fixed.** The plan chooses to panic on unknown keys because only the four listed keys are legal. It calls for an explicit unknown-key test.

4. **Minor — exact integer types: fixed.** The plan specifies exact `int` values for `MaxRetries` and `TimeoutSec`, with no coercion, and explicitly identifies `int64`, `float64`, and `json.Number` as panic cases.

5. **Nit — validation return value: fixed.** The plan says that an invalid effective LogLevel returns the fully resolved `Config` and a wrapped `ErrInvalidLogLevel`, making the error return contract explicit.

6. **Nit — package placement: fixed.** The plan places implementation beside `worktree/go.mod` and the package test in `worktree/`, so the `config` package is compiled by `go test ./...` from the module root.
