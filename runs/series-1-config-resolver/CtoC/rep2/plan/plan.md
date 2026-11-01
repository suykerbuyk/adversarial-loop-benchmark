# Implementation Plan — Layered Configuration Resolver (`package config`)

Round 1. No prior adversary findings exist (`plan/plan_findings.md` absent), so there is no rebuttal file.

## 0. Location and files

Module `config` (go.mod: `module config`, `go 1.25`), package lives at the worktree root next to
the shipped `resolve_public_test.go` (which is kept byte-for-byte unmodified).

| File | Contents |
|---|---|
| `config.go` | Package doc, `Config`, `ErrInvalidLogLevel`, `Resolve`, the internal `layer` type and `resolve` loop. |
| `fields.go` | The field descriptor type, the generic constructor, the `fields` table, the key index. |
| `validate.go` | Valid log-level set and `validate(Config) error`. |
| `resolve_test.go` | Internal (package `config`) tests beyond the public suite (§6). |

No third-party dependencies. Standard library only: `errors`, `fmt`.

## 1. Public API (fixed by spec, reproduced exactly)

```go
// Config is the effective configuration produced by Resolve.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// ErrInvalidLogLevel is wrapped by the error Resolve returns when the
// effective LogLevel is not one of "debug", "info", "warn", "error", "".
var ErrInvalidLogLevel = errors.New("invalid log level")

// Resolve combines the layers in increasing precedence order
// (defaults < file < env < flags) and returns the effective Config.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Documented contract (in the godoc):
- A key present in a layer sets the field even when its value is a zero value. An absent key has no effect.
  A nil map counts as an empty layer.
- Panics if a layer has a key that is not a `Config` field name, or a value whose dynamic type is not
  exactly the field's Go type. This includes a `nil` value, named types such as `type myInt int`,
  `int64`, and `float64` from JSON. These are programmer errors.
- On error the returned `Config` is the zero value. Callers must not use it.
- Never mutates the input maps. Safe for concurrent use: package-level tables are read-only after init.

`ErrInvalidLogLevel` is a `var` (as the spec requires) set by `errors.New`. It is never reassigned.

## 2. Internal data model

### 2.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags" — used only in panic messages
    values map[string]any
}
```
`Resolve` is a thin adapter:
```go
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
    return resolve([]layer{
        {"defaults", defaults},
        {"file", file},
        {"env", env},
        {"flags", flags},
    })
}
```
The slice is ordered lowest → highest precedence. `resolve` does not know how many layers exist or
what they are called. Adding a layer means adding one slice element (plus the parameter, if the
public signature is ever extended).

