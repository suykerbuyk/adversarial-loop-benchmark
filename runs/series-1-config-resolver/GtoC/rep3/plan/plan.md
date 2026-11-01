# Implementation plan: layered configuration resolver

Package `config` in `worktree/`. Module path is already `config` (`go 1.25`). Do not implement in this step; this document is the plan only.

No prior adversary findings exist at `plan/plan_findings.md`, so there is no rebuttal file.

## 1. Public API

All of the following is exported from package `config`. Nothing else is exported.

```go
// Config is the effective configuration after layer resolution.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// ErrInvalidLogLevel is the sentinel for an effective LogLevel outside the
// legal set. Resolve wraps it; callers match with errors.Is.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve combines defaults, file, env, and flags (lowest to highest
// precedence) into one Config.
//
// A key present in a layer sets that field, including when the value is
// false, 0, or "". A key absent from a layer leaves the field unchanged.
// A field presented by no layer keeps its Go zero value.
//
// Only the keys "Verbose", "MaxRetries", "LogLevel", and "TimeoutSec" are
// legal. Any other key panics. A present value whose dynamic type is not
// the field's type (bool, int, string, int respectively) panics, including
// when a higher layer later overrides that field.
//
// Validation runs only on the effective LogLevel. Legal values are
// "debug", "info", "warn", "error", and "". Any other effective value
// returns a non-nil Config (the resolved values) and an error for which
// errors.Is(err, ErrInvalidLogLevel) is true. Overridden presented levels
// are not validated. Verbose, MaxRetries, and TimeoutSec are not validated.
//
// A nil map is an empty layer: every field is absent.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Contract details that the signature alone does not show:

- Layer argument order is fixed: `defaults`, `file`, `env`, `flags`. Names denote precedence only. `Resolve` does not read the environment, files, or the command line.
- Presence is map-key presence (`v, ok := m[key]`), never "value is non-zero". `false`, `0`, and `""` override a lower layer. An untyped nil value stored under a legal key is present and has the wrong dynamic type, so it panics.
- On `ErrInvalidLogLevel`, the returned `Config` is the fully resolved value (invalid `LogLevel` included), not `Config{}`. The spec requires `(Config, error)` and does not say to discard the value. Callers that only check the error still behave correctly. Public test 8 discards the config with `_`.
- The error wraps the sentinel so `errors.Is` works: `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)`. The message quotes the effective level and does not include overridden levels.
- Wrong type and unknown key are programmer errors and panic. They are not returned as `error`. Panic values are `error` values from `fmt.Errorf` (recoverable as `error`, no extra exported sentinels). Panic text names the layer and the key and the expected type. It does not print the value (`%v` / `%#v` of a secret or a huge object must not appear).
- Integer fields accept only dynamic type `int`. `int64`, `int32`, `uint`, `float64`, and a named `type MyInt int` all panic. `LogLevel` accepts only dynamic type `string`; a named `type Level string` panics. `Verbose` accepts only dynamic type `bool`.
- Unknown keys are rejected even if every legal field is also present. The unknown-key rule is a reading of "The only legal keys are …" plus the maintainability requirement: silently dropping a typo would let a lower layer show through and look like a successful override. Both panic conditions are stated in the `Resolve` doc comment so the behavior is part of the package contract.

## 2. File layout

| File | Contents |
| --- | --- |
| `doc.go` | Package comment: precedence, presence vs absence, validation scope, panic conditions. |
| `resolve.go` | `Config`, `ErrInvalidLogLevel`, `Resolve`, internal field bindings, layer walk, validation. |
| `resolve_test.go` | Tests beyond the shipped suite (see §6). |
| `resolve_public_test.go` | Already present; do not edit. |

`go.mod` already declares `module config` and `go 1.25`. Leave it unchanged.

## 3. Internal data model

Two small types, both unexported. No interfaces. No reflection on the hot path.

```go
// layer is one precedence step. name is only for panic messages.
type layer struct {
    name   string
    values map[string]any
}

// field binds one legal key to one Config field.
// apply type-checks v with an exact type switch and stores it.
// A default branch panics; there is no conversion.
type field struct {
    key  string
    apply func(c *Config, layerName string, v any)
}
```

The field table is a package-level slice in stable declaration order, matching `Config` field order:

```go
var fields = []field{
    {key: "Verbose", apply: applyBool((*Config).setVerbose)},
    {key: "MaxRetries", apply: applyInt((*Config).setMaxRetries)},
    {key: "LogLevel", apply: applyString((*Config).setLogLevel)},
    {key: "TimeoutSec", apply: applyInt((*Config).setTimeoutSec)},
}

var knownKeys = map[string]struct{}{ /* filled by init from fields */ }
```

