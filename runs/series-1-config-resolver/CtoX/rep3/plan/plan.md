# Implementation Plan — Layered Configuration Resolver (`package config`)

Round 2. `plan/plan_findings.md` does not exist on disk. However, the round-1 adversary transcript (`turns/p1_adv.json`) records one finding: *major: unknown keys should not panic*. That finding is answered in `plan_rebuttal.md`. The verdict there is rebutted, with one change to the plan: the unknown-key policy now lives in a single, documented place (§3, §4) so it can be changed by editing one function.

## 0. Where things go
- Module root: `worktree/` (`go.mod`: `module config`, `go 1.25`). The package is `config` and lives at the module root next to the shipped `resolve_public_test.go`.
- Files to add:
  - `config.go`: the public API (`Config`, `ErrInvalidLogLevel`, `Resolve`), the internal layer type, and the resolve loop.
  - `fields.go`: the field descriptor table (one entry per `Config` field) and the generic descriptor constructor.
  - `validate.go`: log-level validation for the effective config.
  - `resolve_test.go`: extra internal tests (§5). The public test file stays exactly as shipped.
- Only the standard library is used (`errors`, `fmt`, `reflect` (just to get type names for panic messages), `sort`).

## 1. Public API (fixed by the spec; no additions)
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
- Each item gets a doc comment covering precedence, what counts as present or absent, when it panics, and what errors it returns.
- No other exported identifiers. Keeping the surface small makes it easy to evolve later.

## 2. Internal data model

### 2.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags", used only in panic messages
    values map[string]any // may be nil; a nil map means every field is absent
}
```
`Resolve` builds `[]layer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}}` in ascending precedence order and passes it to an unexported `resolve(layers []layer) (Config, error)`. To add a layer later, you add one parameter and one slice element; the algorithm doesn't change.

### 2.2 Field descriptors (one table instead of per-field code)
```go
type field struct {
    key      string                          // map key, e.g. "MaxRetries"
    typeName string                          // expected Go type, for panic messages
    assign   func(c *Config, v any) bool     // type-checks v and stores it; false on wrong type
}

