# Implementation Plan: Layered Configuration Resolver (`package config`)

Status: round 2. This revision addresses every item in `plan/plan_findings.md`; the point-by-point
response is in `plan/plan_rebuttal.md`. Changes since round 1 are marked **[R2]**.

## 0. Layout

- Module `config` (`worktree/go.mod`, go 1.25). The package lives at the module root, next to the
  shipped `resolve_public_test.go`, which is not modified.
- Files to add:
  - `worktree/config.go`: public API (`Config`, `ErrInvalidLogLevel`, `Resolve`), the layer/field
    model, merging, validation.
  - `worktree/config_internal_test.go`: additional tests (see §6).
- Standard library only (`errors`, `fmt`). Tests additionally use `reflect`, `maps`, `sync`.

## 1. Public API (exactly as fixed by the spec)

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

- `ErrInvalidLogLevel` is a package-level sentinel. The returned error wraps it, so callers match
  with `errors.Is`, not `==`.
- Every exported identifier has a doc comment. **[R2]** `Resolve`'s doc comment states explicitly:
  1. precedence order (defaults < file < env < flags) and that nil maps are allowed;
  2. the present/absent rule (a present zero value overrides) and the Go-zero-value fallback;
  3. **both** panic conditions: (a) a key other than the four legal keys, (b) a present value whose
     dynamic Go type is not exactly the field's type — checked in every layer, including values
     that are later overridden;
  4. the error contract: on an invalid effective `LogLevel` it returns the **effective** `Config`
     together with an error matching `ErrInvalidLogLevel`; the Config is returned for diagnostics
     only and callers must not use it as a valid configuration when `err != nil`.

## 2. Internal data model

### 2.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags"; used only in panic messages
    values map[string]any // may be nil: nil means every field is absent
}
```
`Resolve` builds `[]layer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}}`
(lowest → highest precedence) and delegates to the unexported `resolve(layers []layer) (Config, error)`.
Adding a layer is one slice entry (plus a parameter if the public signature ever changes); merge
logic is untouched.

### 2.2 Field bindings (one table entry per field) **[R2: validator co-located; ordered slice]**
```go
// binding ties one legal key to one Config field: how to type-check and store a presented value,
// and (optionally) how to validate the effective value.
type binding struct {
    key      string
    set      func(c *Config, layerName string, v any) // type-checks v (panics) and stores it
    validate func(c *Config) error                    // nil-safe: nil means "not validated"
}

// bindings is in Config field-declaration order so iteration (validation) is deterministic.
var bindings = []binding{
    bind("Verbose",    func(c *Config) *bool   { return &c.Verbose },    nil),
    bind("MaxRetries", func(c *Config) *int    { return &c.MaxRetries }, nil),
    bind("LogLevel",   func(c *Config) *string { return &c.LogLevel },   validateLogLevel),
    bind("TimeoutSec", func(c *Config) *int    { return &c.TimeoutSec }, nil),
}

// byKey is built once from bindings in an init-time helper; read-only afterwards.
var byKey = indexBindings(bindings) // map[string]*binding; panics at init on a duplicate key

func bind[T any](key string, field func(*Config) *T, check func(T) error) binding {
    b := binding{key: key}
    b.set = func(c *Config, layerName string, v any) {
        t, ok := v.(T)
        if !ok {
            panic(fmt.Sprintf("config: layer %q, key %q: value has type %T, want %T",
                layerName, key, v, *new(T)))
        }
        *field(c) = t
    }
    if check != nil {
        b.validate = func(c *Config) error { return check(*field(c)) }
    }
    return b
}

func validateLogLevel(s string) error {
    switch s {
    case "", "debug", "info", "warn", "error":
        return nil
    }
    return fmt.Errorf("%w: %q", ErrInvalidLogLevel, s)
}
```
- **[R2]** Panic messages print only the key, the layer and the dynamic type (`%T`), never the
  value, so future secret-bearing fields cannot leak into crash logs. Example:
  `config: layer "env", key "MaxRetries": value has type int64, want int`.
- Generics give a single type-check site; no per-field copy-paste. Adding a field = add the
  struct field + one `bind(...)` line (with an optional validator in the same line). The binding
  tests in §6 fail if the line is forgotten or mis-wired.
- All package-level state is immutable after init → `Resolve` is safe for concurrent use.

## 3. Algorithm

```
resolve(layers):
    var c Config                                  // every field starts at its Go zero value
    for each layer l in order (lowest precedence first):
        for key, v := range l.values:             // nil map → zero iterations
            b, ok := byKey[key]
            if !ok: panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, key))
            b.set(&c, l.name, v)                  // type-checks (may panic), then overwrites
    var errs []error
    for _, b := range bindings:                   // fixed order → deterministic
        if b.validate != nil:
            if err := b.validate(&c); err != nil: errs = append(errs, err)
    return c, errors.Join(errs...)                // [R2] effective c returned even on error;
                                                  // errors.Join(nil...) == nil
