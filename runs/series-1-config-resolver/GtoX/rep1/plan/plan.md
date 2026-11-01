# Implementation plan: layered configuration resolver

Package `config` under the existing module (`module config`, Go 1.25, `worktree/go.mod`). In-memory only. No environment, file, or flag parsing. This document is the plan only; no code is written here.

No prior adversary findings exist at `plan/plan_findings.md`, so there is no rebuttal file.

## Public API

All symbols live in package `config`. Callers import the module path `config`.

```go
package config

import "errors"

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "", "debug", "info", "warn", "error".
//
// It is a sentinel created with errors.New. Resolve wraps it with %w so
// errors.Is(err, ErrInvalidLogLevel) is true. The wrapped error names the
// rejected value. Callers must not depend on the exact error string.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the effective configuration after layer merge and validation.
// Field order is fixed: Verbose, MaxRetries, LogLevel, TimeoutSec.
// Zero values are legal results (false, 0, "", 0).
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// Resolve merges four layers in ascending precedence and validates the
// effective LogLevel.
//
// Precedence, lowest to highest: defaults, file, env, flags.
// A key present in a map sets that field, including zero values
// (false, 0, ""). A key absent from a map, or a nil map, leaves the field
// unchanged. If no layer presents a field, the result is the Go zero value.
//
// Legal keys are "Verbose", "MaxRetries", "LogLevel", "TimeoutSec".
// A present value whose dynamic type is wrong for its key panics.
// An unknown key panics (programmer error).
//
// Validation runs only on the effective LogLevel, after every layer has been
// applied. A presented LogLevel that a higher layer overrides is not validated.
// Verbose, MaxRetries, and TimeoutSec are not validated.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

### Contract details the signature alone does not state

- Presence is map-key presence (`_, ok := m[key]`), never “value is non-zero”. `false`, `0`, and `""` override lower layers.
- A `nil` map is an empty layer: every field is absent. The public tests pass `nil` for unused layers.
- Extra keys beyond the four legal names are programmer errors and panic. The spec says the only legal keys are those four.
- Wrong dynamic type for a legal key panics. Accepted types:
  - `"Verbose"` → `bool` only. `*bool`, integer types, and `string` panic.
  - `"MaxRetries"` and `"TimeoutSec"` → `int` only. `int32`, `int64`, `uint`, `float64`, and untyped-looking numeric boxes panic. Callers who build maps from JSON must convert before calling `Resolve`.
  - `"LogLevel"` → `string` only. `[]byte` and `fmt.Stringer` panic.
- Panic message includes the key and the actual dynamic type (`%T`) so a future maintainer can diagnose a bad layer without a debugger. Panic is unconditional; `Resolve` does not return a typed error for programmer mistakes.
- On validation failure, the returned `Config` is the zero `Config`. Callers must not use the config when `err != nil`. The public test only checks the error, so a non-zero partial config would still pass that test; returning zero is the maintainable choice and is stated so hidden tests that inspect the value have a defined result.
- `ErrInvalidLogLevel` is a package-level variable of interface type `error`, matching the spec’s `var ErrInvalidLogLevel error`. It is not a custom type. Wrapping uses `fmt.Errorf("...: %w", ErrInvalidLogLevel)`.
- `Resolve` does not mutate the caller’s maps. It only reads keys.
- Layer names are precedence labels only. The function does not look at `os.Environ`, files, or `flag`.

## Internal data model

One file is enough: `resolve.go`. Tests ship separately (`resolve_public_test.go` is already in the module root). No subpackages.

### Field descriptors

A small table drives both merge and type checks so adding a field later is one row, not a new control-flow branch.

```go
type fieldID int

const (
    fieldVerbose fieldID = iota
    fieldMaxRetries
    fieldLogLevel
    fieldTimeoutSec
)

type fieldSpec struct {
    key  string
    id   fieldID
    // apply panics if v has the wrong dynamic type, otherwise writes into cfg.
    apply func(cfg *Config, v any)
}