`init` builds `knownKeys` from `fields` and panics if a key is duplicated. That keeps "add a field" to one slice entry plus one setter. Lookup of unknown keys uses `knownKeys`. Application walks `fields` in slice order and looks each key up in the layer map, so behavior does not depend on map iteration order.

Setters are tiny methods so `apply` closures stay typed:

```go
func (c *Config) setVerbose(v bool)       { c.Verbose = v }
func (c *Config) setMaxRetries(v int)     { c.MaxRetries = v }
func (c *Config) setLogLevel(v string)    { c.LogLevel = v }
func (c *Config) setTimeoutSec(v int)     { c.TimeoutSec = v }
```

`applyBool` / `applyInt` / `applyString` each do one type switch. The failure arm panics with an `error`:

```text
config: layer "<name>": key "<key>": value has type <T>, want <want>
```

`<T>` is `fmt.Sprintf("%T", v)` (the type only). `<want>` is `bool`, `int`, or `string`.

Legal log levels live in one set used only after resolution:

```go
var legalLogLevels = map[string]struct{}{
    "":      {},
    "debug": {},
    "info":  {},
    "warn":  {},
    "error": {},
}
```

Comparison is exact and case-sensitive. `"INFO"`, `"warning"`, `" info"`, and `"trace"` are illegal.

## 4. Resolve algorithm

```text
func Resolve(defaults, file, env, flags map[string]any) (Config, error):
    layers := []layer{
        {"defaults", defaults},
        {"file", file},
        {"env", env},
        {"flags", flags},
    }
    var c Config   // all zeros; this is the result if every field is absent
    for _, l := range layers:
        l.apply(&c)
    if err := validate(&c); err != nil:
        return c, err
    return c, nil
```

`layer.apply`:

1. If `values` is nil, return. A nil map is an empty layer.
2. Collect every key not in `knownKeys`. If the list is non-empty, sort it and panic with all of them listed (deterministic; map iteration order must not change the message). Example: `config: layer "env": unknown key(s) "bogus", "zeta"`. Cap the echoed list (at most 5 keys, each key at most 32 runes, trailing `...` on truncation, `and N more` for the rest) so a hostile or accidental huge key cannot blow up a log line. Document the cap in the `Resolve` comment.
3. For each `f` in `fields` (declaration order): if `v, ok := values[f.key]; ok`, call `f.apply(&c, l.name, v)`. `ok == false` means absent: do not call `apply`, do not write the field. A present zero value has `ok == true` and is written.

This is a last-write-wins walk from lowest precedence to highest. Equivalent to "among layers that present the field, the highest wins", and it naturally leaves an unpresented field at the Go zero value.

`validate`:

```text
if _, ok := legalLogLevels[c.LogLevel]; !ok:
    return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
return nil
```

Only `LogLevel` is checked, and only the value sitting on `c` after the walk. A lower layer's `"trace"` that was overwritten by `"info"` is never seen here. A lower layer's `"trace"` that was overwritten by `""` is legal. An effective `"trace"` returns the resolved `Config` (with `LogLevel == "trace"`) plus the wrapped sentinel.

No `errors.Join`. One validation rule means one error value, and `errors.Unwrap(err) == ErrInvalidLogLevel`.

## 5. How each stated rule is satisfied

| Rule | How |
| --- | --- |
| Four layers, higher overrides lower | Walk order defaults → file → env → flags; each present key overwrites. |
| Present zero values override (`false`, `0`, `""`) | Presence is the comma-ok idiom, not a non-zero check. The typed setter stores the zero. |
| Absent field has no effect | Missing key skips `apply`. Other keys in the same map still apply (public test 6: flags sets `Verbose` and leaves `LogLevel` from file). |
| No layer presents a field → Go zero | `var c Config` starts at zeros; untouched fields stay there. |
| Nil layer map | Treated as empty; same as a map with no keys. Public tests pass nil for unused layers. |
| Only four legal keys | Any other key panics before any field of that layer is applied. |
| Wrong dynamic type panics | Exact type switch in `apply*`. Checked for every present value, including values a higher layer overrides, because the check runs when the layer is applied, before later layers run. |
| Validation only of effective `LogLevel` | `validate` runs once, after the walk, and reads `c.LogLevel` only. |
| Legal levels `debug`, `info`, `warn`, `error`, `""` | `legalLogLevels` set; empty string is a member, so public test 5 returns nil error. |
| Anything else → `errors.Is(err, ErrInvalidLogLevel)` | `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)`. |
| Other fields not validated | No checks on `Verbose`, `MaxRetries`, or `TimeoutSec` (negative ints are allowed). |
| No I/O | Function body only reads the four maps. |
| `go build`, `go vet`, `go test ./...` | Standard library only. Public tests live in package `config` and call `Resolve` directly. |
| Room for more layers and fields | A new layer is one `layer{name, map}` entry in the slice (call signature is fixed today; internal walk already takes a slice). A new field is one `field` entry, one setter, and, if it needs validation, one extra check in `validate` that returns its own sentinel without `Join` until a second failure mode exists. |

