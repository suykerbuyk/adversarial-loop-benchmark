# Implementation plan: layered configuration resolver

No prior adversary findings exist at `plan/plan_findings.md`. This plan covers the first implementation pass. Nothing in this document is implemented yet.

Package location: `worktree/` is already `module config` (`go 1.25`). All new code lives in that module so `go test ./...` sees `package config`. The shipped public test file is `instrument/resolve_public_test.go` and is also present as `worktree/resolve_public_test.go`. Implementation files are added beside it. Do not edit the public test.

## Public API

File: `worktree/config.go`, `package config`.

```go
package config

import "errors"

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// the legal values: "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Config is the effective configuration after layer resolution.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// Resolve combines four ordered layers into an effective Config.
// Precedence, low to high: defaults, file, env, flags.
// A nil map is an empty layer (every field absent).
// A key present in a map sets that field, including zero values.
// A key absent from a map leaves the field unchanged.
// A present value of the wrong Go type for its key panics.
// Unknown keys panic.
// Validation runs only on the effective LogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Sentinel identity: `ErrInvalidLogLevel` is a package-level `error` variable created with `errors.New`. Callers (and the public test) use `errors.Is`. Do not wrap a different sentinel. On the invalid-level path, return `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)` so `errors.Is` holds and the bad value is visible. Returning the bare sentinel is also correct for the public test; wrapping is preferred so the error remains diagnosable.

The function signature, type name, field names, and sentinel name are fixed by the spec and the public tests. No other exported names are required. Keep helpers unexported.

## Internal data model

The four layer arguments stay as `map[string]any`. There is no second struct for partial layers. Presence is "key exists in the map" (`v, ok := layer[name]`). A stored zero value (`false`, `0`, `""`) is present. A missing key is absent. A nil map has no keys, so it is an all-absent layer. Do not treat a zero value as absent. Do not allocate a sentinel "unset" wrapper type.

Known fields and their required dynamic types:

| Key | Go type of presented value | Config field |
| --- | --- | --- |
| `Verbose` | `bool` | `Verbose` |
| `MaxRetries` | `int` | `MaxRetries` |
| `LogLevel` | `string` | `LogLevel` |
| `TimeoutSec` | `int` | `TimeoutSec` |

Typed aliases that are not the exact type (`int32`, `int64`, a defined `type Level string`, `*bool`) are the wrong type and panic. Numeric kind-matching is out of scope. JSON-style `float64` is the wrong type and panics. The public tests pass plain `bool`, `int`, and `string` literals, which have those exact types.

Field registry (unexported), one entry per field, so adding a field later is a table change plus a `Config` field:

```go
type fieldKind int

const (
    kindBool fieldKind = iota
    kindInt
    kindString
)

type fieldSpec struct {
    name string
    kind fieldKind
    // apply writes a type-checked value into cfg. Called only when the key is present.
    apply func(cfg *Config, v any)
}

var fields = []fieldSpec{
    {name: "Verbose", kind: kindBool, apply: func(cfg *Config, v any) { cfg.Verbose = v.(bool) }},
    {name: "MaxRetries", kind: kindInt, apply: func(cfg *Config, v any) { cfg.MaxRetries = v.(int) }},
    {name: "LogLevel", kind: kindString, apply: func(cfg *Config, v any) { cfg.LogLevel = v.(string) }},
    {name: "TimeoutSec", kind: kindInt, apply: func(cfg *Config, v any) { cfg.TimeoutSec = v.(int) }},
}
```

`apply` may type-assert after `checkType`, or `checkType` may return the asserted value. Either way the panic happens before the write if the dynamic type is wrong.

Unknown-key set, built once from `fields`, used when scanning each layer. A key that is not in this set panics. The spec says the only legal keys are the four field names.

Layer order is a fixed slice built inside `Resolve`, low precedence first:

```go
layers := []map[string]any{defaults, file, env, flags}
```

Walking low-to-high and overwriting on presence is equivalent to "highest presenting layer wins" and keeps the apply path uniform. Do not special-case which argument is which beyond this order. The names denote precedence only. `Resolve` does not read the environment, files, or the command line.

Legal log levels, unexported:

```go
var legalLogLevels = map[string]struct{}{
    "":      {},
    "debug": {},
    "info":  {},
    "warn":  {},
    "error": {},
}
```

`""` is legal. Comparison is exact and case-sensitive. `"INFO"`, `"trace"`, `"warning"`, and whitespace-padded values are invalid.

## Algorithm

```
func Resolve(defaults, file, env, flags map[string]any) (Config, error):
    var cfg Config   // all fields are Go zero values
    layers := []map[string]any{defaults, file, env, flags}
    for _, layer := range layers:
        if layer == nil:
            continue
        for key, value := range layer:
            spec, ok := fieldByName[key]
            if !ok:
                panic(fmt.Sprintf("config: unknown key %q", key))
            spec.apply(&cfg, value)   // panics if value has the wrong dynamic type
    if _, ok := legalLogLevels[cfg.LogLevel]; !ok:
        return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
    return cfg, nil
