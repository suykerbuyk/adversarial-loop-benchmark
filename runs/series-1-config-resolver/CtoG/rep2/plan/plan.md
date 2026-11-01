# Implementation Plan: Layered Configuration Resolver (`package config`)

Status: round 1. No prior adversary findings (`plan/plan_findings.md` doesn't exist), so there's
no rebuttal file for this round.

## 0. Location and build setup
- Code goes in `worktree/`. That directory already has `go.mod` (`module config`, `go 1.25`) and
  a copy of `resolve_public_test.go`. The test file is identical to `instrument/resolve_public_test.go`
  and must not be modified.
- New files:
  - `worktree/config.go`: the production code (API, field table, merge, validation).
  - `worktree/config_test.go`: the implementor's extra tests (see §6).
- Standard library only. Requires Go 1.18+ for generics, and `go.mod` declares 1.25.

## 1. Public API (exactly what the spec fixes, nothing more)
```go
package config

type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

var ErrInvalidLogLevel = errors.New("config: invalid log level")

func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```
- `ErrInvalidLogLevel` is a package-level sentinel declared with `var`, as the spec requires.
  Callers match it with `errors.Is`.
- The package exports nothing else. Helpers, the field table and the layer type are unexported,
  so the surface stays exactly what the spec fixes and can change internally without breaking
  callers.
- Every exported identifier gets a doc comment, plus a package doc comment that states the
  precedence order and the present/absent semantics.

## 2. Internal data model

### 2.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags": used only in panic messages
    values map[string]any
}
```
`Resolve` builds `[]layer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}}`
in ascending precedence and passes it to an unexported `resolve(layers []layer) (Config, error)`.
To add a layer later, add one slice element and one `Resolve` parameter. The merge logic does
not change.

### 2.2 Field table (one row per `Config` field)
```go
type field struct {
    key string
    // set assigns v to the field in c. It returns false if v has the wrong dynamic type.
    set func(c *Config, v any) bool
}

func bind[T any](key string, ptr func(*Config) *T) field {
    return field{key: key, set: func(c *Config, v any) bool {
        t, ok := v.(T)
        if ok { *ptr(c) = t }
        return ok
    }}
}

var fields = []field{
    bind("Verbose",    func(c *Config) *bool   { return &c.Verbose }),
    bind("MaxRetries", func(c *Config) *int    { return &c.MaxRetries }),
    bind("LogLevel",   func(c *Config) *string { return &c.LogLevel }),
    bind("TimeoutSec", func(c *Config) *int    { return &c.TimeoutSec }),
}
```
- Each row ties the key to its Go type and its struct field at compile time. To add a field, add
  one struct field and one table row. There's no reflection and no per-field `switch` that could
  drift out of sync.
- The type assertion is `v.(T)` with the exact type: `int` only, not `int64`, `float64` or other
  integer kinds; `bool`; `string`. A presented `nil` value (key present, value `nil`) fails the
  assertion and counts as a wrong type.
- A `known` set (`map[string]struct{}`) built from `fields` in `init` (or computed lazily from the
  table) supports the unknown-key check. It comes from the table, so it can't diverge from it.

## 3. Algorithm
```
resolve(layers):
    var cfg Config                         // starts at Go zero values: "no layer presents" rule
    for each layer L in ascending precedence:
        checkKeys(L)                       // panic on unknown keys (see §4.3)
        for each f in fields (table order):
            v, present := L.values[f.key]  // comma-ok: presence, not value, decides
            if !present { continue }       // absent: no effect
            if !f.set(&cfg, v) { panic(typeError(L.name, f.key, v)) }
    if err := validateLogLevel(cfg.LogLevel); err != nil {
        return Config{}, err
    }
    return cfg, nil
