# Implementation plan: layered configuration resolver

Package: `config` (module path `config`, Go 1.25, existing `go.mod` in the worktree).
Deliverable file: `worktree/resolve.go` (package `config`). Public tests already live at `worktree/resolve_public_test.go` and must keep passing unchanged.

No prior adversary findings exist (`plan/plan_findings.md` is absent). This plan is the initial design.

## Public API

Fixed signatures. Do not add required parameters, do not change field types, do not rename exports.

```go
package config

import "errors"

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "", "debug", "info", "warn", "error".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Config is the effective configuration after layer resolution.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// Resolve combines four layers in ascending precedence:
// defaults, file, env, flags. A nil map is an empty layer.
// A present value of the wrong Go type panics.
// An unknown key panics (programmer error; only the four field names are legal).
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Error contract:

- On invalid effective `LogLevel`, return a non-nil error such that `errors.Is(err, ErrInvalidLogLevel)` is true. The sentinel itself may be returned directly, or wrapped with `%w`. Either satisfies the public test. Prefer returning the sentinel directly so callers can compare with `==` as well as `errors.Is`. The zero `Config` accompanies the error (public test ignores the value).
- On success, return `(cfg, nil)`.
- `Verbose`, `MaxRetries`, and `TimeoutSec` are never validated. Any `bool` / `int` / `int` is legal, including zero values and negative ints.

Type contract (programmer error, panic, not `error`):

- A key other than `"Verbose"`, `"MaxRetries"`, `"LogLevel"`, `"TimeoutSec"` panics.
- A present value whose dynamic type is wrong for its key panics. Accepted types are exact, not convertible:
  - `Verbose`: `bool` only. `*bool`, integer `0`/`1`, and `string` panic.
  - `MaxRetries`, `TimeoutSec`: `int` only. `int32`, `int64`, `float64`, and untyped numeric values that arrived as another concrete type panic. Map literals in the public tests use untyped integer constants, which become `int` inside `map[string]any`.
  - `LogLevel`: `string` only.
- Absence is “key not in the map” (including a nil map). A present nil interface value (`any(nil)` stored under a key) is a wrong type and panics. JSON-null style values are not in scope.

## Internal data model

Presence must be tracked separately from the value. A `Config` alone cannot tell “not set” from “set to zero”.

```go
// fieldKind identifies one legal key and the Go type it must have.
type fieldKind int

const (
    fieldVerbose fieldKind = iota
    fieldMaxRetries
    fieldLogLevel
    fieldTimeoutSec
)

// layer is one precedence slot. Name is for panic messages and future growth.
type layer struct {
    name   string
    values map[string]any
}

