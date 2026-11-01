# Implementation plan: layered configuration resolver

Package `config` (module path `config`, Go 1.25, existing `go.mod` in the worktree). No I/O. Caller supplies four in-memory layers. Implement only what the spec requires, plus the design decisions recorded below. Do not implement until this plan is approved.

This revision addresses `plan/plan_findings.md`. Point-by-point status is in `plan/plan_rebuttal.md`.

## Public API

Package name: `config`. Files live next to `go.mod`.

```go
package config

// Config is the effective configuration after layer merge.
// Field order and names are fixed; tests compare with ==.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// ErrInvalidLogLevel is returned (via errors.Is) when the effective LogLevel
// is not one of the values accepted by validate.
// Concrete value: errors.New("config: invalid log level").
// The legal set is not duplicated here; see the unexported legalLogLevels set.
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve merges defaults, file, env, flags in increasing precedence.
// A nil map is an empty layer (every field absent).
// Unknown keys and wrong Go types panic (design decision for unknown keys;
// spec for wrong types). See doc.go.
// On success and on validation failure, the Config value is the merged
// effective configuration. Validation runs only on the effective LogLevel.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Exported surface is exactly `Resolve`, `Config`, and `ErrInvalidLogLevel`. No extra exported types. The four-argument signature stays fixed so existing callers, including the public tests, do not break.

## Internal data model

Presence is map-key presence, not value non-zeroness. A layer is `map[string]any`. A missing key, a nil map, and a key that is not in a higher layer all mean "absent" for that layer. A present key whose value is `false`, `0`, or `""` still sets the field.

Fixed field catalog (not caller-extensible in v1, but table-driven so a new field is one row):

| Key          | Go type | Zero when no layer presents it |
|--------------|---------|--------------------------------|
| `Verbose`    | `bool`  | `false`                        |
| `MaxRetries` | `int`   | `0`                            |
| `LogLevel`   | `string`| `""`                           |
| `TimeoutSec` | `int`   | `0`                            |

Internal types (unexported). Type identity lives in one place: `kind`. There is no second type-check inside a per-field closure.

```go
type fieldKind int // kindBool, kindInt, kindString

type fieldSpec struct {
    key  string
    kind fieldKind
}

// fieldCatalog is the single source of field keys and kinds.
var fieldCatalog = []fieldSpec{
    {key: "Verbose", kind: kindBool},
    {key: "MaxRetries", kind: kindInt},
    {key: "LogLevel", kind: kindString},
    {key: "TimeoutSec", kind: kindInt},
}
```

`apply(dst *Config, spec fieldSpec, v any)` switches on `spec.kind`, type-asserts to `bool`, `int`, or `string`, and writes the matching `Config` field selected by `spec.key`. A kind/type mismatch panics. A key that is in the catalog but has no write arm panics (catalog and struct drifted). Panic text names the key and the dynamic type (`fmt.Sprintf("%T", v)`) so a programmer error is diagnosable.

Legal dynamic types, driven only by `kind`:

- `kindBool` (`Verbose`): `bool` only. `*bool` and numeric types panic.
- `kindInt` (`MaxRetries`, `TimeoutSec`): `int` only. `int32`, `int64`, `float64`, and `uint` panic. JSON-decoded numbers are out of scope; the caller must pass `int`.
- `kindString` (`LogLevel`): `string` only.

Unknown keys (any key not in the catalog) panic when encountered in any layer, including a key that would be overridden. This is a **design decision**, not a sentence in the spec. The spec says the only legal keys are the four field names and states an explicit panic only for a present value of the wrong Go type. Panicking on an unknown key is the chosen reading of "only legal keys," so a typo cannot be silently ignored. Record that decision in `doc.go`. Panic before returning a `Config`. Scan every layer for unknown keys and type errors even if a higher layer also presents the field, so a programmer error is never hidden by precedence.

Nil `any` stored under a known key (`map[string]any{"Verbose": nil}`) is a present key with the wrong type and panics. Spec: a present value of the wrong Go type panics; nil is not `bool`, `int`, or `string`.

No reflection on the resolve path. Reflection is used only in a test that checks catalog coverage of exported `Config` fields. No `encoding` imports. No environment, file, or flag parsing.

## How each stated rule is satisfied

### Precedence and presence

Implement one algorithm: sequential full scan, low to high. Do not treat "pick the highest presenting layer per field" as equivalent. That formulation skips overridden values, so it would miss a wrong-typed value in a lower layer that a higher layer overrides, and a catalog-key-only loop would miss unknown keys. The full scan is required by the programmer-error rule below.

1. Start from a zero `Config` (covers "no layer presents a field").
2. Build `layers := []map[string]any{defaults, file, env, flags}` and call unexported `resolve(layers ...map[string]any) (Config, error)`.
3. Walk layers from lowest to highest.
4. For each layer, if the map is nil, skip it.
5. For each key in the layer:
   - Unknown key: panic.
   - Known key: `apply` (type-check from `kind`, write the field).
6. Because the walk is low-to-high, a later present key overwrites an earlier one, including overwriting a non-zero with `false`, `0`, or `""`.
7. A key absent from a higher layer is never written by that layer, so the lower value remains. This is the absent-passes-through rule. It is not implemented by "skip zero values".

`Resolve` is a thin wrapper:

```go
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
    return resolve(defaults, file, env, flags)
}
```

Nil layer argument equals an empty map. Public tests pass `nil` for unused layers.

Adding a layer later means a new exported wrapper (or an additional exported variadic API) that still calls `resolve`. Merge logic does not change, and the existing `Resolve` signature does not break.

### Validation

After the merge, and only then, `return cfg, validate(cfg)`.

Legal levels live in one unexported set. The sentinel comment and tests refer to that set; they do not restate a second list that can drift.

```go
var legalLogLevels = map[string]struct{}{
    "":      {},
    "debug": {},
    "info":  {},
    "warn":  {},
    "error": {},
}

