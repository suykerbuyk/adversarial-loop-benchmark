# Implementation Plan: Layered Configuration Resolver (`package config`)

Status: round 1. No `plan_findings.md` existed when this was written, so there is no rebuttal file.

## 1. Files

Everything goes in `worktree/`. The module is `config` (go 1.25); toolchain is go1.27.2.

| File | Purpose |
|---|---|
| `config.go` | `Config`, `ErrInvalidLogLevel`, `Resolve`, the field table, and the layer walk |
| `resolve_public_test.go` | Shipped public tests (already present). Left unchanged. |
| `resolve_test.go` | Extra tests from §6, in `package config` (internal, so it can check the field table) |

The package has no dependencies outside the standard library (`errors`, `fmt`, `sort`; `reflect` in tests only).

## 2. Public API (exactly as the spec fixes it)

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

- `ErrInvalidLogLevel` is declared as a `var` of type `error` (the spec's signature) and set with `errors.New`. Callers match it with `errors.Is`.
- No other exported identifiers. Keeping the surface small lets the internals change later without breaking callers.

## 3. Internal data model

### 3.1 Layers: an ordered list, lowest precedence first
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags": used only in panic messages
    values map[string]any // may be nil; nil means nothing is presented
}
```
`Resolve` builds `[]layer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}}` and passes it to an unexported `resolve(layers []layer) (Config, error)`. Adding a fifth layer later means one new parameter and one new slice element. The merge logic does not change.

### 3.2 Fields: a single declarative table
```go
type field struct {
    key   string                                     // map key, e.g. "MaxRetries"
    apply func(c *Config, layerName string, v any)   // type-checks v and stores it in c
}

func newField[T any](key string, set func(*Config, T)) field {
    return field{key: key, apply: func(c *Config, layerName string, v any) {
        t, ok := v.(T)
        if !ok {
            panic(fmt.Sprintf("config: layer %q: key %q has type %T, want %T", layerName, key, v, *new(T)))
        }
        set(c, t)
    }}
}