```

`fieldByName` is an unexported `map[string]fieldSpec` initialized from `fields` in `init` (or a package-level composite literal). Iteration order of a layer map does not matter: each key is independent.

Type check inside `apply` (or a shared `mustValue`):

- `kindBool`: `v.(bool)`, else panic with key name, expected type, and `fmt.Sprintf("%T", v)`.
- `kindInt`: `v.(int)`, else panic the same way.
- `kindString`: `v.(string)`, else panic the same way.

Panic text is not part of the public contract. It must identify the key and the type mismatch so a programmer error is obvious. Use `panic`, not a returned error. Wrong type and unknown key are programmer errors, as specified.

On validation failure, return the zero `Config` and the wrapped sentinel. Do not return a partially meaningful `Config` alongside the error. The public test ignores the config value on that path. Callers that check `err` first see a defined zero value.

Do not validate intermediate presented `LogLevel` values. A lower layer may present `"trace"` and a higher layer may present `"info"`. Only `"info"` is checked, and it is legal. A higher layer that presents `""` overrides a lower illegal or legal value. Effective `""` is legal and returns `nil` error.

`Verbose`, `MaxRetries`, and `TimeoutSec` are never range-checked. Negative ints, `false`, and `0` are valid effective values when presented (or when they are the zero value of an absent field).

## How each stated rule is satisfied

1. **Four ordered layers, higher overrides lower.** `layers` is `[defaults, file, env, flags]`. Each present key overwrites `cfg`. The last write is the highest-precedence presenter. Public cases 2 and 7.

2. **In-memory only.** `Resolve` uses only its four arguments. No `os.Getenv`, no file IO, no `flag` package.

3. **Present includes zero values.** Presence is the comma-ok map lookup, not a value comparison. `false`, `0`, and `""` overwrite. Public cases 3, 4, and 5.

4. **Absent has no effect.** A key not in the map is not applied. Nil maps are skipped. A field never presented stays at the Go zero value of `Config`. Public case 6 (flags omits `LogLevel`, file value remains). Public case 1 (only defaults presented).

5. **No layer presents a field → Go zero.** `var cfg Config` starts at zero. Fields with no successful apply stay zero. Example: all-nil arguments yield `Config{}` and `nil` error, because effective `LogLevel` is `""`, which is legal.

6. **Wrong Go type panics.** Type assert to `bool`, `int`, or `string` before write. No coercion.

7. **Only legal keys.** Any other key panics before apply. This includes empty string keys and differently cased field names (`"verbose"`).

8. **Validation only on effective `LogLevel`.** The legal-set check runs once, after all layers are applied, on `cfg.LogLevel` only. Overridden presented values are never passed to the check. Legal set: `"debug"`, `"info"`, `"warn"`, `"error"`, `""`. Anything else returns an error that unwraps to `ErrInvalidLogLevel`. Public case 8 (`"trace"`). Other fields are not validated.

9. **Public acceptance.** The eight tests in `resolve_public_test.go` map onto the rules above. `go build`, `go vet`, and `go test ./...` are the acceptance commands, run from `worktree/`.

## Extensibility (years, more layers and fields)

- New field: add a `Config` field, one `fieldSpec` row (name, kind, apply), and, if it needs validation, a post-resolve check that sees only the effective value. Layer walk stays unchanged.
- New layer: add a parameter only if the public signature is allowed to change. The spec fixes four parameters today, so do not add a fifth parameter in this pass. Internally the walk is already "a slice of maps". A later API can accept `...map[string]any` or a `[]Layer` without changing the per-field apply logic. Document that constraint in a short comment above `Resolve`: precedence is argument order, low to high, and the slice is the extension point.
- Do not encode field logic in a switch duplicated per layer. One registry, one walk.
- Keep `ErrInvalidLogLevel` stable. Future field validators get their own sentinels. Do not overload this sentinel for non-log-level failures.

This pass does not add a variadic API, file loading, or env parsing. Those would violate the fixed signature and the in-memory scope.

## Error and panic policy

| Condition | Behavior |
| --- | --- |
| Unknown key in any layer | panic |
| Present value, wrong dynamic type | panic |
| Effective `LogLevel` not in the legal set | return `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)` |
| Effective `LogLevel` in the legal set, including `""` | return `cfg, nil` |
| Nil layer | treat as empty, no error |
| Zero values presented | apply them, no error |
| Negative `MaxRetries` or `TimeoutSec` | apply them, no error |

No other error values. `Resolve` does not return a non-nil error that fails `errors.Is(err, ErrInvalidLogLevel)`.

## Files to add

- `worktree/config.go` — sentinel, `Config`, `Resolve`, field registry, type checks, log-level check.
- `worktree/config_test.go` — unexported-path tests described below. Same package (`package config`) so tests can call `Resolve` directly. Do not use `package config_test`. The public file is already `package config`.

Do not modify `worktree/go.mod`, `worktree/resolve_public_test.go`, or `instrument/resolve_public_test.go`.

## Test approach

Acceptance (must pass, already written):

```
cd worktree && go test ./...
cd worktree && go vet ./...
cd worktree && go build ./...
```

The eight public tests:

1. `TestDefaultsOnly` — only defaults map is non-nil; all four fields match that map.
2. `TestHigherNonZeroWins` — file `LogLevel=debug` beats defaults `info`.
3. `TestPresentFalseOverridesTrue` — flags `Verbose=false` beats defaults `true`.
4. `TestPresentZeroIntOverrides` — env `MaxRetries=0` beats defaults `3`.
5. `TestPresentEmptyStringOverrides` — file `LogLevel=""` beats defaults `"info"`, error is nil.
6. `TestAbsentPassesThrough` — flags sets `Verbose` only; file `LogLevel=warn` remains; defaults nil.
7. `TestAllFourLayersPrecedence` — `TimeoutSec` presented as 1, 2, 3, 4; effective is 4.
8. `TestInvalidEffectiveLogLevel` — flags `LogLevel=trace`; `errors.Is(err, ErrInvalidLogLevel)`.

Additional tests in `config_test.go` (implementor-owned, not required by the spec text but used to lock the rules the public suite does not hit):

- All four arguments nil → zero `Config`, nil error.
- Each field absent from every layer while another field is set → that field is the Go zero value.
- Same field presented in defaults and flags only (file and env omit it) → flags wins. Confirms absence in the middle layers does not block a higher layer and does not clear a lower one incorrectly when the higher layer is also absent: defaults + file omit + env omit + flags omit → defaults remains.
- Lower layer `LogLevel=trace`, higher layer `LogLevel=info` → nil error, effective `info`. Proves overridden values are not validated.
- Lower layer `LogLevel=info`, higher layer `LogLevel=trace` → `ErrInvalidLogLevel`. Proves the effective value is what is validated.
- Each legal level (`""`, `debug`, `info`, `warn`, `error`) presented only in flags → nil error.
- Illegal levels not in the public test: `"INFO"`, `"warning"`, `" debug"` → `ErrInvalidLogLevel`.
- Unknown key panics (`recover` in the test).
- Wrong types panic: `Verbose` as `string`, `MaxRetries` as `int64`, `LogLevel` as `int`, `TimeoutSec` as `float64`.
- `errors.Is` still holds when the error is wrapped (if wrapping is used). A direct `==` comparison is not required by the spec. The public test uses `errors.Is` only.
- Mixed map: one layer sets `Verbose=false` and `MaxRetries=0` and omits `LogLevel` and `TimeoutSec`, lower layer set the omitted fields → omitted fields show through, presented zeros win.

Panic tests must recover. A panic that escapes fails the test, which is what we want if the implementation returns an error instead of panicking.

No table is required for the public file. A table is appropriate inside `config_test.go` for the legal/illegal log-level list and the precedence permutations.

## Implementation order

1. Add `config.go` with the API, registry, walk, type panic, and log-level check.
2. Add `config_test.go` with the extra cases above.
3. Run `go test ./...`, `go vet ./...`, and `go build ./...` in `worktree/`.
4. Fix any failure in the implementation. Do not change the public tests to make them pass.

## Out of scope

- Reading environment variables, files, or flags.
- Defaulting absent fields to anything other than the Go zero value.
- Validating `Verbose`, `MaxRetries`, or `TimeoutSec`.
- Coercing numeric types.
- Exporting the field registry or layer type.
- Changing `Resolve`'s signature.