var fields = []fieldSpec{
    {key: "Verbose", id: fieldVerbose, apply: applyVerbose},
    {key: "MaxRetries", id: fieldMaxRetries, apply: applyMaxRetries},
    {key: "LogLevel", id: fieldLogLevel, apply: applyLogLevel},
    {key: "TimeoutSec", id: fieldTimeoutSec, apply: applyTimeoutSec},
}
```

`applyVerbose`, `applyMaxRetries`, `applyLogLevel`, and `applyTimeoutSec` each do a single type switch (or comma-ok assertion) and assign. Wrong type calls `panic` with key and `%T`.

A parallel `knownKeys map[string]struct{}` (built from `fields` in `init`, or a composite literal) rejects unknown keys in O(1).

### Layers

`Resolve` does not invent a layer type. It walks a fixed slice:

```go
layers := []map[string]any{defaults, file, env, flags}
```

Index 0 is lowest precedence. Later layers overwrite earlier ones only for keys they present. This slice is the extension point: a future fifth layer is another argument plus one more element, or a variadic `...map[string]any` if the public signature is ever widened. The public signature stays four named parameters for this task.

### Effective value

`Config` itself is the accumulator. It starts at the Go zero value, which is exactly the “no layer presented this field” result. There is no separate “was set” bitmap: absence is expressed by never writing the field. That matches the override rule and avoids confusing “not set” with a stored zero.

### Legal log levels

```go
var legalLogLevels = map[string]struct{}{
    "":      {},
    "debug": {},
    "info":  {},
    "warn":  {},
    "error": {},
}
```

Membership is exact string equality, case-sensitive. `"INFO"`, `"warning"`, `"trace"`, and whitespace-padded values are invalid. `""` is legal.

## How each stated rule is satisfied

### Scope

`Resolve` reads only the four maps passed in. No `os`, `flag`, `bufio`, or filesystem imports. Layer parameter names document precedence and nothing else.

### Presence versus absence

For each layer from lowest to highest, for each key in that map:

1. If the key is unknown, panic.
2. If the key is known, call `apply`, which type-checks and writes the field.

Iteration is over the field table, not over `range layer`, so behavior does not depend on map iteration order. For each field, look the key up in the current layer. Present (`ok == true`) means apply, even when the value is `false`, `0`, or `""`. Absent (`ok == false`) means skip. A nil layer yields `ok == false` for every key.

Because higher layers run later, the last present value remains. That is the highest-precedence presenter.

### Zero-value override

There is no `if v != zero` guard. `applyVerbose` assigns `cfg.Verbose = v.(bool)` even when `v` is `false`. `applyMaxRetries` assigns `0`. `applyLogLevel` assigns `""`. Public tests 3, 4, and 5 lock this in.

### Absent field shows through

Skipping an absent key leaves the accumulator as written by a lower layer, or as zero if none wrote it. Public test 6: `flags` presents `Verbose` only, so `LogLevel` stays the `file` value `"warn"`.

### No presenter → Go zero

The accumulator starts zero and is written only when a key is present. Public test 1 uses nil for the three higher layers, so the result equals the defaults map. A call with four nil maps returns `Config{}` and `nil` error (`LogLevel == ""` is legal).

### Wrong type panics

Each `apply` function accepts exactly one concrete type, listed above. A failed assertion panics before any further field is written. The panic happens at the moment the bad value is presented, including when a higher layer would have overridden that field. The spec says a present value of the wrong type is a programmer error and must panic; it does not say “panic only if the bad value wins”. Panicking on encounter is simpler and fails fast.

### Unknown key panics

Before applying, every key in the layer is checked against `knownKeys`. Unknown keys panic even if a known key in the same map is well typed. Check is per layer, so a typo in `defaults` panics even when `flags` is valid.

Implementation note: ranging the map to find unknown keys is required, because the field table only knows legal keys. Unknown-key scan and field apply are two passes per layer, or one pass that dispatches through `knownKeys` to the spec. Either is fine. Preferred shape:

```text
for each layer in [defaults, file, env, flags]:
    if layer == nil: continue
    for key := range layer:
        spec, ok := fieldByKey[key]
        if !ok: panic("config: unknown key " + key)
        spec.apply(cfg, layer[key])
```

Map iteration order does not affect the result: each key writes a different field, and a duplicate key cannot exist in one map. Cross-layer order is fixed by the outer slice.

### Validation of effective LogLevel only

After the loop, if `cfg.LogLevel` is not in `legalLogLevels`, return `Config{}, fmt.Errorf("config: log level %q: %w", cfg.LogLevel, ErrInvalidLogLevel)`.

Do not validate inside `applyLogLevel`. A lower layer may present `"trace"` and a higher layer may present `"info"`; the effective value is `"info"` and must succeed. Conversely, a lower `"info"` overridden by a higher `"trace"` must fail. Empty string is in the legal set, so test 5 returns nil error.

`Verbose`, `MaxRetries`, and `TimeoutSec` have no range checks. Negative retries and negative timeouts are accepted.

### errors.Is

`fmt.Errorf` with `%w` preserves the sentinel. `errors.Is(err, ErrInvalidLogLevel)` is the public contract. Returning the bare sentinel would also satisfy `errors.Is`, but wrapping records the bad value without changing the Is-check. Do not use a custom type whose `Is` method ignores the sentinel unless it still unwraps to `ErrInvalidLogLevel`.

### Build and vet

- `go build ./...` in the module root.
- `go vet ./...` clean: no unchecked errors that matter, no printf mismatches, panic arguments are strings or formatted strings used correctly.
- `go test ./...` runs the shipped public tests. They live in package `config` (same package), so they call `Resolve` and read `ErrInvalidLogLevel` directly.

### Longevity

- New field: add a `fieldID`, a `fieldSpec` row, an `apply` function, and a `Config` field. Merge and unknown-key logic stay unchanged.
- New layer: add a parameter and insert it at the correct index in the layer slice. Precedence is position, not a name string inside the map.
- New log level: add one map entry. Do not scatter string compares.
- Do not use reflection over `Config` for the merge. Reflection would accept wrong kinds more easily (for example `int64` into an `int` field) and would hide the panic policy. Explicit apply functions document the exact dynamic type.
- Do not copy maps. Do not sort keys except if a future deterministic panic-on-multiple-unknown-keys requirement appears; one unknown key is enough to panic.

## Control flow of Resolve

```text
func Resolve(defaults, file, env, flags map[string]any) (Config, error):
    var cfg Config
    for _, layer := range []map[string]any{defaults, file, env, flags}:
        if layer == nil:
            continue
        for key, val := range layer:
            spec, ok := fieldByKey[key]
            if !ok:
                panic(fmt.Sprintf("config: unknown key %q", key))
            spec.apply(&cfg, val)   // panics on wrong dynamic type
    if _, ok := legalLogLevels[cfg.LogLevel]; !ok:
        return Config{}, fmt.Errorf("config: log level %q: %w", cfg.LogLevel, ErrInvalidLogLevel)
    return cfg, nil