var fields = []field{
    newField("Verbose",    func(c *Config, v bool)   { c.Verbose = v }),
    newField("MaxRetries", func(c *Config, v int)    { c.MaxRetries = v }),
    newField("LogLevel",   func(c *Config, v string) { c.LogLevel = v }),
    newField("TimeoutSec", func(c *Config, v int)    { c.TimeoutSec = v }),
}
var fieldByKey = map[string]field{...} // built from `fields` in init or a helper. Used to detect unknown keys.
```
Why this shape:
- To add a field you add one struct field and one table line. The generic constructor makes the type check in the table match the setter's type, so a key cannot be given one type in the check and another in the assignment.
- No reflection at runtime. Behaviour is explicit, fast, and easy to read.
- A test (§6) uses reflection to check that the table and `Config` list the same field names, so adding a field without a table entry fails CI.

### 3.3 Log-level validation
```go
func validLogLevel(s string) bool {
    switch s {
    case "debug", "info", "warn", "error", "":
        return true
    }
    return false
}
```
The comparison is exact: case-sensitive, with no trimming. `"INFO"` and `" info"` are invalid because the spec lists exact values.

## 4. Algorithm (`resolve`)

```
var c Config                         // every field starts at its Go zero value (rule: no layer presents it)
for _, l := range layers {           // lowest to highest precedence
    checkUnknownKeys(l)              // panics on any key not in fieldByKey (sorted, deterministic message)
    for _, f := range fields {       // fixed table order, deterministic
        if v, ok := l.values[f.key]; ok {   // presence is the comma-ok form, never a zero-value test
            f.apply(&c, l.name, v)          // type check (panic) and then assign; a later layer overwrites an earlier one
        }
    }
}
if !validLogLevel(c.LogLevel) {
    return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
}
return c, nil
```

## 5. How each rule is satisfied

| Spec rule | Mechanism |
|---|---|
| Present key sets the field, including `false`, `0`, `""` | Presence is decided only by `v, ok := m[key]`. The value is never compared to its zero value. |
| Highest presenting layer wins | Layers are applied from lowest to highest. Each presented value overwrites the earlier one, so the last presenter (the highest) remains. |
| Absent key has no effect; the lower value shows through | If `ok` is false, nothing is assigned, so the value from lower layers stays. |
| No layer presents the field: Go zero value | `c` starts as `Config{}`. |
| `nil` layer maps | Reading from a nil map returns `ok == false`, so a nil map presents nothing. No special case needed. |
| Wrong Go type panics | `v.(T)` uses an exact type assertion. `int64(3)`, `float64(3)`, a named `type B bool`, and a present `nil` value all fail and panic with the layer, key, actual type, and wanted type. |
| Type check covers **every** presented value, overridden ones included | The walk visits every layer and every presented key. It never stops early at the top presenter, so a wrong-typed value in `defaults` panics even when `flags` overrides it. The spec says any present value of the wrong type is a programmer error. |
| Only the four legal keys | Any other key (for example the typo `"Loglevel"`) is the same class of programmer error, so it also panics. Unknown keys are sorted so the message is deterministic. Ignoring a typo silently would hide the very bug the "only legal keys" rule targets. |
| Validate only the effective `LogLevel` | Validation runs once, after the merge, on `c.LogLevel`. Overridden values are never inspected for validity. They are only type-checked. |
| Legal levels include explicit `""` | `""` is a case in `validLogLevel`. If no layer presents `LogLevel`, the effective value is `""`, so that case is valid too. |
| Invalid effective level gives an error matching `errors.Is(err, ErrInvalidLogLevel)` | `fmt.Errorf("%w: %q", ...)` wraps the sentinel, and the message includes the bad value for diagnosis. |
| `Verbose`, `MaxRetries`, `TimeoutSec` not validated | No checks on them. Negative values pass through. |
| Order between panic and error | All type and key checks happen during the merge, before validation. A programmer error therefore always panics, even if the effective level would also be invalid. |
| Value returned with an error | `Config{}`. This follows the Go convention that results are not meaningful when `err != nil`, and it stops callers from using a half-valid config. |
| No environment, file, or CLI access | The package imports only `errors`, `fmt`, and `sort`. |
| Inputs are not mutated | Maps are only read. |

Panics are deliberate here. The spec requires them for programmer errors, and `Resolve` does not recover from them.

## 6. Test approach

Run: `go build ./... && go vet ./... && go test -race ./...` (and `gofmt -l .` must print nothing).

1. **Public suite** (`resolve_public_test.go`): all 8 cases, unchanged.
2. **`resolve_test.go`**: table-driven tests with `t.Run` subtests.
   - Precedence matrix: for each field, each subset of layers presenting it gives the highest presenter's value. Zero-valued presenters (`false`, `0`, `""`) override non-zero lower values.
   - All four layers nil, or all empty maps: result is `Config{}` and `err == nil`.
   - Mixed fields from different layers combine correctly (for example `Verbose` from `flags`, `MaxRetries` from `env`, `TimeoutSec` from `defaults`).
   - Every legal level (`debug`, `info`, `warn`, `error`, `""`) is accepted as the effective value.
   - Invalid effective levels `"trace"`, `"INFO"`, `" info"` give `errors.Is(err, ErrInvalidLogLevel)` and a zero `Config`.
   - An overridden invalid level is not validated: `defaults{"LogLevel":"trace"}` with `flags{"LogLevel":"info"}` gives no error. The same holds when `""` overrides it.
   - An invalid lower level shows through when higher layers are absent: `file{"LogLevel":"bogus"}`, with `env` and `flags` absent, gives the error.
   - Panics (using a `mustPanic(t, func())` helper with `recover`):
     - wrong types for each key: `"Verbose": "true"`, `"MaxRetries": int64(1)`, `"MaxRetries": 1.0`, `"LogLevel": 1`, `"TimeoutSec": "30"`;
     - a present untyped `nil` value;
     - a named type (`type myBool bool`);
     - a wrong type in a lower layer that a higher layer overrides with a correct type;
     - an unknown key (`"Loglevel"`).
     The panic message should name the layer and the key.
   - Inputs are not mutated: deep-copy the maps before the call and compare with `reflect.DeepEqual` afterwards.
   - **Table-sync guard**: reflect over `Config`'s fields and check that the set of names equals the set of `fields[i].key`, with no duplicate keys. Also check that each table entry's type matches the struct field's type. Do this by applying a zero value of the struct field's type and confirming it does not panic.
3. **Example** (`ExampleResolve`) in the test file shows typical use and is checked by `go test`.

## 7. Extensibility notes (the "lives for years" requirement)
- New field: one struct field and one `newField` line. The sync test enforces this.
- New field that needs validation: add an optional `validate func(Config) error` hook to the table, or a post-merge validation list. This is not added now, to avoid speculative generality. The single post-merge validation step is where it would go.
- New layer: one parameter and one slice element. Since the API is fixed, a future variadic or `ResolveLayers` entry point can wrap the same `resolve` function.