func validate(cfg Config) error {
    if _, ok := legalLogLevels[cfg.LogLevel]; ok {
        return nil
    }
    return fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)
}
```

- Overridden illegal levels are never inspected as the effective value. Example: defaults `"trace"`, flags `"info"` → success, `LogLevel == "info"`. The lower `"trace"` was still type-checked as `string` during the scan; it is not validated.
- Effective `""` is legal (public test 5). No error. An illegal lower level overridden by an explicit `""` is also legal: defaults `"trace"`, flags `""` → `LogLevel == ""`, nil error.
- Effective `"trace"` returns the merged `Config` (so `LogLevel == "trace"` and any other fields that were presented) and an error with `errors.Is(err, ErrInvalidLogLevel)` (public test 8). Spec: `Resolve` returns the effective `Config` and an error. The error case is not an exception.
- Error text is `config: invalid log level: "trace"`. The prefix is not repeated.
- `Verbose`, `MaxRetries`, and `TimeoutSec` are not range-checked. Negative ints and any bool are accepted.
- Case-sensitive. `"Debug"`, `"INFO"`, `" warn"` are invalid effective values.
- No trimming.

### Programmer errors (panic)

- Unknown key in any layer. Design decision; documented in `doc.go`.
- Present value whose dynamic type is not the field's kind, including `nil`. Spec: programmer error must panic. Applies whether or not a higher layer overrides that key.

Do not return these as `error`. `recover` is not used inside `Resolve` or `resolve`.

### Scope boundaries

- Does not read env, files, or `os.Args`. Layer names are precedence labels only.
- Does not mutate the input maps.
- Does not copy maps for the caller. Reads only.
- Map iteration order does not matter: each key is independent, and unknown-key panics may fire in any order. Tests must not depend on which illegal key panics first when several are illegal.
- Concurrency: `Resolve` does not synchronize. Caller must not mutate a map during `Resolve`. Document in the function comment. No package-level mutable state except the sentinel error and the immutable catalog and legal-level set.

### Growth (maintainability, no extra behavior)

- Field list is one slice of `fieldSpec` (`key` + `kind` only). Adding a field later is a new `Config` field, one catalog row, and one write arm in `apply` keyed by `spec.key`. A test fails if an exported `Config` field has no catalog row.
- Layer merge is `resolve(layers ...map[string]any)`. Adding a layer does not change merge logic and does not change the exported `Resolve` signature.
- Validation is `validate(cfg Config) error`, separate from merge. Policy changes do not touch the scan.
- Comments only where presence-vs-zero or the unknown-key design decision is non-obvious.

## File layout

```
worktree/
  go.mod                      # existing: module config, go 1.25
  resolve_public_test.go      # existing, identical to instrument/; do not edit or replace
  doc.go                      # package comment: presence, precedence, panic vs error, unknown-key decision
  resolve.go                  # Config, ErrInvalidLogLevel, Resolve, resolve, catalog, apply, validate
  resolve_test.go             # tests beyond the shipped suite (see below)