```
- A `nil` layer map is valid. Reading a nil map returns `present == false`, so every field is
  absent. The public tests pass `nil` for unused layers.
- The input maps are only read. `Resolve` never mutates or keeps them.
- Layer and field iteration use slices, never map ranges, so the result and the first panic (if
  any) are deterministic.

## 4. How each rule is satisfied

### 4.1 Override rule
- **Present, including zero values:** presence is the comma-ok result `_, present := m[key]`,
  never a comparison with the zero value. So `false`, `0` and `""` are assigned like any other
  value (public cases 3, 4 and 5).
- **Highest precedence wins:** layers are applied low to high, and each present value overwrites
  the field. The last writer is the highest layer that presents the field (cases 2 and 7).
- **Absent passes through:** an absent key is skipped and the lower value stays (case 6).
- **No layer presents:** `cfg` starts as `Config{}`, so the field keeps its Go zero value
  (case 1 covers the defaults-only case; §6 covers all-nil).

### 4.2 Wrong type panics
- Every present value in every layer is type-checked, including values a higher layer overrides.
  The spec says *a present value of the wrong type is a programmer error*, with no exception for
  shadowed values. The check happens during assignment, which visits every present value.
- Panic value: a descriptive string, built with `fmt.Sprintf`, of the form
  `config: layer "file": key "MaxRetries": want int, got float64`. The wanted type name comes from
  the field row: `bind` captures `reflect.TypeFor[T]()`, or `fmt.Sprintf("%T", zero T)` to avoid
  a `reflect` import. A `nil` value prints as `got <nil>`.
- Type panics happen during the merge, before validation. A call that has both a type error and
  an invalid level panics; it doesn't return the error. A programmer error takes priority over a
  data error.

### 4.3 Unknown keys (an interpretation, stated explicitly)
The spec says the only legal keys are the four field names, but it doesn't say what happens with
any other key. I treat an illegal key the same way as a wrong-typed value: a programmer error
that **panics**. For example, a typo such as `"Loglevel"` would otherwise be silently ignored and
the field would quietly fall back to a lower layer, which is the hardest kind of config bug to
find.
- Implementation of `checkKeys`: if `len(L.values)` is greater than the number of recognised
  present keys, collect the keys not in `known`, sort them, and panic with
  `config: layer "env": unknown key(s) ["Loglevel"]`. Sorting keeps the message deterministic.
- This rule also applies to every layer, overridden or not.

### 4.4 Validation of the effective LogLevel
```go
func validateLogLevel(level string) error {
    switch level {
    case "", "debug", "info", "warn", "error":
        return nil
    }
    return fmt.Errorf("%w: %q (want one of \"debug\", \"info\", \"warn\", \"error\", or \"\")",
        ErrInvalidLogLevel, level)
}
```
- The function runs exactly once, on `cfg.LogLevel` after the merge, so an overridden `LogLevel`
  is never examined. For example, `file="trace"` with `flags="info"` gives no error.
- `""` is legal both when a layer presents it explicitly and when no layer presents `LogLevel`
  (the zero value). The two cases are indistinguishable after the merge, and both are allowed.
- The comparison is exact and case-sensitive, with no trimming: `"INFO"` and `" info"` are
  invalid. The spec lists exact strings.
- Wrapping with `%w` makes `errors.Is(err, ErrInvalidLogLevel)` true (case 8), and the message
  names the offending value.
- `Verbose`, `MaxRetries` and `TimeoutSec` are never validated. Negative ints are accepted.

### 4.5 Return value on error
On a validation failure, `Resolve` returns `Config{}` together with the error, following the
usual Go convention that the other results are unspecified or zero when `err != nil`. Callers
shouldn't use a config that failed validation. The doc comment on `Resolve` states this.

## 5. Quality, vet and maintainability
- `gofmt`-clean and `go vet`-clean. There are no printf-verb mismatches: the panic and error
  formatting use `%q`, `%T` and `%w` correctly.
- Small, single-purpose unexported functions: `resolve`, `checkKeys`, `validateLogLevel`,
  `bind`.
- Comments explain *why* presence uses comma-ok rather than zero-value checks, since that is
  the central rule of the spec and the most likely regression.

## 6. Test approach
Run the commands `go build ./...`, `go vet ./...` and `go test ./...` (also with `-race` and
`-count=1`) in `worktree/`.

1. **The public suite** (`resolve_public_test.go`, unmodified) covers spec cases 1–8.
2. **Extra table-driven tests** in `config_test.go`, each row with a name, four layers, a
   wanted `Config` and a wanted error:
   - all four layers `nil` gives `Config{}` with no error. This checks the "no layer presents"
     rule and that `""` is a valid effective level.
   - empty non-nil maps behave the same as `nil`.
   - every zero value presented at each precedence boundary overrides a non-zero value below
     it: `false` over `true`, `0` over `N` for both int fields, and `""` over a level, for each
     adjacent pair defaults→file→env→flags.
   - the opposite direction: a non-zero value over a zero value.
   - a field present only in a middle layer (`env`) shows through `flags` when `flags` presents
     other fields.
   - each field resolved from a different layer in the same call (mixed sources).
   - every legal level, `"debug"`, `"info"`, `"warn"`, `"error"` and `""`, as the effective
     value gives no error.
   - invalid effective levels `"trace"`, `"INFO"`, `" info"`, `"warning"` and `"\x00"` each
     give `errors.Is(err, ErrInvalidLogLevel)` and a zero `Config`.
   - an invalid level that is overridden gives no error: `defaults="trace"` with `flags="info"`,
     and `file="bogus"` with `env=""`.
   - a valid lower level overridden by an invalid higher one gives an error.
   - negative or large `MaxRetries` and `TimeoutSec` give no error.
3. **Panic tests**, using a helper `mustPanic(t, func())` that recovers and asserts a non-nil
   panic and a message substring:
   - wrong types: `"Verbose": "true"`, `"MaxRetries": int64(3)`, `"MaxRetries": 3.0`,
     `"LogLevel": 1`, `"TimeoutSec": nil`, one per layer position.
   - a wrong type in a layer that a higher layer overrides still panics.
   - an unknown key (`"Loglevel"`, `"Timeout"`) in any layer panics, and the message names the
     key.
   - a wrong type together with an invalid level panics; it doesn't return an error.
4. **Input immutability:** take a copy of each input map before the call, then compare with
   `maps.Equal` after the call.
5. **Determinism:** call `Resolve` repeatedly on the same inputs and expect identical results.
   Also check that the unknown-key panic message is stable across calls when there are several
   unknown keys.
6. **Example test** (`ExampleResolve`) that shows the zero-value override. It is executable
   documentation, checked by `go test`.

## 7. Out of scope (per spec)
No reading of environment variables, files or the command line, and no conversion between
types: string `"3"` is not converted to `3`. That kind of parsing belongs to the caller that
builds the layer maps.
