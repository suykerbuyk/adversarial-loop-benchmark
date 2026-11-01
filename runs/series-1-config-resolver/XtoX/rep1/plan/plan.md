# Implementation plan: layered configuration resolver

## Public API

Implement package `config` with the API fixed by the spec:

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

Define `ErrInvalidLogLevel` as a stable package-level sentinel using `errors.New`. If the effective
log level is invalid, return an error that wraps this sentinel (for example with `%w`) so callers
can use `errors.Is`. Return the resolved config alongside that error; callers should rely on the
error to determine validity.

## Internal data model and resolution

Treat each input as an optional layer: nil maps are equivalent to maps with no presented fields.
For maintainability as fields and layers grow, describe the fields once in an internal ordered field
specification containing the key, expected Go type, and assignment behavior (or use small typed
resolution helpers where Go's type system makes a single generic table awkward). Do not infer
presence from a value's zero-ness.

Resolve by applying layers in ascending precedence: defaults, file, env, then flags. For each
recognized field, check map membership with the two-result lookup. When present, assert its required
Go type and assign it to the corresponding `Config` field. A failed assertion panics, as required
for programmer errors. An absent key does nothing, preserving any lower-layer value; if no layer
presents the field, the initial zero-valued `Config` remains in effect. The contract restricts keys
to the four listed names.

After all layers have been applied, validate only `Config.LogLevel`. Accept exactly `"debug"`,
`"info"`, `"warn"`, `"error"`, and `""`. Any other effective value returns an error wrapping
`ErrInvalidLogLevel`. Do not validate values from individual layers before overrides, or validate
`Verbose`, `MaxRetries`, or `TimeoutSec`.

## Rule-to-design mapping

| Requirement | Implementation behavior |
| --- | --- |
| Four ordered layers; higher overrides lower | Apply maps in the fixed order defaults → file → env → flags; each present value replaces the existing field. |
| Zero values are explicit values | Use map membership, not truthiness or zero-value checks; assign `false`, `0`, and `""` when present. |
| Missing key has no effect | Skip absent keys so lower-layer values pass through. |
| No layer presents a field | Start with `Config{}` and leave the field untouched, yielding its Go zero value. |
| Wrong Go type for a present field panics | Use a checked type assertion for every present recognized key and panic on failure. |
| Validate only the final log level | Perform the validation once after all four overlays. |
| Empty log level is valid | Include `""` in the accepted set. |
| Invalid effective log level supports `errors.Is` | Wrap the exported sentinel with `%w` in the returned error. |
| In-memory only | Consume only the supplied maps; no environment, filesystem, or command-line access. |

## Test approach

Use the shipped public tests as acceptance coverage for defaults-only resolution, higher-layer
precedence, explicit false/zero/empty overrides, pass-through for absent fields, all-four-layer
precedence, and `errors.Is` behavior for an invalid effective log level.

Add focused package tests for the remaining contract boundaries: nil/empty maps producing zero
values; wrong-type values for each of the four keys panicking; invalid lower-layer log levels being
accepted when overridden by a valid higher layer; and each legal log level, including the empty
string. Keep tests table-driven where it improves clarity. Run `go test ./...`, `go build ./...`,
and `go vet ./...` from the module root (`worktree`) as the completion checks.
