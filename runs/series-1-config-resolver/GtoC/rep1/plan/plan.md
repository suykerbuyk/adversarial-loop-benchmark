# Implementation plan: layered configuration resolver

Package `config` (module `config`, Go 1.25) lives in the worktree root next to the shipped `resolve_public_test.go`. Implementation only; this document does not change code.

No prior adversary findings exist at `plan/plan_findings.md`, so there is no rebuttal file.

## Public API

Fixed signatures, exported from package `config`:

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

`ErrInvalidLogLevel` is a sentinel created with `errors.New`. Invalid effective log levels are returned with `fmt.Errorf("...: %w", ErrInvalidLogLevel)` so `errors.Is(err, ErrInvalidLogLevel)` is true. The sentinel text is stable and documents the legal set.

`Resolve` does not read the environment, files, or the command line. Layer names are precedence labels only. Callers own the maps; `Resolve` does not mutate them.

Panic contract (programmer error, not a returned error):

- A present key is not one of `"Verbose"`, `"MaxRetries"`, `"LogLevel"`, `"TimeoutSec"`.
- A present value’s dynamic type is wrong for that key (see type table below).
- A numeric value that is not exactly `int` (for example `int64`, `int32`, `float64`, `uint`) is the wrong type and panics. JSON-decoded numbers are not accepted; the caller must present `int`.

Nil maps are absent layers (every field absent). A nil map is not an error.

## Internal data model

Two small unexported types, sized so a later field or layer is one registration, not a rewrite of `Resolve`.

### Field descriptors

```go
type fieldKind int // kindBool, kindInt, kindString

type fieldDesc struct {
    key  string
    kind fieldKind
    set  func(*Config, any) // type already checked
}
```

A package-level slice, declaration order matching `Config`:

| key | kind | Config field | accepted dynamic type |
|---|---|---|---|
| `Verbose` | bool | `Verbose` | `bool` |
| `MaxRetries` | int | `MaxRetries` | `int` |
| `LogLevel` | string | `LogLevel` | `string` |
| `TimeoutSec` | int | `TimeoutSec` | `int` |

Adding a field means appending a descriptor and a `Config` field. Lookup is a linear scan of this slice (four entries). Unknown keys panic with the key name.

### Layers

`Resolve` does not store layers in a new type. It walks a fixed local slice:

```go
layers := []map[string]any{defaults, file, env, flags}
```

Index 0 is lowest precedence; the last index is highest. Adding a layer later is inserting a parameter and one slice element, then applying descriptors in the same loop. Do not hard-code four separate merge blocks.

No presence bitset is required. Effective state is the `Config` value itself, which starts at Go zero values. A field is written only when some layer presents it. Absence leaves the current value (zero, or a lower layer’s value) untouched.

## Algorithm

```
cfg := Config{}                          // all zeros; covers "no layer presents a field"
for each layer from lowest to highest precedence:
    if layer == nil: continue
    for each key, value in layer:
        desc := lookup(key)              // panic if unknown key
        assertType(desc.kind, value)     // panic if wrong dynamic type
        desc.set(&cfg, value)            // including false, 0, ""
validate(cfg.LogLevel)                   // only the effective value
return cfg, err
```

Map iteration order does not matter: each key is applied independently, and a duplicate key cannot appear in one map.

### How each stated rule is satisfied

**Present vs absent.** Presence is map membership, not a non-zero check. `v, ok := layer[desc.key]` (or ranging the map and matching descriptors) treats `false`, `0`, and `""` as present. A missing key does not call `set`, so lower values show through. A nil layer has no keys.

**Highest presenting layer wins.** Layers are applied low to high, so a later present value overwrites an earlier one. A higher layer that omits the key does not overwrite. This covers: file over defaults for a non-empty `LogLevel`; flags `Verbose=false` over defaults `true`; env `MaxRetries=0` over defaults `3`; file `LogLevel=""` over defaults `"info"`; flags omitting `LogLevel` while file presents it; all four layers presenting `TimeoutSec` (flags wins).

**No presenter yields zero.** `Config{}` is the start state. Unmentioned fields stay `false`, `0`, `""`, `0`.

**Validation only on effective `LogLevel`.** After the merge, one check:

```go
switch cfg.LogLevel {
case "debug", "info", "warn", "error", "":
    return cfg, nil
default:
    return cfg, fmt.Errorf("config: invalid log level %q: %w", cfg.LogLevel, ErrInvalidLogLevel)
}
```

