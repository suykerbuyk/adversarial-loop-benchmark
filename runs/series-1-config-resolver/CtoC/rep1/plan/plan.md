# Implementation Plan: Layered Configuration Resolver (`package config`)

Status: round 1. There is no `plan/plan_findings.md`, so this round has no rebuttal.

## 1. Files

The module is already declared in `worktree/go.mod` (`module config`, `go 1.25`). The package lives at the module root next to the shipped `resolve_public_test.go`.

| File | Contents |
|---|---|
| `config.go` | Package doc, `Config`, `ErrInvalidLogLevel`, `Resolve`, internal `resolve(layers ...)` |
| `fields.go` | Field descriptor type, generic `bind` helper, the `fields` table |
| `validate.go` | `validLogLevels` set, `validate(Config) error` |
| `resolve_public_test.go` | Shipped as-is; never edited |
| `resolve_internal_test.go` | Extra tests (§6) |

Only the standard library is used (`errors`, `fmt`). No reflection in production code.

## 2. Public API (exactly as specified)

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

- `ErrInvalidLogLevel` is a package-level `var`, as the spec requires. It is a sentinel, and callers test it with `errors.Is`.
- `Resolve` only calls `resolve(defaults, file, env, flags)`. The variadic internal function takes layers lowest to highest precedence. If a layer is added later, the change is one argument here. The resolution logic stays the same.
- Return contract: on success it returns `(cfg, nil)`. On an invalid log level it returns `(Config{}, err)`, where `err` wraps `ErrInvalidLogLevel`. This follows the Go convention that a value returned alongside a non-nil error is not meaningful. Type and key errors panic (§4.3) and never come back as errors.

## 3. Internal data model

```go
// field describes one Config field and how a layer value is applied to it.
type field struct {
    name  string                       // map key, e.g. "LogLevel"
    apply func(c *Config, v any)       // type-checks v and stores it; panics on wrong type
}

// bind builds a field whose value must have exactly Go type T.
func bind[T any](name string, ptr func(*Config) *T) field {
    return field{
        name: name,
        apply: func(c *Config, v any) {
            t, ok := v.(T)
            if !ok {
                panic(fmt.Sprintf("config: key %q: value %#v has type %T, want %T", name, v, v, *new(T)))
            }
            *ptr(c) = t
        },
    }
}

var fields = []field{
    bind("Verbose",    func(c *Config) *bool   { return &c.Verbose }),
    bind("MaxRetries", func(c *Config) *int    { return &c.MaxRetries }),
    bind("LogLevel",   func(c *Config) *string { return &c.LogLevel }),
    bind("TimeoutSec", func(c *Config) *int    { return &c.TimeoutSec }),
}
```

How this helps maintenance:
- One table drives everything. A new field means one struct field plus one `bind` line. The generic helper ties the type assertion to the destination field's type at compile time, so a field can't be bound with the wrong `T`.
- `fields` is read-only after package init, so concurrent `Resolve` calls are safe.
- The key-to-descriptor lookup is a linear scan over a slice of 4. That is cheap and keeps the order deterministic. If the table grows a lot, a `map[string]field` index can be built at init without changing behaviour.

## 4. Algorithm and how each rule is satisfied

```go
func resolve(layers ...map[string]any) (Config, error) {
    var cfg Config                        // starts at Go zero values
    for _, layer := range layers {        // lowest -> highest precedence
        checkKeys(layer)                  // panics on an unknown key
        for _, f := range fields {
            if v, ok := layer[f.name]; ok {   // comma-ok = presence, not zero-ness
                f.apply(&cfg, v)              // type-check + overwrite
            }
        }
    }
    if err := validate(cfg); err != nil {
        return Config{}, err
    }
    return cfg, nil
}
```

### 4.1 Override rule
- **A present field sets the value, zero values included.** Presence is decided only by the comma-ok map lookup `v, ok := layer[key]`. The value is never compared to its zero value, so `false`, `0`, and `""` overwrite exactly like any other value (public tests 3, 4, 5).
- **The highest-precedence presenting layer wins.** Layers are applied lowest to highest, and each present value overwrites what came before. The last writer is therefore the highest-precedence layer that presents the field (tests 2, 7).
- **An absent field has no effect.** If `ok` is false nothing is written, so the lower layer's value shows through (test 6).
- **If no layer presents a field, it gets the Go zero value.** `cfg` starts as `Config{}` (test 1 covers the other fields being untouched).
- **Nil layers.** Indexing a nil map returns `ok == false` and ranging over it does nothing, so `nil` means "presents nothing". The public tests pass `nil` layers.

