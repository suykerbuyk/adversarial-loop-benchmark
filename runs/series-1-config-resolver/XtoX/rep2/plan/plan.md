# Implementation Plan: Layered Configuration Resolver

## Public API

Implement package `config` with the API fixed by the specification:

```go
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

var ErrInvalidLogLevel = errors.New("invalid log level")

func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

`Resolve` will use the maps in the given precedence order and return a zero or partially populated `Config` together with a validation error when the effective log level is invalid. The sentinel is stable and callers can check it with `errors.Is`.

## Internal data model and resolution

Represent the four inputs as an ordered slice of named layer maps, lowest precedence first (`defaults`, `file`, `env`, `flags`). For each field, scan that slice from highest precedence to lowest and use Go's two-result map lookup (`value, present := layer[key]`). Presence, rather than whether the value is nonzero, determines whether the layer sets the field. This preserves explicit `false`, `0`, and `""` values. If no layer contains the field, leave its corresponding `Config` member at its Go zero value.

Keep the resolved public `Config` as the effective data model; do not introduce a second merge representation or infer presence from zero values. Use small typed resolution helpers (or equivalent explicit field logic) that assert the required Go type for a present value. A present value of the wrong type panics as required. Only the four documented keys are inputs to this API; handling of keys outside that contract is not used to infer any field.

## Rule-by-rule behavior

1. **Layer ordering:** search flags, env, file, defaults when resolving each field, so the highest-precedence layer that presents it wins.
2. **Presence and zero values:** use map membership independently of value content. Explicit false, integer zero, and empty string overwrite lower values.
3. **Absence:** continue to lower layers when a key is missing. If missing in every layer, retain the field's Go zero value.
4. **Types:** assert `bool` for `Verbose`, `int` for `MaxRetries` and `TimeoutSec`, and `string` for `LogLevel`; a mismatched present value panics.
5. **Validation timing:** merge all fields before validating. Validate only the resulting `Config.LogLevel`, so an invalid value in a lower layer does not matter if a higher layer overrides it.
6. **Valid log levels:** accept exactly `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`. Any other effective string returns an error wrapping `ErrInvalidLogLevel` (for example with `%w`), ensuring `errors.Is` succeeds. The other fields have no validation.

## Test approach

Run the shipped public tests with `go test ./...`; they cover defaults, nonzero precedence, explicit false/zero/empty overrides, absent-field pass-through, all-layer precedence, and `errors.Is` on an invalid effective log level. Add focused package tests for the full contract: all-zero/no-input behavior; each field's type mismatch panics; every allowed and disallowed log level; invalid lower-precedence levels overridden by valid higher-precedence levels; and explicit empty log level overriding an invalid lower value. Also verify the sentinel with `errors.Is` rather than comparing error strings. Finish by checking `go build ./...` and `go vet ./...`.

## Implementation sequence

1. Add the `config` package files with the declared API and sentinel.
2. Implement presence-aware typed resolution for each field across the ordered layers.
3. Validate only the final effective log level and wrap the sentinel on failure.
4. Add or run the tests listed above and the required build/vet checks.
