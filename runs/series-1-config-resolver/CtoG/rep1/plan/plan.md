# Implementation Plan — Layered Configuration Resolver (`package config`)

Round 1. No `plan/plan_findings.md` exists yet, so there are no adversary findings to address and no
rebuttal file was written.

## 0. Context and layout
- Module: `worktree/go.mod` → `module config`, `go 1.25`. The package lives at the module root
  (`package config`), next to the shipped `resolve_public_test.go`, which is identical to
  `instrument/resolve_public_test.go`. That file is **not modified**.
- Files to add:
  - `config.go`: package doc, `Config`, `ErrInvalidLogLevel`, `Resolve`.
  - `fields.go`: the field table (one entry per `Config` field) and the generic binder.
  - `loglevel.go`: the set of legal log levels and `validateLogLevel`.
  - `resolve_test.go`: internal tests (in addition to the public suite).
- No third-party dependencies. Standard library only (`errors`, `fmt`, `reflect` only for
  `reflect.TypeFor` in panic messages and in a test).

## 1. Public API (fixed by spec, reproduced exactly)
```go
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

var ErrInvalidLogLevel = errors.New("config: invalid log level")

func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```
- `ErrInvalidLogLevel` is a package-level sentinel from `errors.New`. It is a `var` as the spec
  requires, and it is never reassigned.
- Nothing else is exported. The field table, layer type, and validator stay unexported so that they
  can change later without breaking callers.

## 2. Internal data model
### 2.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags", used only in messages
    values map[string]any // may be nil; nil means "presents nothing"
}
```
`Resolve` builds `[]layer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}}`,
ordered from **lowest to highest** precedence, and passes it to an unexported
`resolve(layers []layer) (Config, error)`. To add a layer later, you add one parameter and one slice
element. The merge logic stays the same.

### 2.2 Fields (table-driven, type-safe)
```go
type field struct {
    key string                    // map key, e.g. "MaxRetries"
    set func(c *Config, v any)    // type-checks v and assigns; panics on wrong type
}

func bind[T any](key string, ptr func(*Config) *T) field {
    return field{key: key, set: func(c *Config, v any) {
        t, ok := v.(T)
        if !ok {
            panic(fmt.Sprintf("config: key %q: value %#v has type %T, want %v",
                key, v, v, reflect.TypeFor[T]()))
        }
        *ptr(c) = t
    }}
}

var fields = []field{
    bind("Verbose",    func(c *Config) *bool   { return &c.Verbose }),
    bind("MaxRetries", func(c *Config) *int    { return &c.MaxRetries }),
    bind("LogLevel",   func(c *Config) *string { return &c.LogLevel }),
    bind("TimeoutSec", func(c *Config) *int    { return &c.TimeoutSec }),
}

