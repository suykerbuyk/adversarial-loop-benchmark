# Implementation plan: layered configuration resolver

## Public API

Implement package `config` in the module root (`module config`, Go 1.25) with the exact public surface from the specification:

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

`ErrInvalidLogLevel` is a stable sentinel. For an invalid effective level, return a zero `Config` and an error wrapping that sentinel (for example, with the offending level in the message and `%w`) so callers can use `errors.Is`.

## Internal data model and resolution

Keep the public `Config` as the resolved typed value. Treat each input map as a sparse layer: map key presence represents explicit presentation, including zero values. Internally, form a fixed ordered sequence of the four layer maps, from lowest to highest precedence: defaults, file, env, flags. Do not use map iteration order to determine precedence.

For each supported field, perform a typed lookup in each layer using the two-result map lookup (`value, present := layer[key]`). When present, assert the exact required Go type and assign it to the corresponding `Config` field; when absent, leave the field untouched. Walking layers low-to-high means the last present value wins while explicit `false`, `0`, and `""` remain assignments. Start from `Config{}`, which naturally supplies Go zero values if no layer presents a field. A present value of the wrong Go type is a programmer error and must panic; use explicit type assertions, with a descriptive panic message if desired.

Use small typed field resolvers or similarly explicit field-specific code so the supported key-to-type mapping is visible and type-safe. Do not add reflection or a generic untyped merge that could obscure presence and type errors. Keep precedence handling centralized in the layer sequence so additional layers can be appended in order as the package grows. When fields are added, add their typed resolution and validation policy explicitly.

The contract defines the supported key set as exactly `Verbose`, `MaxRetries`, `LogLevel`, and `TimeoutSec`; behavior for maps containing other keys is outside the specified input domain and does not affect supported fields.

## Rules and how the implementation satisfies them

1. **Layer precedence:** process defaults, file, env, then flags; each later present value overwrites the earlier assignment.
2. **Presence versus zero value:** branch on map membership, never on whether the value equals its type's zero value. This preserves explicit `false`, `0`, and empty strings.
3. **Absent fields:** do not assign on a missing key. Lower-layer values remain; if all layers omit a key, the field remains its Go zero value.
4. **Types:** use `bool` for `Verbose`, `int` for `MaxRetries` and `TimeoutSec`, and `string` for `LogLevel`. A present value with a different dynamic Go type panics.
5. **Effective-only validation:** resolve all fields first, then validate only `cfg.LogLevel`. Accept `debug`, `info`, `warn`, `error`, and `""`. Return a wrapped `ErrInvalidLogLevel` for any other effective string. Since overwritten values are not checked during merging, an invalid lower-layer value overridden by a valid higher layer cannot fail resolution.
6. **No unrelated validation or I/O:** do not validate numeric/boolean fields, read environment variables, files, or command-line arguments, or infer missing values from external sources.

## Test approach

Use the shipped public tests in `instrument/resolve_public_test.go` as acceptance coverage for defaults-only resolution, higher-layer wins, explicit false/zero/empty overrides, absent pass-through, all-four-layer precedence, and `errors.Is` behavior for an invalid effective level. The package implementation should be placed where `go test ./...` in the module can discover it.

Add focused package tests alongside the implementation for uncovered contract edges: all-absent input yields `Config{}`; each supported key panics on a wrong present Go type; an invalid lower-precedence `LogLevel` overridden by a valid higher-precedence value succeeds; invalid effective values return an error matching the sentinel; and each accepted log level, including empty, succeeds. Run `go test ./...`, `go build ./...`, and `go vet ./...` to verify the deliverables.
