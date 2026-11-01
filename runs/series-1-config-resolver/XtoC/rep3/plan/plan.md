# Layered Configuration Resolver Implementation Plan

## Package and public API

Implement the `config` package at the module root, in `config.go` (alongside the shipped root-level `resolve_public_test.go`), with exactly the specified API:

```go
func Resolve(defaults, file, env, flags map[string]any) (Config, error)

type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

var ErrInvalidLogLevel = errors.New("config: invalid log level")
```

The sentinel is stable and package-prefixed. Document `Resolve` to state that inputs are read but never mutated or retained; a validation error returns `Config{}` and a wrapped `ErrInvalidLogLevel`.

## Internal data model

Represent field knowledge in one registry of descriptors, one entry per supported key. Each descriptor contains the key and an `apply func(*Config, any)` that performs a checked assertion to the exact declared Go type and assigns the typed value to its `Config` field. On failed assertion, panic with a message containing the key, layer name, expected Go type, and actual type (`%T`). A nil value for a present key therefore panics as a type mismatch.

Use the same descriptor registry to validate map keys: build a membership lookup from the registry (or otherwise consult the registry directly) and panic on any key outside the four supported names. This catches misspellings instead of silently treating them as absent. Include the offending key and layer name in the panic. Avoid reflection over `Config`; explicit typed descriptor functions keep the type contract visible and make failures readable.

Keep layer names adjacent to their maps in an ordered slice: `defaults`, `file`, `env`, `flags`, from lowest to highest precedence. Start with `Config{}`. For each layer in that order, first reject unsupported keys and then visit the field descriptors; for each present key, call its `apply` function. Membership lookup uses the two-result map lookup, so explicit `false`, `0`, and `""` are applied, while absence leaves the prior value intact. Continue scanning all layers even after a higher layer has supplied a key: every present value must be type-checked, including an invalidly typed value overridden later.

## Resolution and validation rules

1. **Precedence and absence:** ordered low-to-high assignment makes the highest-precedence layer that presents a key win. Missing keys have no effect. A field never presented stays at its Go zero value.
2. **Wrong types and unknown keys:** every present known key must have its exact type (`bool`, `int`, or `string`) or panic, irrespective of whether a later value overrides it. Any key outside `Verbose`, `MaxRetries`, `LogLevel`, and `TimeoutSec` is an unsupported programmer input and also panics. Diagnostics identify key and layer; type errors additionally identify expected and actual types.
3. **Effective-only `LogLevel` validation:** after all maps have been processed, accept exactly `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`. Validate only the resolved value, so an invalid value overwritten by a higher layer is not rejected. For another effective value, return `Config{}` and `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)` so `errors.Is` works and the value is quoted in diagnostics.
4. **Other fields:** do not add semantic or range validation for `Verbose`, `MaxRetries`, or `TimeoutSec`.
5. **Scope and ownership:** use only the four caller-supplied in-memory maps. Do not consult the process environment, files, or command-line arguments; do not mutate or retain input maps.

The descriptor registry keeps the implementation maintainable as fields are added: a new supported field is represented by its `Config` member and one descriptor entry, while resolution, key checking, and ordered precedence remain generic. Adding a field also requires updating public contract and focused tests.

## Test approach

Keep implementation-focused tests in the root `config_test.go` package. First run the shipped public suite to verify all eight acceptance cases. Add tests for:

- nil maps and the all-absent zero `Config`;
- each allowed `LogLevel`, plus invalid effective levels, with `errors.Is` and the zero `Config` on error;
- an invalid lower-layer level overridden by a valid higher-layer value;
- explicit `false`, `0`, and `""` in the highest-precedence layer;
- wrong-type values for each expected type, including a wrong-typed lower-layer value overridden by a correctly typed higher-layer value, and a present nil value;
- unknown keys in each layer (including case/spelling variants), checking panic diagnostics identify key and layer;
- input maps remaining unchanged.

Finally, check the required commands `go test ./...`, `go build ./...`, and `go vet ./...`. The repository currently has no root `go.mod` visible in the supplied tree, so establish or confirm the module setup from the implementation harness before treating command execution as meaningful; the package and implementation file nevertheless belong at the root as specified by the shipped test layout.