var fieldsByKey = indexFields(fields) // map[string]field; panics at init on a duplicate key
```
Generics let the compiler tie each key to the Go type of its struct field. To add a field later, you
add one struct field and one `bind` line. A test (§5) fails if the table and the struct drift apart.
I chose not to use reflection over struct tags for the merge itself. Explicit code is easier to read
and debug, and it avoids reflection's runtime cost and surprises.

### 2.3 Log-level set
```go
var validLogLevels = map[string]struct{}{"": {}, "debug": {}, "info": {}, "warn": {}, "error": {}}
```

## 3. Algorithm (`resolve`)
```
var cfg Config                                    // every field starts at its Go zero value
var levelSrc string                               // name of the layer that last set LogLevel
for _, l := range layers {                        // lowest → highest precedence
    for key, v := range l.values {                // presence = key exists in the map
        f, ok := fieldsByKey[key]
        if !ok { panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, key)) }
        f.set(&cfg, v)                            // type check + assign (overwrites lower layers)
        if key == "LogLevel" { levelSrc = l.name }
    }
}
if err := validateLogLevel(cfg.LogLevel); err != nil {
    return Config{}, fmt.Errorf("%w %q (set by layer %q)", ErrInvalidLogLevel, cfg.LogLevel, levelSrc)
}
return cfg, nil
```
(The `levelSrc` part of the message is used only when a layer set the level. A zero-value `""` is
always valid, so an error can only come from a presenting layer.)

`resolve` never writes to any input map. Reading a nil map is legal in Go and yields no entries.

## 4. How each rule is satisfied
| Spec rule | Mechanism |
|---|---|
| Present means key in map, even with value `false`/`0`/`""` | Presence comes only from iterating the map's keys (`range l.values`). Values are never compared against zero. |
| Highest-precedence presenting layer wins | Layers are applied from lowest to highest, so later assignments overwrite earlier ones. |
| Absent: layer has no effect, lower value shows through | An absent key is never visited, so the value already in `cfg` from lower layers stays. |
| No layer presents a field → Go zero value | `cfg` starts as `Config{}`. |
| `nil` layer (as in the public tests) | Ranging over a nil map does nothing. It behaves like an empty layer. |
| Wrong Go type for a key → panic | `bind`'s checked type assertion `v.(T)` panics with a descriptive message. The check uses the exact type. `int64`, `float64`, `uint`, named types such as `type Level string`, and an untyped `nil` value are all wrong types and all panic. |
| Wrong type panics **even if overridden** | Every present value in every layer is type-checked when it is applied. The loop never short-circuits from the top layer down. A bad value in `defaults` panics even if `flags` presents the same key. |
| Only four legal keys | An unknown key is also treated as a programmer error and panics (see §6, decision D1). |
| Validate only the effective `LogLevel` | Validation runs once, after merging, on `cfg.LogLevel`. Overridden values are never inspected. |
| Legal levels `debug/info/warn/error/""` | `validLogLevels` membership is exact and case-sensitive, so `"INFO"` and `" info"` are invalid. |
| Invalid → `errors.Is(err, ErrInvalidLogLevel)` | The error is built with `fmt.Errorf("%w …", ErrInvalidLogLevel, …)`, which wraps the sentinel. |
| Other fields not validated | No checks are made on them. Negative `MaxRetries`/`TimeoutSec` are accepted. |
| Panics take priority over the LogLevel error | Type and key panics happen during the merge, before validation runs. |

## 5. Test approach
Run `gofmt -l .` (must print nothing), `go build ./...`, `go vet ./...`, and `go test -race ./...` in
`worktree/`.

1. **Public suite**: `resolve_public_test.go` is unmodified and covers spec cases 1–8.
2. **`resolve_test.go`** (package `config`, table-driven, `t.Run` subtests):
   - **Zero-value override, every field × every non-default layer.** For each of `Verbose=false`,
     `MaxRetries=0`, `TimeoutSec=0`, `LogLevel=""`, put a non-zero value in `defaults` and the zero
     value in `file`, `env`, and `flags` in turn. Expect the zero value.
   - **Absent pass-through across gaps**: the value is set in `defaults` only, and higher layers
     present other keys. Also: the value is in `file`, while `env` and `flags` are non-nil maps
     without the key.
   - **Precedence for every pair of layers** (i < j): the higher layer wins, for each field.
   - **Nothing presented**: all four layers `nil`, and all four empty maps. Expect `Config{}` and a
     nil error, because the empty `LogLevel` is legal.
   - **Validation of the effective value only**: `defaults{"LogLevel":"trace"}` with
     `flags{"LogLevel":"info"}` returns no error. `defaults{"LogLevel":"info"}` with
     `flags{"LogLevel":"trace"}` returns an error. All five legal levels are accepted. Invalid
     values `"trace"`, `"INFO"`, `"Debug"`, `" info"`, and `"warning"` each satisfy `errors.Is`. On
     error, the returned `Config` is the zero value.
   - **Non-LogLevel fields unvalidated**: `MaxRetries=-1` and `TimeoutSec=-5` are returned as given.
   - **Wrong-type panics**, checked with a `mustPanic(t, func())` helper that uses `recover`. Cases:
     `MaxRetries: int64(3)`, `MaxRetries: 3.0`, `MaxRetries: "3"`, `Verbose: 1`, `Verbose: "true"`,
     `LogLevel: []byte("info")`, `LogLevel: nil`, and a named type `type lvl string`. Also, a wrong
     type in `defaults` that is overridden by a correct value in `flags` must still panic.
   - **Unknown key panics**: `"verbose"` (wrong case) and `"Timeout"`.
   - **Inputs not mutated**: snapshot the input maps, call `Resolve`, and compare with
     `maps.Equal`/`reflect.DeepEqual`.
   - **Table/struct drift guard**: use `reflect` over `Config` to check that every struct field has
     exactly one `fields` entry with the same name and type, and that there are no extras.
   - **Error message sanity**: the error string contains the bad value and the layer name.
3. Optional: a small `Example` test (`ExampleResolve`) for documentation.

## 6. Decisions on points the spec leaves open
- **D1 – Unknown keys panic.** The spec says the four keys are "the only legal keys" and treats a
  wrong type as a programmer error. An unknown key (a typo such as `"Loglevel"`) is the same kind of
  mistake. Ignoring it silently would hide bugs, which is exactly what this resolver exists to avoid.
- **D2 – On a validation error, return `Config{}`.** This is the idiomatic Go convention: when the
  error is non-nil, callers should not use the value. Spec case 8 discards the value anyway.
- **D3 – Exact type matching.** Only `bool`, `int`, and `string` exactly are accepted. No numeric
  conversion is done, and no `float64` from JSON is accepted. The spec says a wrong Go type must
  panic, and converting would blur that rule.
- **D4 – Panic messages.** They name the key, the layer where possible (unknown key), the received
  type, and the expected type, so that a programmer error is quick to diagnose.
- **D5 – Package doc comment** describes presence semantics (key present means set, even when the
  value is a zero value) and the panic contract, so that callers know the API's assumptions.