### 2.2 Fields: a descriptor table, one line per field
```go
type field struct {
    key      string                      // map key == Config field name
    typeName string                      // for panic messages, e.g. "int"
    set      func(c *Config, v any) bool // assigns v if its dynamic type is exact; reports ok
}

func fieldOf[T any](key string, ptr func(*Config) *T) field {
    var zero T
    return field{
        key:      key,
        typeName: fmt.Sprintf("%T", zero),
        set: func(c *Config, v any) bool {
            t, ok := v.(T)   // exact dynamic type match; nil interface fails
            if ok {
                *ptr(c) = t
            }
            return ok
        },
    }
}

var fields = []field{
    fieldOf("Verbose",    func(c *Config) *bool   { return &c.Verbose }),
    fieldOf("MaxRetries", func(c *Config) *int    { return &c.MaxRetries }),
    fieldOf("LogLevel",   func(c *Config) *string { return &c.LogLevel }),
    fieldOf("TimeoutSec", func(c *Config) *int    { return &c.TimeoutSec }),
}

var fieldIndex = indexFields(fields) // map[string]int; panics at init on a duplicate key
```
Why a table instead of reflection: it is explicit, has compile-time type safety (the `ptr` closure
fixes `T` to the struct field's type, so a wrong `T` will not compile), has no reflection at runtime,
and the exact-type check is a plain type assertion. A reflection-based test (§6.4) covers the one
weakness, which is drift between the struct and the table. Adding a field takes two lines: the
struct field and its descriptor. If the field is validated, a rule goes in `validate`.

Why not `*T` pointers in an intermediate struct ("presence via pointers"): the table plus comma-ok
lookup already encodes presence directly. An intermediate struct would duplicate every field.

## 3. Algorithm

```go
func resolve(layers []layer) (Config, error) {
    var c Config                          // fields no layer presents keep the Go zero value
    for _, l := range layers {            // lowest → highest; later writes override
        checkKnownKeys(l)                 // panics on any key not in fieldIndex
        for _, f := range fields {        // fixed table order → deterministic
            v, present := l.values[f.key] // comma-ok: presence, not zero-ness
            if !present {
                continue                  // absent: lower value shows through
            }
            if !f.set(&c, v) {
                panic(fmt.Sprintf("config: layer %q: key %q: value %#v has type %T, want %s",
                    l.name, f.key, v, v, f.typeName))
            }
        }
    }
    if err := validate(c); err != nil {
        return Config{}, err
    }
    return c, nil
}

func checkKnownKeys(l layer) {
    for k := range l.values {
        if _, ok := fieldIndex[k]; !ok {
            panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, k))
        }
    }
}
```
Reading a nil map is legal in Go and yields "absent" for every key, so a nil layer needs no
special case.

## 4. How each rule is satisfied

| Spec rule | Mechanism |
|---|---|
| Present field sets the value, including `false`/`0`/`""` | Presence is decided by `v, present := m[key]`, never by comparing against the zero value. A present zero is assigned like any other value. |
| Highest-precedence presenting layer wins | Layers are applied lowest → highest into one `Config`, so the last write (highest presenting layer) remains. |
| Absent: no effect, lower value shows through | `!present` → `continue`. Nothing is written, so whatever an earlier layer wrote (or the zero value) remains. |
| No layer presents → Go zero value | `var c Config` starts zeroed. Untouched fields stay zero. |
| Four maps in order defaults, file, env, flags | `Resolve` builds the layer slice in exactly that order. |
| Pure in-memory: no env, files, or argv | The package imports only `errors` and `fmt`. No `os` or `flag`. |
| Only legal keys are the four names | `checkKnownKeys` panics on anything else, including case variants like `"verbose"`. (Spec: "the only legal keys are…". A silently ignored typo such as `"Verbos"` would be a latent bug, so it is treated as the same class of programmer error as a wrong type.) |
| Wrong Go type for a present value must panic | Exact type assertion `v.(T)` in `set`. This fails for `nil`, `int64`, `float64`, `uint`, `"true"`, named types, and so on, and `resolve` panics with layer, key, value, actual type and wanted type. **Every layer is type-checked, including layers whose value is later overridden.** The spec's type rule has no "effective only" carve-out, unlike validation. |
| Validation only on the effective `LogLevel` | `validate(c)` runs once, after all layers are merged, on the final `Config`. An overridden presented `LogLevel` (e.g. defaults `"trace"`, flags `"info"`) never reaches it. |
| Legal levels: `debug`, `info`, `warn`, `error`, `""` | `validLogLevels = map[string]struct{}{"debug":{}, "info":{}, "warn":{}, "error":{}, "":{}}`. Exact, case-sensitive match with no trimming or normalisation: `"INFO"`, `" info"`, and `"warning"` are invalid. `""` is valid whether it was presented explicitly or is the zero value from no layer presenting it. Both cases give effective `""`, and the spec validates only the effective value. |
| Error satisfies `errors.Is(err, ErrInvalidLogLevel)` | `fmt.Errorf("config: %w %q (want one of \"debug\", \"info\", \"warn\", \"error\", or \"\")", ErrInvalidLogLevel, c.LogLevel)`. `%w` wraps, so `errors.Is` holds, and the offending value is in the message. |
| `Verbose`, `MaxRetries`, `TimeoutSec` not validated | `validate` only checks `LogLevel`. Negative ints, for example, are accepted. |

```go
func validate(c Config) error {
    if _, ok := validLogLevels[c.LogLevel]; !ok {
        return fmt.Errorf(...%w..., ErrInvalidLogLevel, c.LogLevel)
    }
    return nil
}
```
`validate` takes the whole `Config`, so future per-field rules on effective values go in one place.

## 5. Edge-case decisions (explicit)

1. **All four layers nil or empty** → `Config{}` and `nil` error (effective `LogLevel` `""` is legal).
2. **Present key with `nil` value** → panic (a nil interface is not a `bool`/`int`/`string`).
3. **Named or alias types**: `type myInt int` → panic, because its type is not exactly `int`. A true
   alias (`type I = int`) is identical to `int` and is accepted, as Go type identity dictates.
4. **Overridden layer with a wrong-typed value** → still panics (§4).
5. **Overridden invalid `LogLevel`** → no error. **Non-overridden invalid `LogLevel` in any layer,
   including `defaults`** → error.
6. **Return value on error** → `Config{}` (the Go convention that results are unspecified when
   err != nil; zero avoids handing out a half-trusted config).
7. **Panic determinism**: within a layer, field checks follow table order. When a layer has several
   unknown keys, which one the panic message names depends on map iteration order. That has no
   effect on behaviour (it still panics) and tests match on substrings that do not depend on it.
8. **Input maps** are only read, never written or retained.

## 6. Test approach

The public suite `resolve_public_test.go` stays unmodified and must pass (cases 1–8). An additional
internal file, `resolve_test.go` (package `config`), has table-driven tests:

### 6.1 Override semantics (table: name, four layers, want Config)
- All nil; all empty maps → zero Config, no error.
- Present zero for each field type in each higher layer over a non-zero lower value: `Verbose=false`,
  `MaxRetries=0`, `TimeoutSec=0`, `LogLevel=""`, presented by file, env, and flags in turn.
- Absent passes through several gaps (only defaults sets it; file/env/flags absent). A middle layer
  shows through (env set, flags absent, defaults/file set lower).
- Fields are independent: each field wins from a different layer in the same call, e.g. Verbose from
  flags, MaxRetries from env, LogLevel from file, TimeoutSec from defaults.
- Non-zero lower value, zero higher value, non-zero highest value → highest wins.
- Full `Config` equality is compared (`got != want`) so stray writes to other fields are caught.

### 6.2 Validation
- Each legal level (`debug`, `info`, `warn`, `error`, `""`) as the effective value → no error.
- Illegal values (`trace`, `INFO`, ` info`, `warning`, `fatal`) → `errors.Is(err, ErrInvalidLogLevel)`.
  The message contains the quoted bad value, and the returned Config is zero.
- Invalid at defaults with no override → error. Invalid at defaults/file/env overridden by a valid
  level at a higher layer → no error. Invalid overridden by an explicit `""` → no error, effective `""`.
- `LogLevel` never presented → `""`, no error.
- Non-LogLevel fields are not validated: negative `MaxRetries`/`TimeoutSec` → no error.

### 6.3 Panics (helper `mustPanic(t, substr, func())` using `recover`)
- Wrong type per field: `Verbose: "true"`, `Verbose: 1`, `MaxRetries: int64(3)`,
  `MaxRetries: 3.0`, `LogLevel: 1`, `TimeoutSec: "30"`, `TimeoutSec: uint(5)`.
- `nil` value for a present key.
- Named type `type myInt int` for `MaxRetries`.
- Wrong type in an **overridden** layer (defaults `MaxRetries: "x"`, flags `MaxRetries: 1`) → still panics.
- Wrong type in each of the four layers, with the layer name in the panic message.
- Unknown key (`"Verbos"`) and case variant (`"verbose"`) → panic naming the key.

### 6.4 Structural and maintainability guards
- Reflection test: for every field of `Config` there is exactly one descriptor whose `key` equals the
  struct field name and whose `typeName` equals the field's type. `len(fields) == NumField()`. This
  catches a field added to the struct but not the table.
- Inputs are not mutated: deep-copy the maps before the call and compare with `reflect.DeepEqual` after.
- Concurrency: run `Resolve` from many goroutines under `go test -race`.

### 6.5 Commands (acceptance gate)
```
gofmt -l .        # no output
go vet ./...      # clean
go build ./...
go test -race -count=1 ./...
```
