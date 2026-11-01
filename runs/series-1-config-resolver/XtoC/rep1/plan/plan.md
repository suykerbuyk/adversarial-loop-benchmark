# Implementation plan: layered configuration resolver

## Package and public API

Implement the package at the module root as `package config` (for example, `config.go`), alongside the shipped root-package tests. The public API is exactly:

```go
func Resolve(defaults, file, env, flags map[string]any) (Config, error)

type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

var ErrInvalidLogLevel error
```

`ErrInvalidLogLevel` is a stable sentinel created with `errors.New`. For invalid effective log levels, return the fully resolved `Config` together with an error wrapping the sentinel and including the offending value with `%q`; callers can use `errors.Is`. This deliberately preserves the resolved data while reporting validation failure.

## Internal data model

Represent layers as one ordered descriptor slice in ascending precedence, each containing its name and input map: defaults, file, env, flags. Represent fields in one descriptor table, with one entry per `Config` field containing its key, expected exact Go type, and an apply function that assigns the typed value to a `*Config`. This table is the sole source for legal keys, type checking, and field assignment. Keep a single case-sensitive set (or a single switch) of allowed log levels: `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`.

Resolution proceeds in two phases:

1. Validate all input maps before choosing winners. For every present key, look it up in the field descriptor table. Panic on an unknown key, or on a known key whose value is not exactly its declared Go type. Check every occurrence in every layer, including overridden values. Nil maps simply contain no entries. Type assertions are exact: nil, named types, `int32`, `int64`, and numeric conversions are not accepted in place of the required `bool`, `int`, or `string`.
2. For each field descriptor, scan layer descriptors from highest to lowest precedence and assign the first present value. Use comma-ok presence detection, never zero-value comparison. If no layer presents the field, leave its `Config` member at its Go zero value. After all assignments, validate only the effective `Config.LogLevel`.

Panic messages identify the layer name, key, expected type, and actual dynamic type (`%T`). Unknown-key panics identify the layer and key. An invalid-level error identifies the effective bad value using `%q`; provenance can be tracked during selection and included if convenient, while validity itself is based only on the effective result.

## Rule coverage

- **Precedence and absence:** the ordered layer table establishes defaults < file < env < flags. Highest-to-lowest lookup selects the highest layer that presents each field; absence falls through independently by field.
- **Presented zero values:** comma-ok lookup treats `false`, `0`, and `""` as present, so they override lower values.
- **No presentation:** untouched config fields remain at Go zero values.
- **Programmer errors:** the descriptor table checks every present key in every map before selection. Wrong types and unknown keys panic, even when the offending entry is overridden. No coercion occurs.
- **Validation:** only the final effective log level is checked, against the exact case-sensitive allowed set. An overridden invalid value is not rejected; an effective invalid value returns an error satisfying `errors.Is(err, ErrInvalidLogLevel)`.
- **Scope:** use only supplied in-memory maps; do not read environment variables, files, or command-line arguments, and do not mutate the maps.
- **Maintainability:** adding a field requires one descriptor entry. A reflection-based consistency test checks that every exported `Config` field corresponds to exactly one descriptor and every descriptor corresponds to a `Config` field.

## Test approach

Retain all eight shipped public acceptance tests. Add focused in-memory package tests covering:

1. Nil/all-absent layers yield zero `Config`; verify absence pass-through and independent per-field precedence.
2. Each of the five legal effective log levels succeeds. Table-test invalid effective levels (including `"INFO"`, `" info"`, and `"info "`) placed at different layer positions; each error must satisfy `errors.Is`. Also test an invalid lower-level value overridden by an explicit empty string, which succeeds.
3. For each field, test exact required types and panic behavior. Include wrong-typed values hidden by a higher-precedence correct value, plus nil, named-type values (such as `type Level string`), `int32`, `int64`, and `float64` where applicable. These must panic rather than convert.
4. Test that any unknown key in any layer panics, including a typo, with layer and key identifiable in the panic message.
5. Reflection checks establish a one-to-one correspondence between `Config` fields and descriptor entries.
6. On invalid effective `LogLevel`, assert that the returned `Config` retains the resolved fields while the error wraps the sentinel.
7. Inputs are not mutated.

Use deterministic tests without external dependencies. Acceptance commands are `go test ./...`, `go build ./...`, and `go vet ./...`; format implementation and test files with `gofmt`.