func fieldOf[T any](key string, ptr func(*Config) *T) field {
    return field{
        key:      key,
        typeName: reflect.TypeFor[T]().String(),
        assign: func(c *Config, v any) bool {
            t, ok := v.(T)
            if ok { *ptr(c) = t }
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
```
- The generic constructor ties the expected type to the struct field's type at compile time. A descriptor that says `int` for a `bool` field will not compile.
- Adding a field means one struct field plus one table line. A reflection test (§5) fails if the two get out of sync.
- `v.(T)` is an exact dynamic-type check. `int64(3)`, `float64(3)`, `uint(3)`, a named type like `type Level string`, and an untyped `nil` value are all wrong types for their keys and cause a panic.

## 3. Algorithm
```
resolve(layers):
    var c Config                                // every field starts at its Go zero value (rule: no layer presents it)
    for each layer L in ascending precedence:
        matched := 0
        for each f in fields (table order, deterministic):
            v, present := L.values[f.key]       // comma-ok: present even when v is false/0/""
            if !present { continue }            // absent: lower value shows through
            matched++
            if !f.assign(&c, v) {
                panic(fmt.Sprintf("config: layer %q: key %q: want %s, got %T (%v)",
                                  L.name, f.key, f.typeName, v, v))
            }
        if matched != len(L.values):
            rejectUnknownKeys(L)                // single policy point; panics naming layer + first unknown key (sorted)
    if err := validate(c); err != nil { return Config{}, err }
    return c, nil
```
Notes:
- Layers are applied in a single pass from lowest to highest precedence, and each present value overwrites the last. The final value of each field therefore comes from the highest layer that presents it. If no layer presents it, the field keeps its zero value.
- Presence is checked with comma-ok (`v, ok := m[k]`). It is never inferred from the value, which is what lets explicit `false`, `0`, and `""` override.
- Every present value in every layer is type-checked, including values that a higher layer later overrides. The spec says a wrong-type present value "must panic" with no exception for overridden values, so a latent type error cannot hide just because it gets shadowed.
- The input maps are only read, never mutated or retained.
- Unknown keys panic. That is a judgment call, explained in §4.

## 4. How each stated rule is satisfied

| Spec rule | Mechanism |
|---|---|
| Layer = `map[string]any`; key present ⇒ presented field, even if `false`/`0`/`""` | comma-ok lookup; the value is never compared against zero |
| Absent key ⇒ no effect, lower value shows through | `continue` on `!present`; `c` keeps the value from lower layers |
| Highest-precedence presenting layer wins | single ascending pass; later writes overwrite earlier ones |
| No layer presents ⇒ Go zero value | `var c Config` start state |
| nil layer maps (used by the public tests) | reading a nil map returns `ok == false`, so every field is absent and `len(nil) == 0` |
| Wrong Go type ⇒ panic | exact type assertion in `assign`; panic with a descriptive message naming the layer, key, expected type and actual type. Applies to every layer, including overridden values. Untyped `nil` is a wrong type. |
| Only the four legal keys | Any other key (e.g. a typo such as `"Loglevel"`) panics as a programmer error. The spec says these are "the only legal keys", and silently ignoring a misspelled key would leave the config wrong with no signal. A key that is unknown in any layer always panics, including when the map is otherwise valid. The panic reports the lexicographically first unknown key, so messages are deterministic. This does not change the API signature or any behavior the spec states: every legal-key input the spec describes still resolves exactly as specified. The policy is confined to `rejectUnknownKeys` and its doc comment. That function is the only place that would need to change if the project later decides to ignore unknown keys (see `plan_rebuttal.md`). |
| Validate only the effective `LogLevel` | `validate` runs once, on the resolved `Config`, after all layers are merged. An overridden `"trace"` in a lower layer is never inspected. |
| Legal levels `debug`, `info`, `warn`, `error`, `""` | `switch c.LogLevel { case "", "debug", "info", "warn", "error": return nil }`. The match is exact and case-sensitive (`"INFO"` and `" info"` are invalid), because the spec lists the exact strings. |
| Invalid effective level ⇒ `errors.Is(err, ErrInvalidLogLevel)` | `return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)`. The error wraps the sentinel and includes the offending value. |
| `Verbose`, `MaxRetries`, `TimeoutSec` not validated | no checks; negative ints, for example, are accepted |
| Return value on error | `Config{}` plus the error, following the usual Go convention that other results are meaningless when `err != nil`. This is documented on `Resolve`. |
| No env/file/flag I/O | the package does no I/O; the layer names only label precedence (and appear in panic messages) |
| `go build` / `go vet` clean | standard code only; no unkeyed composite literals of foreign types, no shadowed sentinel |

Why panics are not errors: the spec says type errors are programmer errors and must panic. The returned `error` is reserved for the single data-dependent failure, an invalid effective log level, so callers only need to handle `ErrInvalidLogLevel`.

## 5. Test approach
All tests run with `go test ./...`, plus `go vet ./...` and `gofmt -l .` (which should print nothing). Optionally `go test -race`.

1. **Public suite**: `resolve_public_test.go` unchanged (the 8 spec cases).
2. **`resolve_test.go`** (same package), table-driven with `t.Run` names:
   - Override semantics:
     - every field set to its zero value in `flags` overrides non-zero values in `defaults` (all four fields at once);
     - an absent field at every layer gives the zero value;
     - all four layers nil gives `Config{}` and no error;
     - empty non-nil maps behave like nil;
     - interleaved presence, e.g. `defaults` and `env` present a field while `file` and `flags` don't, so the `env` value wins;
     - each field independently comes from a different layer.
   - Precedence for each field: present in every subset of the layers (the 16 subsets × 4 fields can be generated in a loop). The expected value is from the highest present layer, or zero if none.
   - Validation:
     - each legal level (`""`, `debug`, `info`, `warn`, `error`) gives no error;
     - `trace`, `INFO`, `" info"`, `"warning"` give an error matching `errors.Is(err, ErrInvalidLogLevel)`, and the returned `Config` is the zero value;
     - an invalid level in a lower layer overridden by a valid one (including `""`) gives no error;
     - an invalid level in `defaults` with no override gives an error;
     - the error message contains the offending value.
   - Panics (helper `mustPanic(t, func())` using `recover`):
     - wrong type for each key (`"Verbose": "true"`, `"MaxRetries": int64(3)`, `"MaxRetries": 3.0`, `"LogLevel": 5`, `"TimeoutSec": "30"`);
     - untyped `nil` value;
     - a wrong type in a lower layer that a valid higher layer overrides still panics;
     - an unknown key panics, and the panic message names the key and the layer.
   - No mutation: inputs are deep-compared before and after `Resolve`.
   - Table/struct sync guard: use reflection over `Config` to check that the set of `reflect.TypeFor[Config]()` field names equals the set of `fields[i].key`, and that each descriptor's `typeName` equals the struct field's type. This keeps the table from drifting as fields are added.
3. Acceptance gate: `go build ./... && go vet ./... && go test ./...` all pass in `worktree/`.

## 6. Extensibility notes (documented in code comments)
- **New field:** add the struct field and one `fieldOf` line. If you forget either, the sync test fails. If the field needs validation, extend `validate` (keep it operating on the effective config only).
- **New layer:** add a parameter to `Resolve` in precedence position and one slice element. `resolve` is layer-count-agnostic.
- **New error kinds:** add a new sentinel and wrap it with `%w`. Don't change `ErrInvalidLogLevel`, since callers depend on `errors.Is`.