```
Map iteration order within a layer does not affect the result: each key in one layer is a
distinct field. Layer order is fixed by the slice. With a single validated field, the joined
error is just the wrapped `ErrInvalidLogLevel` error; `errors.Is` works through `errors.Join`
either way, and multiple future validators all get reported.

## 4. How each rule is satisfied

| Spec rule | Mechanism |
|---|---|
| Present key sets the field, even to `false`/`0`/`""` | Presence = key in map (`range` visits it). Value is never compared to zero; a zero overwrites like any value. |
| Highest-precedence presenting layer wins | Layers applied lowest→highest; each write overwrites; last presenter wins. |
| Absent key has no effect; lower value shows through | Absent keys are never visited; earlier writes remain. |
| No layer presents → Go zero value | `c` starts as `Config{}`. |
| nil layers (public tests pass `nil`) | Ranging over a nil map is a no-op. |
| Only four legal keys | Any other key panics (§5.1), documented in `Resolve`'s doc comment. |
| Wrong Go type panics | `v.(T)` is an exact dynamic-type assertion: `int64`, `float64`, `uint`, a named `type Level string`, `"true"` for bool, an untyped `nil` value all fail → panic. Checked for every present value in every layer, including overridden ones. |
| Only effective `LogLevel` validated | Validators run once, after merging, on the merged `c`. An overridden `"trace"` is never examined. |
| Legal levels `debug`,`info`,`warn`,`error`,`""` | Exact, case-sensitive switch; no trimming (`"INFO"`, `" info"` invalid). |
| Invalid → `errors.Is(err, ErrInvalidLogLevel)` | `fmt.Errorf("%w: %q", ...)` (wrapped again by `errors.Join`); message e.g. `config: invalid log level: "trace"`. |
| `Resolve` "returns the effective Config and an error" | **[R2]** The merged `c` is always returned, also alongside a validation error. |
| `Verbose`, `MaxRetries`, `TimeoutSec` not validated | Their bindings have `validate == nil`; negatives pass through. |
| No env/file/flag I/O | Imports only `errors`, `fmt`. |

## 5. Decisions where the spec leaves room

### 5.1 Unknown keys panic
The spec states "the only legal keys are …" and treats caller contract violations as programmer
errors. Silently ignoring a typo (`"loglevel"`, `"Timeout"`) would let a field fall back to a lower
layer invisibly — the worst failure mode for configuration. Panicking matches the spec's treatment
of wrong types. **[R2]** This is a deliberate contract decision and is stated in `Resolve`'s doc
comment next to the wrong-type panic, so future layer adapters (e.g. one that passes extra keys
through) must filter to legal keys.

### 5.2 Return value on error **[R2: changed]**
Per spec ("returns the effective `Config` and an error"), `Resolve` returns the effective merged
Config together with the error. Doc comment: the Config is diagnostic-only when `err != nil`.

### 5.3 Inputs are never mutated or retained.

### 5.4 Panic vs. error
Panics only for contract violations (unknown key, wrong type); a correctly typed but invalid value
(bad level) is an error.

## 6. Test approach

Commands: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...`, `gofmt -l .`
(expected: no output).

1. **Public suite**: `resolve_public_test.go`, unchanged; all 8 cases pass.
2. **`config_internal_test.go`** (same package, table-driven):
   - All layers nil; all layers empty maps → `Config{}`, nil error (`""` level valid).
   - For each field, a zero value presented by each of file/env/flags over a non-zero lower layer
     → the zero value wins.
   - Absent-passthrough from each lower layer, including gaps (present in defaults only).
   - Middle-layer win (env presents, flags absent → env beats file and defaults).
   - Mixed: each field won by a different layer in one call.
   - Validation:
     - each legal level as effective → nil error;
     - `"trace"`, `"INFO"`, `" info"`, `"warning"` → `errors.Is(err, ErrInvalidLogLevel)`, **[R2]**
       and the returned Config equals the full effective config (offending `LogLevel` plus the
       other three resolved fields from mixed layers);
     - invalid `"trace"` in defaults/file/env overridden by a valid flags level → no error;
     - valid lower level overridden by invalid higher level → error;
     - invalid lower level overridden by `""` → no error;
     - error message contains the offending value quoted.
   - Non-LogLevel fields not validated: `MaxRetries: -1`, `TimeoutSec: -5` → no error, passed through.
   - Panics via helper `mustPanic(t, func())` using `recover`. **[R2]** Each case has exactly one bad
     key, and tests assert only that a panic occurs (optionally that the message contains the key
     name), never the full message — map iteration order would otherwise make them flaky. Cases:
     `int64` MaxRetries; `float64` TimeoutSec; `string` Verbose; `int` LogLevel; named string type
     for LogLevel; untyped `nil` value; unknown key `"loglevel"`; wrong type in a lower layer later
     overridden (still panics).
   - **[R2]** Panic message does not contain the value: e.g. `"MaxRetries": int64(987654)` → the
     recovered message does not contain `"987654"` but does contain `int64`.
   - Inputs not mutated: compare `maps.Clone` copies before/after.
   - **Binding coverage**: via `reflect`, the set of `Config` field names equals the set of
     `bindings` keys, `bindings` is in field-declaration order, and `byKey` has the same size
     (no duplicates).
   - **[R2] Binding wiring**: for each `Config` field (via `reflect`), build a distinct non-zero value
     of the field's type (`bool` → `true`; `int` → `1000+i`; `string` → `"x"+name`), call that
     key's `set` on a zero `Config`, and assert that exactly the field with that name changed (to
     that value) and every other field is still zero. Catches same-type mis-binding such as
     `"TimeoutSec"` → `&c.MaxRetries`.
   - Concurrency: `Resolve` from several goroutines (`sync.WaitGroup`) under `-race`.

## 7. Out of scope
No reading of environment, files or flags; no extra exported API. The fixed API is the entire
exported surface.
