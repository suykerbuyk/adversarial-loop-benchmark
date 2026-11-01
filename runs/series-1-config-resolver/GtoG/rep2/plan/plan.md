# Implementation plan: layered configuration resolver

Package `config` computes an effective `Config` from four in-memory layers. Precedence, low to high, is `defaults`, `file`, `env`, `flags`. The package does not read the environment, files, or the command line. There are no prior adversary findings (`plan/plan_findings.md` is absent), so this plan is the first cut.

The implementation lives in the existing module at `worktree/` (`module config`, Go 1.25). The shipped public tests already sit at `worktree/resolve_public_test.go` and call `Resolve` from package `config`.

## Public API

File: `worktree/config.go` (package `config`).

```go
package config

import "errors"

// ErrInvalidLogLevel is returned when the effective LogLevel is not one of
// "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Config is the resolved configuration. Unpresented fields keep the Go zero value.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// Resolve combines four layers in ascending precedence: defaults, file, env, flags.
// A key present in a layer sets that field, including zero values (false, 0, "").
// A key absent from a layer leaves the field unchanged. A present value of the
// wrong Go type panics. An unknown key panics. Only the effective LogLevel is
// validated.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Contract details that the signature alone does not show:

- `nil` maps are legal and mean every field is absent on that layer. The public tests pass `nil` for unused layers.
- Return the zero `Config` together with a non-nil error when validation fails. Callers that check `errors.Is` do not use the value, but a partial struct must not look like a successful resolve of an illegal level. Use `return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)` so `errors.Is(err, ErrInvalidLogLevel)` holds and the illegal value is available for diagnostics.
- `ErrInvalidLogLevel` is a package-level sentinel created with `errors.New`. Do not replace it with a custom type that fails `errors.Is` against the sentinel. Wrapping with `%w` is required, not optional, so both `errors.Is` and a useful message work.
- Panic, do not return an error, for a wrong Go type or an unknown key. The spec calls both programmer errors. Panic with a message that names the key, the layer index or name, and the dynamic type (or the illegal key). Do not recover inside `Resolve`.

## Internal data model

Keep the public surface to `Resolve`, `Config`, and `ErrInvalidLogLevel`. Everything else is unexported so later fields and layers do not force API churn.

### Field descriptors

One descriptor per field, in a package-level slice. Adding a field is one entry plus a member on `Config`.

```go
type fieldID int

const (
    fieldVerbose fieldID = iota
    fieldMaxRetries
    fieldLogLevel
    fieldTimeoutSec
)

type kind int

const (
    kindBool kind = iota
    kindInt
    kindString
)

type fieldDesc struct {
    id   fieldID
    key  string // map key; identical to the exported Config field name
    kind kind
}
```

Initial table, in a stable order used only for iteration (precedence is independent of this order):

| id | key | kind | assigned to |
|---|---|---|---|
| `fieldVerbose` | `"Verbose"` | `kindBool` | `Config.Verbose` |
| `fieldMaxRetries` | `"MaxRetries"` | `kindInt` | `Config.MaxRetries` |
| `fieldLogLevel` | `"LogLevel"` | `kindString` | `Config.LogLevel` |
| `fieldTimeoutSec` | `"TimeoutSec"` | `kindInt` | `Config.TimeoutSec` |

Keys are case-sensitive. `"verbose"` is an unknown key and panics. Only these four strings are legal.

### Layers

Do not store the four maps. Accept them as a fixed-length array built inside `Resolve`, index 0 lowest precedence:

```go
type layer struct {
    name string
    vals map[string]any // may be nil
}