Unknown-key panic is stricter than a literal reading that only mandates a panic for a wrong type. It is deliberate and documented (§1). A future reviewer who wants silent ignore would have to change the doc comment and the tests together.

## 6. Test approach

Shipped tests in `resolve_public_test.go` cover the eight acceptance cases. Do not modify that file. `resolve_test.go` adds the gaps the public suite does not pin down.

Public cases, already shipped, must keep passing:

1. Defaults only → effective equals those values.
2. Non-empty file `LogLevel` beats non-empty default.
3. `flags` `Verbose=false` beats default `true`.
4. `env` `MaxRetries=0` beats default `3`.
5. `file` `LogLevel=""` beats default `"info"`, and the error is nil.
6. Field absent from flags, present in file, shows through even when flags sets a different field.
7. Same field in all four layers → flags wins.
8. Effective `"trace"` → `errors.Is(err, ErrInvalidLogLevel)`.

Additional tests:

- **Zeros when nothing is presented.** `Resolve(nil, nil, nil, nil)` returns `Config{}` and a nil error (`""` is a legal level).
- **Each legal level.** Effective `"debug"`, `"info"`, `"warn"`, `"error"`, `""` from flags alone return nil error.
- **Invalid level is case-sensitive and exact.** `"INFO"`, `"Warning"`, `" info"`, `"error "` each return the sentinel.
- **Overridden invalid level is not validated.** defaults `LogLevel="trace"`, flags `LogLevel="info"` returns nil error and `"info"`. defaults `"trace"`, file `""` returns nil error and `""`.
- **Invalid effective still returns the resolved config.** flags `LogLevel="trace"`, defaults `MaxRetries=7` → error matches sentinel, `MaxRetries==7`, `LogLevel=="trace"`.
- **Per-field precedence and independence.** file sets `Verbose=false` and `TimeoutSec=9`; env sets `MaxRetries=0`; flags sets `LogLevel="error"`; defaults set the other starting values. Each field comes from the highest layer that presented it. A sibling key in a higher layer does not clear an absent field.
- **Empty string and zero int on the highest layer.** flags `LogLevel=""` over file `"debug"`; flags `TimeoutSec=0` over env `10`.
- **Wrong type panics.** Table: `Verbose` with `"true"`, `1`, `nil`; `MaxRetries` with `int64(1)`, `float64(1)`, `uint(1)`; `LogLevel` with `[]byte("info")` and a named string type; `TimeoutSec` with `int32(1)`. Each must panic. Assert the recovered value is an `error` and that the message contains the layer name and the key, and does not contain a distinctive payload planted in the value (for example a string field is not the integer case; use a custom type whose `%T` is short and whose string form contains `secret=`).
- **Wrong type in a lower layer still panics** when a higher layer presents a well-typed value for the same key.
- **Unknown key panics.** `"loglevel"`, `"verbose"`, `""`, and a key with a space. Message contains the layer name. Recovered value is an `error`.
- **Unknown key listed deterministically.** One layer with two unknown keys, invoked many times, always panics with the same message, keys in sorted order.
- **Unknown key is not applied past.** A layer that has a legal key and an unknown key panics; the test only checks the panic (partial application of that layer is unspecified and must not be relied on). Lower layers already applied stay irrelevant because the panic aborts `Resolve`.
- **Nil and empty maps.** `Resolve(map[string]any{}, nil, map[string]any{}, nil)` equals all-zero success.
- **Binding table covers `Config`.** A test walks `reflect.TypeOf(Config{})` fields and checks each exported field name is a key in `fields`, and `len(fields)` equals the number of exported fields. This fails when someone adds a struct field and forgets the table.
- **Duplicate field registration.** Call the unexported index helper with a duplicated slice and expect a panic that names the key. If indexing stays in `init` only, extract the helper so the test can call it.

Panic tests use `defer` + `recover` and `t.Fatalf` if recover returns nil. They do not use `t.Parallel` around shared mutation; the package has no mutable global after `init`.

Commands the implementor runs from `worktree/`:

```text
go test ./...
go vet ./...
go build ./...
```

No race-sensitive state, but `go test -race ./...` should still pass and is worth one run.

## 7. Out of scope

- Reading environment variables, files, or `flag` values.
- Exported sentinels other than `ErrInvalidLogLevel`.
- Converting numeric types (`int64` → `int`) or accepting `json.Number`.
- Validating `MaxRetries` or `TimeoutSec` ranges.
- Copying or locking the input maps. `Resolve` does not retain the maps after return, so the caller may reuse them. Document that the maps must not be mutated concurrently with `Resolve`.
