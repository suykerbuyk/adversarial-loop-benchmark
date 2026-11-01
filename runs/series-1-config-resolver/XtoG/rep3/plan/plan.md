# Implementation plan: layered configuration resolver

## Public API

Implement package `config` in the module root, with the fixed API:

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

`ErrInvalidLogLevel` is the sentinel for an invalid effective level. Return an error wrapping it with the field name and effective value, so `errors.Is` works and diagnostics identify the problem.

## Internal model and resolution

Represent the four maps as an ordered slice from lowest to highest precedence. Define one private descriptor per supported field containing its key and a typed apply function. The descriptors form the shared registry for legal field names and value application; keep validation of `LogLevel` on the final resolved `Config`, after all layers are applied.

Each apply function looks up its key using the map's two-result lookup. If absent it leaves the destination unchanged. If present, it asserts the exact required Go type (`bool`, `int`, or `string`) and panics on mismatch. Use a descriptive panic that includes the key, expected type, and actual dynamic type. Assign the asserted value even when it is `false`, `0`, or `""`; presence, not zero-value comparison, determines override.

For each descriptor, apply it across all layers from defaults through flags, so the last layer containing that key wins. Initialize `Config` with its Go zero value; keys absent from every layer retain that value. Ignore keys outside the supported descriptor registry: the spec enumerates legal keys but does not require rejection, and does not define a strict-key mode.

After resolution, accept effective `LogLevel` values `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`. Return the resolved config plus a wrapped `ErrInvalidLogLevel` for every other effective value. A lower layer's invalid value is harmless when a higher layer overrides it, because validation happens only after resolution. Do not validate the other fields.

## Rule coverage

- **Precedence:** ordered layer application makes flags win over env, env over file, and file over defaults for each field independently.
- **Presence and zero values:** the map lookup's `ok` bit distinguishes absence from an explicitly supplied zero value; assign whenever present.
- **No presented value:** zero-initialized fields remain at their Go zero value.
- **Types:** exact typed assertions enforce the specified Go types; wrong types panic rather than return a validation error.
- **LogLevel:** validate only the final effective value, permit the four named levels and explicit empty string, and wrap the sentinel for all other values.
- **Growth:** the field registry centralizes field keys and typed application, limiting future additions to one descriptor plus the corresponding public `Config` field and any validation rule.
- **Scope:** operate solely on caller-provided maps; do not read environment, files, or command-line state.

## Test approach

Run the shipped acceptance suite with `go test ./...` from the module root. Its eight tests cover defaults, higher-layer override, explicit false/zero/empty overrides, pass-through when a higher layer omits a field, four-layer precedence, and `errors.Is` for invalid effective LogLevel.

Add focused package tests for requirements not directly covered by the public suite: all-absent input yields the zero `Config`; each field's wrong type panics; each legal LogLevel succeeds; an invalid lower-layer LogLevel overridden by a valid higher layer succeeds; unknown keys do not affect known fields; and an invalid effective level's returned error includes useful field/value context while matching `ErrInvalidLogLevel`. Also run `go build ./...` and `go vet ./...` to check the stated build and vet requirements.