// inside Resolve:
layers := [4]layer{
    {"defaults", defaults},
    {"file", file},
    {"env", env},
    {"flags", flags},
}
```

Walk this array from low to high. A later present value overwrites an earlier one. That is the entire precedence rule. A fifth layer later is another element in this array and another `Resolve` parameter (or a variadic form, only if the public signature is deliberately changed — it is fixed today).

### Presence, not zero-ness

Presence is `_, ok := layer.vals[key]`. A stored zero value is present. Absence is `ok == false`, including when the map itself is `nil`. Never use `==` against `false`, `0`, or `""` to decide whether a layer contributed. Never use reflection to detect "unset".

### Apply path

```text
var cfg Config
for each field in fieldTable:
    for each layer from defaults to flags:
        v, ok := layer.vals[key]
        if !ok: continue
        cfg = assign(cfg, field, coerce(field, v, layer))
    // if no layer presented the field, cfg keeps the Go zero for that member
if !legalLogLevel(cfg.LogLevel):
    return Config{}, wrapped ErrInvalidLogLevel
return cfg, nil
```

`coerce` is a type switch on `kind`, not on the field name, so new fields of an existing kind share it:

- `kindBool`: accept only `bool`. Reject `*bool`, integer 0/1, and strings.
- `kindInt`: accept only `int`. Reject `int32`, `int64`, `float64`, and `json.Number`. Map literals in the public tests use untyped constants, which become `int`. Callers that decoded JSON must convert numbers to `int` before calling `Resolve`. That matches "wrong Go type panics" and avoids a silent truncation policy.
- `kindString`: accept only `string`.

On mismatch, panic with `fmt.Sprintf("config: field %q in layer %q: got %T, want %s", key, layer.name, v, want)`. A typed nil inside an `any` is still a present wrong type (the dynamic type is not `bool`/`int`/`string`) and panics the same way.

Unknown keys: after applying known fields, or before, scan each non-nil layer. Any key not in the field table panics: `config: unknown field %q in layer %q`. Scan even when every known field is absent, so a typo is never silently ignored. Scan before apply so a bad key fails before any mutation is observable (the mutation is local anyway; the order is for a clearer panic if both a bad key and a bad type exist — unknown key wins if we scan first).

### Validation table

```go
func legalLogLevel(s string) bool {
    switch s {
    case "", "debug", "info", "warn", "error":
        return true
    default:
        return false
    }
}
```

Validate once, after every field has been resolved, and only `cfg.LogLevel`. A lower layer's `"trace"` that a higher layer replaces with `"info"` or `""` does not error. An effective `""` is legal, whether it came from a presented empty string or from no layer presenting `LogLevel` at all (Go zero of `string`).

`Verbose`, `MaxRetries`, and `TimeoutSec` have no range checks. Negative integers and `false` are valid effective values.

## How each stated rule is satisfied

1. **Four ordered layers, higher overrides lower.** `Resolve` parameters are already in ascending precedence. The apply loop writes a field only when `ok` is true, low to high, so the last present value remains.
2. **In-memory only.** No `os.Getenv`, no file I/O, no `flag` package. Layer names are labels on the internal `layer` struct for panic messages.
3. **Present includes zero values.** Presence is map-key existence. `false`, `0`, and `""` overwrite a lower non-zero value. This is what public tests 3, 4, and 5 lock in.
4. **Absent has no effect.** Missing key skips the layer. A `nil` map skips every field. Public test 6: `flags` omits `LogLevel`, so `file`'s `"warn"` remains.
5. **No layer presents a field → Go zero.** `cfg` starts zero and is written only on presence. Public test 1 sets every field from `defaults`; a fully nil call returns `Config{}` and `nil` error because effective `LogLevel` is `""`, which is legal.
6. **Legal keys only.** The field table is the allow-list. Any other key panics.
7. **Wrong Go type panics.** `coerce` accepts exactly `bool`, `int`, or `string` for the field's kind.
8. **Validation is effective-only and LogLevel-only.** The check runs after the merge. Overridden illegal levels are never inspected. Other fields are not checked.
9. **Legal effective levels.** `""`, `"debug"`, `"info"`, `"warn"`, `"error"`. Anything else, including `"trace"` (public test 8), `"DEBUG"`, and whitespace, returns an error wrapping `ErrInvalidLogLevel`.
10. **Public tests.** The eight cases in `instrument/resolve_public_test.go` match rules 3–5 and 8–9 directly. They are copied into the module as `resolve_public_test.go` and must pass unchanged. Do not edit that file.

## Extensibility (years, more layers and fields)

- New field of an existing kind: add a `fieldID`, a `fieldDesc`, a `Config` member, and one assignment arm. Precedence, presence, and panic behavior come for free.
- New kind: add a `kind` constant and one `coerce` arm. Do not special-case the new field in the merge loop.
- New layer: append to the `layers` array in precedence order. The public function signature is fixed by the spec today; a future signature change is out of scope. Internally, do not hard-code "the fourth map wins" outside that array.
- Do not reflect over `Config`. Explicit assignment keeps wrong-field bugs visible to the compiler when a member is added.

## Test approach

Public acceptance is `go test ./...` from `worktree/`, plus `go build` and `go vet`. Those already encode the eight spec cases. Do not weaken or rewrite them.

Add `worktree/resolve_test.go` with extra tests that the public file does not cover. These protect the rules the eight cases leave implicit:

| Case | Expectation |
|---|---|
| All four arguments `nil` | `Config{}`, `nil` error (`LogLevel` zero is legal) |
| Only some fields in `defaults`, rest absent | Unmentioned fields are zero; mentioned fields match |
| `flags` absent, `env` absent, `file` present, `defaults` present | `file` wins per field |
| Same field, non-adjacent layers (`defaults` and `flags` only) | `flags` wins |
| Per-field precedence mixed in one call | Each field takes its own highest present layer (e.g. `Verbose` from `flags`, `MaxRetries` from `env`, `LogLevel` from `file`, `TimeoutSec` from `defaults`) |
| Lower `"trace"`, higher `"info"` | `nil` error, `LogLevel == "info"` |
| Lower `"info"`, higher `"trace"` | `ErrInvalidLogLevel` |
| Effective `""` from flags overriding `"trace"` in defaults | `nil` error |
| Effective `""` because nobody set `LogLevel`, other fields set | `nil` error |
| Each legal level `"debug"`, `"info"`, `"warn"`, `"error"`, `""` | `nil` error |
| `"DEBUG"`, `"Warn"`, `" info"`, `"trace"` | `ErrInvalidLogLevel` |
| `Verbose: false` with no lower layer | `false`, `nil` error |
| `MaxRetries: 0` and `TimeoutSec: 0` as the only presentations | zeros, `nil` error |
| Negative `MaxRetries` / `TimeoutSec` | accepted, no error |
| Unknown key on any single layer, including when other layers are `nil` | panic |
| Wrong type: `Verbose` as `string`, `MaxRetries` as `int64` and `float64`, `LogLevel` as `bool` | panic |
| Present key with an explicitly typed value of the right type | accepted |
| `errors.Is` on a wrapped invalid-level error | true |
| Error path does not require the caller to ignore a populated `Config` | returned `Config` is zero (documented choice) |

Panic tests use `recover` in a helper so a missing panic fails the test.

No test reads the environment or the filesystem. No test depends on map iteration order.

## File layout

```text
worktree/
  go.mod                      # already present; module config, go 1.25
  config.go                   # Config, ErrInvalidLogLevel, Resolve, field table, coerce
  resolve_public_test.go     # already present; do not modify
  resolve_test.go             # additional cases above
```

One implementation file is enough. Split only if `config.go` grows past a single screen of field metadata plus `Resolve`. Do not add a `README` unless asked.

## Build and vet

From `worktree/`:

```text
go test ./...
go build ./...
go vet ./...
```

All three must succeed before the implementation is considered done. `go build` of a library package with no `main` still type-checks the package; `go test` is the acceptance run.

## Out of scope

- Reading env vars, config files, or CLI flags.
- JSON/YAML decoding and numeric type coercion.
- Validation of `Verbose`, `MaxRetries`, or `TimeoutSec`.
- Mutating the caller's maps.
- Concurrent use beyond "no package-level mutable state". `Resolve` allocates only its result and reads the caller's maps. Callers must not mutate a map concurrently with `Resolve`. The field table and sentinel are immutable after init.