Legal effective values are exactly `debug`, `info`, `warn`, `error`, and `""`. `"trace"` (and any other string, any case variant such as `"INFO"` or `"Debug"`, whitespace-padded values) is invalid. Overridden presented values are never inspected: a lower layer’s `"trace"` that a higher layer replaces with `"info"` does not error; a higher layer’s `"trace"` that replaces a legal lower value does error. `Verbose`, `MaxRetries`, and `TimeoutSec` have no range checks (negative ints and arbitrary bools are accepted).

**Wrong type panics before validation.** Type assertion happens when the key is applied. A wrong-typed value that would have been overridden still panics, because the spec calls any present wrong-typed value a programmer error, not “wrong type on the winning layer only.” Panic text names the key and the observed type (`%T`).

**Unknown key panics.** Same rule: illegal keys are programmer errors on any layer, even if another layer is well-formed.

**Zero-value override is not validation failure.** Effective `LogLevel == ""` is legal and returns `nil` error. Effective `MaxRetries == 0` and `Verbose == false` are legal.

**Partial maps.** A layer may present any subset of the four keys. Other fields are unaffected by that layer.

**Return value on validation failure.** Return the merged `Config` alongside the wrapped sentinel (the public test only checks the error). Callers that check `err` first are safe; the struct is still the merged values, including the illegal level.

## File layout

- `config.go` — `Config`, `ErrInvalidLogLevel`, field descriptors, `Resolve`, type assert, log-level check. Short comments only where the presence-vs-zero rule would otherwise look like a missing check.
- `resolve_public_test.go` — already shipped; do not edit.
- `resolve_test.go` — additional tests (below). Unexported helpers stay testable from `package config`.

No subpackages. No third-party modules.

## Extensibility (years, more layers and fields)

- Fields: one `fieldDesc` plus a `Config` field. Merge and panic paths do not name fields except inside descriptors and setters.
- Layers: one more `Resolve` parameter and one more element in the ordered slice. Precedence stays “later overwrites earlier if present.”
- Per-field validation, if a future field needs it, is a method on `fieldDesc` invoked once after merge on the effective value only, matching the log-level rule. Do not validate intermediate layers.
- Do not reflect over `Config` struct tags for the hot path. Explicit descriptors keep the panic/type contract obvious and avoid accepting encoding-specific numeric types by accident. Reflection would also make “wrong Go type panics” harder to keep exact.

## Test approach

Public suite (`go test ./...`) is the acceptance gate and already encodes the eight spec cases. Do not modify it.

| Public test | Rule locked |
|---|---|
| `TestDefaultsOnly` | only defaults presented; nil higher layers ignored; all four fields copied, including non-zero values |
| `TestHigherNonZeroWins` | file `LogLevel` overrides defaults |
| `TestPresentFalseOverridesTrue` | present `false` overrides `true` |
| `TestPresentZeroIntOverrides` | present `0` overrides `3` |
| `TestPresentEmptyStringOverrides` | present `""` overrides `"info"` and is not an error |
| `TestAbsentPassesThrough` | flags omits `LogLevel`; file value shows through; unrelated flags key does not clear it |
| `TestAllFourLayersPrecedence` | flags beat env, file, and defaults |
| `TestInvalidEffectiveLogLevel` | `"trace"` yields `errors.Is(..., ErrInvalidLogLevel)` |

Additional tests in `resolve_test.go` (implementor-owned, not required by the public file, used to lock the rest of the spec):

1. All layers nil or empty → zero `Config`, nil error (absent everything, including legal empty log level).
2. Each field presented only on a middle layer (file or env), higher layers nil or omitting that key → that value wins; other fields stay zero.
3. Lower layer presents `"trace"`, higher layer presents `"error"` → nil error, effective `"error"` (overridden value not validated).
4. Higher layer presents `"trace"` over a legal lower level → `errors.Is` sentinel; direct equality with the sentinel is not required.
5. Each legal level (`debug`, `info`, `warn`, `error`, `""`) on flags alone → nil error.
6. Case-mismatched level (`"INFO"`) → invalid sentinel.
7. `Verbose`, `MaxRetries`, and `TimeoutSec` presented together with an invalid log level → error is still only the log-level sentinel (other fields not rejected).
8. Wrong dynamic type (`Verbose` as `string`, `MaxRetries` as `int64` or `float64`, `LogLevel` as `int`) → `recover` shows a panic, on a non-winning layer as well as the winning layer.
9. Unknown key → panic.
10. Same field, four different values, winner is flags; a second field presented only on defaults survives (independent merge).

Run `go test ./...`, `go vet ./...`, and `go build ./...` from the worktree. No network, no files, no env inspection in tests.

## Non-goals

- Loading YAML/JSON/env/flags.
- Coercing numeric kinds.
- Validating retry or timeout ranges.
- Mutating input maps or copying them defensively beyond reading keys (maps are not retained after return).