```

`worktree/resolve_public_test.go` already exists and matches `instrument/resolve_public_test.go`. The implementor does not copy it and does not modify it. Local tests use `resolve_test.go` so they do not collide.

`go build` and `go vet` must be clean. No third-party dependencies.

## Test approach

Shipped tests (must pass unchanged; file already in the worktree):

| Test | Rule covered |
|------|----------------|
| `TestDefaultsOnly` | Only defaults; nil higher layers; struct equality |
| `TestHigherNonZeroWins` | file LogLevel overrides defaults |
| `TestPresentFalseOverridesTrue` | flags `false` overrides defaults `true` |
| `TestPresentZeroIntOverrides` | env `0` overrides defaults `3` for MaxRetries |
| `TestPresentEmptyStringOverrides` | file `""` overrides `"info"`; `""` is valid |
| `TestAbsentPassesThrough` | flags present for Verbose does not clear file LogLevel; nil defaults |
| `TestAllFourLayersPrecedence` | flags TimeoutSec wins over env, file, defaults |
| `TestInvalidEffectiveLogLevel` | `"trace"` → `errors.Is(..., ErrInvalidLogLevel)` |

Additional tests in `resolve_test.go` (not a substitute for the public suite):

1. No layer presents anything (all nil) → zero `Config`, nil error. Covers the "no presenter → Go zero" rule for every field, including LogLevel `""` being valid.
2. Each legal effective LogLevel (`""`, `debug`, `info`, `warn`, `error`) from flags alone returns nil error. Iterate `legalLogLevels` (or a helper that returns its keys) so the test does not keep a second copy of the set.
3. Illegal effective levels that a higher layer does not override: `"Trace"`, `"debug "`, `"fatal"`. `errors.Is` is true. Also assert the returned `Config.LogLevel` equals the illegal effective value (merged config is returned with the error).
4. Illegal lower level overridden by a legal non-empty higher level does not error: defaults `"trace"`, file `"warn"` → `"warn"`, nil error. Proves validation is on the effective value only.
5. Illegal lower level that remains effective because the higher layer omits the key: defaults `"trace"`, flags `{"Verbose": true}` → error, and `got.Verbose == true` and `got.LogLevel == "trace"`. Presence is per key. Returned config is the merge, not zero.
6. Zero-value override on every numeric/bool field from the highest layer: flags `Verbose=false`, `MaxRetries=0`, `TimeoutSec=0` over non-zero defaults.
7. Mid-layer win: defaults and flags omit `TimeoutSec`, env sets `9` → `9`. file sets `LogLevel`, env and flags omit it → file value.
8. Lower non-zero does not leak when a middle layer sets zero and a higher layer is absent for that key.
9. Illegal lower LogLevel overridden by explicit empty string: defaults `{"LogLevel": "trace"}`, flags `{"LogLevel": ""}` → `LogLevel == ""`, nil error. A skip-zero-values implementation fails this (it would keep `"trace"` and error, or drop `""`).
10. Unknown key panics (`recover` in the test): `"verbose"` (wrong case), `"Extra"`.
11. Wrong type panics: `Verbose` as `string`, `MaxRetries` as `int64`, `LogLevel` as `[]byte`, `TimeoutSec` as `float64`, known key with nil value.
12. Unknown key in a lower layer panics even when flags are well-formed.
13. Wrong type in a lower layer panics even when a higher layer presents a well-typed value for the same key: defaults `{"MaxRetries": "3"}`, flags `{"MaxRetries": 1}` must panic. A highest-layer-only scan would miss this.
14. Input maps are not mutated: clone each input with `maps.Clone` before `Resolve`, then `maps.Equal` the original against the clone (keys and values).
15. `ErrInvalidLogLevel` is non-nil and stable (`==` the package variable, and `errors.Is`).
16. Catalog coverage: reflect over exported fields of `Config`. Each field name appears as exactly one `fieldCatalog` key, and that row's `kind` matches the field type (`bool`/`int`/`string`). Fail if a `Config` field has no row or a row names a field that does not exist. Protects "gains more fields" against a forgotten catalog row.

Panic tests use `defer recover()` and fail if `Resolve` returns instead of panicking. They do not assert panic string equality, only that a panic occurred, so message wording can stay readable.

Run: `go test ./...` and `go vet ./...` from the worktree.

## Implementation order

1. `doc.go` (presence rule, precedence, wrong-type panic from the spec, unknown-key panic as a design decision, no mutation, no synchronization).
2. Exported declarations (`Config`, sentinel, `Resolve` signature) and `resolve(layers ...map[string]any)`.
3. `fieldCatalog` (`key` + `kind` only) and `apply` driven by `kind`.
4. `legalLogLevels` and `validate`. On failure, return the merged `cfg` with `fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)`.
5. Local tests listed above, including lower-layer type panic, `""` overriding an illegal level, reflection catalog check, and `maps.Equal` non-mutation.
6. `go test ./...` and `go vet ./...`.

No other packages, no CLI, no generated code.
