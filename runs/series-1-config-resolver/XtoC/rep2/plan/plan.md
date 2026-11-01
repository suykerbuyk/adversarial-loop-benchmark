# Implementation plan: layered configuration resolver

## Public API and package location

Implement package `config` in the `worktree` module root, in `worktree/config.go`. The public API is fixed by the spec:

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

Define `ErrInvalidLogLevel` as a package-level sentinel using `errors.New`. Document `Resolve` to state that nil maps are empty layers, inputs are only read and never mutated or retained, and wrong-typed or unknown-key entries panic. No file, environment, or command-line access is performed.

## Internal data model and resolution architecture

Use one ordered slice of named layers, constructed once in `Resolve`, in ascending precedence:

```go
[]namedLayer{
    {name: "defaults", values: defaults},
    {name: "file", values: file},
    {name: "env", values: env},
    {name: "flags", values: flags},
}
```

Represent `namedLayer` internally with a name and `map[string]any`. Use one field-descriptor table keyed by the legal public key. Each descriptor has an explicit typed setter closure accepting `*Config`, the value, layer name, and key; the closure checks the exact required Go type and sets the corresponding `Config` field. Use no reflection. The same descriptor table defines the allowed-key set, so key legality and resolution cannot drift apart. Adding a field requires a `Config` member and one descriptor entry; adding a layer requires one ordered slice entry alongside the fixed API wrapper.

Initialize a local `Config{}` and process every layer from lowest to highest. For each map entry, first find its descriptor. An unknown key panics with a message identifying the layer and key. For a known key, invoke the descriptor setter; a type mismatch panics with a diagnostic such as `config: layer %q key %q: want int, got %T`. Process all entries in every layer even if a higher layer will later override them. Thus every present wrong-typed value panics, including one in an overridden lower layer. Presence is established only by map membership; nil values and zero values are still present. Nil maps simply have no entries.

After all layers are applied, validate only the effective `LogLevel`. Accept exactly `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`; comparisons are case-sensitive and do not trim whitespace. If invalid, return `Config{}` and `fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)`. This preserves `errors.Is` matching and includes the offending value. Values overridden by a higher layer are not subject to LogLevel validation, although their types and keys are still checked during application.

## How the rules are met

1. **Precedence:** Ascending application means the highest-precedence layer that presents a key makes the final assignment.
2. **Presence and zero values:** Map lookup uses the key-presence bit, never truthiness. `false`, integer `0`, and empty string overwrite lower values.
3. **Absence:** Omitted keys do not assign; values flow through. Fields omitted by all layers retain Go zero values.
4. **Exact types:** Descriptors accept only `bool`, `int`, and `string` for their respective keys. No coercion occurs. A present nil value, `int64`, `float64`, `uint`, or a named type such as `type Level string` fails the exact assertion and panics.
5. **Unknown keys:** The only legal keys are the four descriptor keys. Any other key in any non-nil layer panics, with the layer and key in its diagnostic.
6. **Panic coverage:** Key and type checks run across every layer before effective-value validation, even for values subsequently overridden.
7. **LogLevel validation:** Validate once on the final effective value. Legal values are exact and case-sensitive; invalid overridden values have no validation effect. An explicit empty string is legal.
8. **Other fields:** `Verbose`, `MaxRetries`, and `TimeoutSec` have no semantic or range validation.
9. **Input ownership:** The resolver reads maps only and retains no references after returning.

## Test approach

Keep the shipped eight public acceptance tests in `worktree/resolve_public_test.go` and run them from the module root with `go test ./...`. Add focused package tests for:

- every field omitted yields `Config{}`;
- each legal LogLevel succeeds, and an invalid lower-layer value overridden by a valid higher layer succeeds;
- invalid effective values include the offending value in the error and satisfy `errors.Is(err, ErrInvalidLogLevel)`, including uppercase and whitespace variants;
- each key with a wrong basic type panics, and a lower bad type still panics when overridden by flags;
- nil, `int64`, `float64`, `uint`, and named scalar values panic rather than being treated as absent or coerced;
- an unknown key panics in each of defaults, file, env, and flags, identifying the layer and key;
- nil maps behave as empty layers; map inputs remain unchanged;
- arbitrary integer values in the two unvalidated integer fields are retained.

Keep tests independent of map iteration order; test each unknown-key layer in a separate invocation. After implementation, verify from `worktree` using `go test ./...`, `go build`, and `go vet`.