### 4.2 Validation
- `validate` runs once, on the final resolved `cfg`, after all layers are applied. A `LogLevel` that a higher layer overrides is never inspected, so `defaults: "trace"` with `flags: "info"` gives no error.
- `var validLogLevels = map[string]struct{}{"debug":{}, "info":{}, "warn":{}, "error":{}, "":{}}`. `""` is legal (test 5). The match is exact and case-sensitive: `"INFO"` and `" info"` are invalid. The spec lists the values literally and asks for no normalisation, so none is done.
- On failure it returns `fmt.Errorf("%w: %q", ErrInvalidLogLevel, cfg.LogLevel)`, so `errors.Is(err, ErrInvalidLogLevel)` is true (test 8) and the message names the bad value.
- A `LogLevel` that no layer presents resolves to `""`, which is legal, so it gives no error.
- `Verbose`, `MaxRetries`, and `TimeoutSec` are not validated. Negative ints are accepted.

### 4.3 Programmer errors (panics)
- **A value of the wrong type panics.** `bind` uses a strict type assertion `v.(T)`. Values that panic include an `int64`/`float64`/`uint` for an `int` field, the string `"true"` for `Verbose`, a named type such as `type Level string`, and an explicit `nil` value (present key, nil interface). This check runs on **every present value in every layer, including values that are later overridden**. The spec says any present value of the wrong type is a programmer error. Checking only the winner would hide bugs depending on what the other layers contain. The panic message gives the key, the value, the actual type, and the expected type.
- **An unknown key panics (judgment call).** The spec says the four keys are "the only legal keys" but does not say what happens with others. The plan treats an illegal key as a programmer error and panics, the same policy as wrong types. Ignoring it silently would turn a typo like `"Loglevel"` or `"logLevel"` into a quiet config bug, which is the worst outcome for a long-lived resolver. Implementation: `checkKeys(layer)` returns early when every key in the layer matches a descriptor (count the matches against `len(layer)`). Otherwise it collects the unknown keys, sorts them so the message is deterministic, and panics with `config: unknown key(s) [..]`.
- These are panics, not returned errors, because the spec requires a panic for wrong types and wants returned errors to mean only invalid input data (`ErrInvalidLogLevel`).

### 4.4 Other guarantees
- Input maps are only read, never modified or kept.
- The result is deterministic: it does not depend on map iteration order, because the code loops over the `fields` slice and sorts unknown keys before reporting them.

## 5. Build hygiene
- `gofmt -l .` must print nothing. `go vet ./...` must be clean. `go build ./...` and `go test -race ./...` must pass.
- Every exported identifier gets a doc comment, plus a package doc comment explaining layer precedence and the present-vs-absent semantics.

## 6. Test approach

**Public suite.** `resolve_public_test.go` runs unmodified and covers the 8 spec cases.

**Internal suite (`resolve_internal_test.go`, table-driven where possible):**
1. All four layers nil (and empty non-nil maps) → `Config{}`, nil error.
2. A full override matrix: for each field, presenting it in each single layer, and in every pair of layers, gives the value from the highest presenting layer.
3. Zero values override in every layer position, for every field type (`false`, `0`, `""` over non-zero values).
4. A zero value in a low layer and an absent key in higher layers → the zero value survives over a non-zero value from an even lower layer.
5. Validation:
   - Every legal level is accepted, including explicit `""`.
   - `"trace"`, `"INFO"`, `"Info"`, `" info"`, `"warning"` are rejected with `errors.Is(err, ErrInvalidLogLevel)`, the returned Config is zero, and the message contains the bad value.
   - An invalid level overridden by a valid one in a higher layer gives no error, tested for each layer position.
   - A valid level overridden by an invalid one gives an error.
   - An invalid level overridden by an explicit `""` gives no error.
6. Panics (helper `mustPanic(t, func())` using `recover`):
   - A wrong type for each field, e.g. `int64(1)`, `1.0`, `"1"` for ints, `1`/`"true"` for Verbose, `1` for LogLevel.
   - A named string type for LogLevel.
   - An explicit `nil` value.
   - A wrong type in a layer that a valid higher layer overrides.
   - An unknown key, including case typos like `"loglevel"`.
   - Panic messages include the key name.
7. Inputs are not mutated: deep-copy the layers before the call and compare with `reflect.DeepEqual` afterwards.
8. Table sync guard: a test uses `reflect` (test-only) to check that every exported `Config` field has exactly one `fields` entry with the same name and a matching Go type. Adding a struct field without binding it then fails CI.
9. A concurrency smoke test: parallel `Resolve` calls under `-race`.

Acceptance gate: `gofmt`, `go vet ./...`, and `go test -race ./...` all clean in `worktree/`.
