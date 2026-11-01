# Implementation plan: layered configuration resolver

## Public API

Implement package `config` in the module's package directory, exposing the API exactly as specified:

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

Define `ErrInvalidLogLevel` as a stable package-level sentinel (for example with `errors.New`). Invalid effective values should return a descriptive wrapped error using `%w`, so `errors.Is` recognizes the sentinel. The function does not read environment variables, files, or arguments; its four maps are the complete input.

## Internal data model and resolution

Use a small private typed intermediate representation containing a `Config` value plus presence bits for each field, or resolve each typed field directly by scanning its corresponding key through the four input maps. Presence must be tracked independently of value so that `false`, integer zero, and empty string are distinguishable from absence. Keep the layer order explicit: defaults, file, env, flags, from lowest to highest precedence.

For each legal key, use the two-result map lookup, validate every presented value against the exact Go type specified for the field (`bool`, `int`, or `string`), and select the highest-precedence value. A failed type assertion panics as required for a wrong-type programmer error, even if that value is overridden in a higher layer. If no layer contains the key, leave the field at its Go zero value. Nil maps naturally behave as maps with no entries. Unknown keys are outside the legal input domain; resolution only reads the four specified keys.

This can be implemented by traversing layers from lowest to highest, assigning a field only when its key is present, while validating each presented value. This keeps precedence handling simple and preserves explicit zero values. The typed intermediate representation is optional; if used, its presence bits must be separate from its values.

## Rules and how the implementation satisfies them

1. **Precedence:** each field is chosen independently, with `flags` above `env`, `env` above `file`, and `file` above `defaults`.
2. **Present zero values:** use the map lookup `value, ok := layer[key]`; if `ok`, assign the value even when it is `false`, `0`, or `""`.
3. **Absent values:** absence does not assign or erase a lower layer's value. If all four lookups are absent, the corresponding field remains its Go zero value.
4. **Type correctness:** assert present values to the field's required exact Go type. On a mismatch, panic rather than converting or returning a validation error.
5. **Log level validation:** validate only the final `Config.LogLevel`, after all four layers have been resolved. Accept exactly `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`. An invalid effective level returns a wrapped `ErrInvalidLogLevel`. Do not reject an invalid lower value if a valid or empty higher-precedence value wins.
6. **Other fields:** do not impose value-range validation on `Verbose`, `MaxRetries`, or `TimeoutSec`.

## Test approach

Use the shipped public tests as acceptance coverage for defaults-only resolution, nonempty override, explicit `false`, explicit integer zero, explicit empty log level, absent-field pass-through, four-layer precedence, and `errors.Is` for an invalid effective log level.

Add focused package tests for nil/all-empty layers producing a zero `Config`; each legal log level including empty; invalid lower log levels masked by higher valid and higher empty values; a wrong Go type for each field panicking; and independent precedence across multiple fields. These tests should verify presence semantics rather than only nonzero overrides. Confirm the package builds with `go build`, is clean under `go vet`, and passes `go test ./...` from the module root.

## Scope and maintainability

Keep the implementation in the `config` package, use only the Go standard library, and keep resolution logic centralized so future fields and layers can be added without duplicating inconsistent precedence or validation rules. Do not add file, environment, or command-line loading; those are explicitly out of scope.