// setState records whether a field has been presented and, if so, its value.
// Only the slot matching the field is meaningful.
type setState struct {
    presented bool
    b         bool
    i         int
    s         string
}
```

Resolution state is a fixed array indexed by `fieldKind`, not a second `map[string]any`. That keeps the “present vs zero” distinction typed and makes adding a field a local change (one kind, one apply case, one struct field).

Layer order is a slice, lowest precedence first:

```go
layers := []layer{
    {"defaults", defaults},
    {"file", file},
    {"env", env},
    {"flags", flags},
}
```

Walk that slice once. For each layer, range the map. For each key, look up `fieldKind`, type-assert, and overwrite that field’s `setState` (set `presented = true`). Later layers overwrite earlier ones. A key absent from a layer is never visited, so lower state remains. A nil map ranges as empty, which matches “absent layer”.

After the walk, copy presented values into `Config`. Unpresented fields stay at the Go zero value of the struct literal.

Validation runs only after the effective `LogLevel` is known:

```go
switch cfg.LogLevel {
case "", "debug", "info", "warn", "error":
    return cfg, nil
default:
    return Config{}, ErrInvalidLogLevel
}
```

An overridden illegal level never reaches this switch. Example: defaults `"trace"`, flags `"info"` → effective `"info"` → success. Example: defaults `"info"`, flags `"trace"` → error. Example: no layer presents `LogLevel` → effective `""` → success (empty is legal).

Unknown keys and bad types panic with a message that names the layer and the key, so a future caller can see which map was wrong. Panic is via `panic(...)`, not `log.Fatal`.

## How each stated rule is satisfied

1. **Four ordered layers, higher overrides lower.** Single forward walk, flags last, last write wins. Public case 7 (`TimeoutSec` 1 then 2 then 3 then 4) yields 4.
2. **Present includes zero values.** The map key’s presence, not the value, sets `presented`. `false`, `0`, and `""` overwrite. Public cases 3, 4, and 5.
3. **Absent has no effect.** Missing keys are not written. A higher layer that omits a field leaves the lower `setState`. Public case 6: flags sets `Verbose` only; file `LogLevel` remains. Nil maps are absent for every field. Public case 1: only defaults presents; file/env/flags nil.
4. **No layer presents a field → Go zero.** `setState.presented` stays false; `Config` zero value is used. Not separately asserted by the public suite, but required by the spec (and implied when case 6 leaves `MaxRetries` and `TimeoutSec` at 0 and `Verbose` false on the defaults-nil path for those fields — case 6 does set `Verbose` from flags).
5. **Wrong type or illegal key panics.** Checked at apply time before any write, so a bad entry does not partially commit that key. Other keys in the same layer may already have been applied; that is acceptable because panic means the call must not be recovered as a normal result. Document that `Resolve` is not panic-safe for partial use of the returned value (it does not return).
6. **Validate only effective `LogLevel`.** Switch runs once, on `cfg.LogLevel`, after merge. Legal set is exactly `"debug"`, `"info"`, `"warn"`, `"error"`, `""`. Anything else, including `"trace"`, `"INFO"`, `"warning"`, and whitespace, returns `ErrInvalidLogLevel`. Public case 8.
7. **Other fields are not validated.** No range checks on retries or timeout.
8. **No I/O.** `Resolve` does not read env, files, or flags. Layer names are precedence labels only.
9. **Years of growth.** Adding a layer is appending to the `layers` slice (call signature would change only if the public API grows; until then extra layers are internal). Adding a field is: new `fieldKind`, new `Config` field, one apply branch, one copy-out branch. No reflection. Reflection would hide the type panic and fight `go vet`; skip it.

Map iteration order does not matter: each key is independent, and a duplicate key cannot appear in one Go map.

## Test approach

Acceptance (already shipped, do not edit):

```text
go test ./...
go build ./...
go vet ./...
```

Run from `worktree/`. The eight public tests cover:

| Test | Rule |
| --- | --- |
| `TestDefaultsOnly` | only defaults; nil higher layers; all four fields |
| `TestHigherNonZeroWins` | file `LogLevel` overrides defaults |
| `TestPresentFalseOverridesTrue` | flags `Verbose=false` overrides defaults `true` |
| `TestPresentZeroIntOverrides` | env `MaxRetries=0` overrides defaults `3` |
| `TestPresentEmptyStringOverrides` | file `LogLevel=""` overrides defaults `"info"`; no error |
| `TestAbsentPassesThrough` | flags omits `LogLevel`; file value shows through |
| `TestAllFourLayersPrecedence` | flags win when all four present `TimeoutSec` |
| `TestInvalidEffectiveLogLevel` | `"trace"` → `errors.Is(..., ErrInvalidLogLevel)` |

Optional package tests (same package, separate file `resolve_extra_test.go`) if time allows after the public suite is green. They are not required for acceptance. If written, they must not change the public API:

- No layer presents any field (all nil) → zero `Config`, nil error (`LogLevel == ""` is legal).
- Illegal level in a lower layer overridden by a legal higher level → nil error (validation is effective-only).
- Illegal level in a higher layer overriding a legal lower level → `ErrInvalidLogLevel`.
- `TimeoutSec=0` presented over a non-zero lower value → `0`.
- Unknown key panics (`recover` in the test).
- Wrong dynamic type panics (`MaxRetries` as `int64`, `Verbose` as `string`).
- Case-sensitive log level: `"INFO"` is invalid.
- Each legal level (`""`, `debug`, `info`, `warn`, `error`) succeeds.

Do not add file, env, or flag parsing. Do not introduce a second package. Do not change `go.mod` unless the toolchain requires it (it should not).

## Implementation order

1. Add `resolve.go` with the sentinel, `Config`, field kinds, layer walk, type checks, copy-out, and log-level switch.
2. `go test ./...`, `go build ./...`, `go vet ./...` in `worktree/`.
3. If a public test fails, fix `resolve.go` only. Do not edit `resolve_public_test.go`.