```

`fieldByKey` is built once from `fields`.

## Test approach

### Shipped public tests (must pass unchanged)

The file `instrument/resolve_public_test.go` is the acceptance suite. It is already copied to `worktree/resolve_public_test.go` and is package `config`. Do not edit it. The eight cases:

| Test | Rule locked |
| --- | --- |
| `TestDefaultsOnly` | Only defaults presented; nil higher layers; full struct equality. |
| `TestHigherNonZeroWins` | file `LogLevel` overrides defaults. |
| `TestPresentFalseOverridesTrue` | flags `Verbose=false` overrides defaults `true`. |
| `TestPresentZeroIntOverrides` | env `MaxRetries=0` overrides defaults `3`. |
| `TestPresentEmptyStringOverrides` | file `LogLevel=""` overrides defaults `"info"`; no error. |
| `TestAbsentPassesThrough` | flags omits `LogLevel`; file value shows through. |
| `TestAllFourLayersPrecedence` | all four present `TimeoutSec`; flags wins. |
| `TestInvalidEffectiveLogLevel` | effective `"trace"` → `errors.Is(err, ErrInvalidLogLevel)`. |

Run: `go test ./...` from the module directory.

### Additional tests to add after the public file (same package)

These are not required by the eight public cases, but they pin rules the public file leaves implicit. Put them in `resolve_test.go` so the shipped file stays untouched.

1. Four nil maps → zero `Config`, nil error.
2. Highest layer omits a field that a middle layer set to zero → zero shows through (absence does not restore a lower non-zero). Example: defaults `MaxRetries=3`, env `MaxRetries=0`, flags omits `MaxRetries` → `0`.
3. Lower invalid log level overridden by higher legal level → nil error, higher value wins. Example: defaults `"trace"`, flags `"error"`.
4. Higher invalid log level overrides a legal lower level → `errors.Is` true. Example: defaults `"info"`, env `"trace"`.
5. Each legal level (`""`, `"debug"`, `"info"`, `"warn"`, `"error"`) resolves with nil error.
6. Case-mismatched `"Debug"` and unknown `"warning"` return `ErrInvalidLogLevel`.
7. Mixed fields across layers: defaults sets all four; file overrides only `LogLevel`; env overrides only `MaxRetries` with `0`; flags overrides only `Verbose` with `false`. Assert the untouched `TimeoutSec` remains the default.
8. Wrong type panics: `Verbose` as `string`, `MaxRetries` as `int64`, `LogLevel` as `int`, `TimeoutSec` as `float64`. Use `recover` in the test.
9. Unknown key `"verbose"` (wrong case) and `"Timeout"` panic.
10. Wrong type in a lower layer panics even if a higher layer presents a correct value for the same key (fail-fast policy).
11. Validation error unwraps to the sentinel and the returned `Config` is zero.
12. Caller maps are not mutated: snapshot before and after `Resolve`.

Panic tests must not use `t.Parallel` with shared globals; this package has no mutable globals besides the sentinel and lookup tables, which are read-only after init, so parallel is fine. Keep panic tests sequential only if that is simpler.

### Commands before calling the work done

From the module root (`worktree/`):

```text
go test ./...
go vet ./...
go build ./...
```

All three must succeed. `go test` is the acceptance gate. Vet must be clean. Build confirms the package is not test-only.

## File layout

```text
worktree/
  go.mod                      # existing: module config, go 1.25
  resolve.go                  # Config, ErrInvalidLogLevel, Resolve, field table
  resolve_public_test.go     # shipped; do not modify
  resolve_test.go            # extra coverage listed above
```

No `doc.go` required. Exported names carry doc comments that restate precedence, zero-value presence, panic-on-bad-type, and effective-only log-level validation.

## Out of scope

- Reading environment variables, files, or `flag.FlagSet`.
- JSON/YAML decoding and numeric widening.
- Defaulting a missing `LogLevel` to `"info"` (missing means `""`, and `""` is legal).
- Validating numeric ranges.
- Concurrent use of one map while `Resolve` runs. The function does not synchronize; callers own the maps. The function itself has no mutable package state, so concurrent `Resolve` calls on distinct maps are safe.
- Changing the public test file or the module path.
