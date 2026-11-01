# Implementation plan: layered configuration resolver

## Public API and package location

Implement package `config` with the exact exported API required by the spec:

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

Define `ErrInvalidLogLevel` as a sentinel error. Put the implementation in a Go source file in `worktree/`, in the same package directory as `worktree/go.mod` and the shipped package test. This makes the public API compile as module `config` under `go test ./...` from that directory. Keep the four-argument `Resolve` signature unchanged; it is fixed by the spec.

On validation failure, return the fully resolved `Config` together with an error wrapping `ErrInvalidLogLevel`. The returned config is useful for diagnosis, while `errors.Is` remains the supported sentinel check.

## Internal data model

Represent precedence as an ordered slice of named layer values, in low-to-high order: defaults, file, env, flags. Resolution scans it from high to low. This keeps the precedence algorithm independent of the four parameter names while respecting the fixed public function signature.

Use one field descriptor table as the registration point for the supported fields. Each descriptor contains the map key, expected dynamic Go type, a typed assignment function into `*Config`, and an optional validator for the resolved field. The four entries are:

| Key | Exact dynamic type | Config field | Effective validation |
| --- | --- | --- | --- |
| `Verbose` | `bool` | `Config.Verbose` | none |
| `MaxRetries` | `int` | `Config.MaxRetries` | none |
| `LogLevel` | `string` | `Config.LogLevel` | `"debug"`, `"info"`, `"warn"`, `"error"`, or `""` |
| `TimeoutSec` | `int` | `Config.TimeoutSec` | none |

For each descriptor, first walk every layer and check every present occurrence's exact dynamic type. Panic on a mismatch, even when a higher-precedence layer also presents that key. In particular, `int64`, `float64`, and `json.Number` are invalid for the integer fields. Then find the highest-precedence layer containing the key and assign its already-checked value. Use map lookup's `(value, ok)` result so explicit `false`, `0`, and `""` remain present values. Initialize the result to `Config{}` so a key absent everywhere keeps its Go zero value.

Unknown keys are outside the legal key set, so panic when encountered. This makes misspellings visible rather than silently treating them as absent fields. Check unknown keys independently of descriptor resolution.

After all fields are resolved, run descriptor validators only on the effective values. The LogLevel validator accepts exactly the five values listed above. An invalid effective value returns the resolved config and a wrapped `ErrInvalidLogLevel`; invalid lower-precedence values hidden by a valid higher value are not validated. No validation is applied to the other fields.

## How the rules are satisfied

- **Layer precedence:** scan flags, env, file, defaults and assign the first present value.
- **Presence versus zero:** rely exclusively on map membership, never on a zero-value comparison.
- **Absent fields:** leave the initialized result field untouched when no layer contains its key.
- **Wrong-type programmer errors:** check all present occurrences before choosing a winner, and panic for any mismatch.
- **Effective-only LogLevel validation:** validate once after resolution, so shadowed values cannot fail allowed-value validation.
- **Exact accepted types:** use `bool`, `int`, and `string`; do not coerce aliases or neighboring numeric representations.
- **No I/O:** consume only the four maps passed to `Resolve`.
- **Extensibility:** keep field metadata, assignment, and optional validation together in the descriptor table; keep precedence traversal over an ordered layer slice. The public signature remains the fixed four-layer API required by this task; any future public expansion would require an explicit API revision.

## Test approach

Use the shipped acceptance suite at `instrument/resolve_public_test.go` (included in the module's public test setup) to cover defaults, precedence, explicit zero values, absent pass-through, all-layer selection, and `errors.Is` for an invalid effective level. Add focused package tests for the omitted boundaries:

- wrong types for every field panic;
- wrong types in a shadowed lower layer panic, including `int64`, `float64`, and `json.Number` for integer fields;
- unknown keys panic;
- each legal effective LogLevel succeeds, and an invalid shadowed LogLevel is ignored;
- a field absent from all layers remains zero;
- invalid effective LogLevel returns a config and an error matching `ErrInvalidLogLevel`.

Verify the package with `go test ./...`, `go build`, and `go vet` from `worktree/`, as required by the spec.
