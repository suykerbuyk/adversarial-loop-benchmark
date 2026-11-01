# Implementation Plan: Layered Configuration Resolver (`package config`)

Round 2. This revision responds to `plan/plan_findings.md`. Point-by-point answers are in
`plan/plan_rebuttal.md`. Items changed in this round are marked **[R2]**.

## 0. Context
- Module: `worktree/go.mod` declares `module config`, `go 1.25`. The package sits at the module root
  (`package config`), beside the shipped `resolve_public_test.go`. That file is identical to
  `instrument/resolve_public_test.go`.
- Files to create, all in `worktree/`:
  - `config.go`: `Config`, `ErrInvalidLogLevel`, `Resolve`, plus the field, layer, and validator machinery.
  - `config_test.go`: extra internal tests (§6). The public test file stays unchanged.
- Only the standard library is used (`errors`, `fmt`, `reflect` at init only, `slices`/`sort`, `strconv`, `strings`).

## 1. Public API (fixed by the spec; nothing else is exported)
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
- `ErrInvalidLogLevel` is a `var` of static type `error`, which is what `errors.New` returns. It is a
  sentinel: never reassigned, and callers match it with `errors.Is`.
- `Resolve` keeps the exact four-parameter signature the spec fixes. It is a thin wrapper:
  `return resolve(layer{"defaults", defaults}, layer{"file", file}, layer{"env", env}, layer{"flags", flags})`.
- **[R2] Layer growth story.** The signature of `Resolve` is frozen and will never change. If a fifth
  layer is needed later, it gets a *new* exported entry point, for example
  `ResolveLayers(layers ...Layer)` or a `Resolve5`-style function. That function forwards to the same
  unexported variadic `resolve(...layer)`, so the merge logic stays untouched and existing callers
  keep compiling.
- **[R2] Documented error contract** (doc comment on `Resolve`):
  - Layers are applied in precedence order.
  - Presence is defined by the key being in the map. Present zero values override lower layers.
    A field that no layer presents keeps its Go zero value.
  - A present value of the wrong type, an unknown key, or an explicit `nil` value causes a panic.
  - Validation applies only to the effective values.
  - *When err != nil, the returned Config is the zero `Config{}`. Callers must not rely on any
    partially merged value.* This is a deliberate, documented contract. Any future partial-result
    API would be a separate function.

## 2. Internal data model

### 2.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags"; used only in panic messages
    values map[string]any // nil is legal: every field absent
}
```
- Layers are ordered from lowest to highest precedence.
- A nil map behaves as an empty map. Reading a nil map is safe in Go, so the public tests' `nil`
  arguments need no special case.

### 2.2 Field descriptors (single source of truth)
```go
type field struct {
    key    string                      // map key, e.g. "MaxRetries"
    typ    string                      // expected Go type name for diagnostics, e.g. "int"
    assign func(c *Config, v any) bool // type-asserts v and stores it; false if wrong type
}

func bind[T any](key string, ptr func(*Config) *T) field {
    return field{
        key: key,
        typ: reflect.TypeFor[T]().String(),
        assign: func(c *Config, v any) bool {
            t, ok := v.(T)
            if !ok {
                return false
            }
            *ptr(c) = t
            return true
        },
    }
}

var fields = []field{
    bind("Verbose",    func(c *Config) *bool   { return &c.Verbose }),
    bind("MaxRetries", func(c *Config) *int    { return &c.MaxRetries }),
    bind("LogLevel",   func(c *Config) *string { return &c.LogLevel }),
    bind("TimeoutSec", func(c *Config) *int    { return &c.TimeoutSec }),
}

// [R2] Index by position, never by pointer into the slice.
var fieldIndex = indexFields(fields) // map[string]int

func indexFields(fs []field) map[string]int {
    idx := make(map[string]int, len(fs))
    for i, f := range fs {
        if _, dup := idx[f.key]; dup {
            panic("config: duplicate field key " + strconv.Quote(f.key)) // [R2] fail at init
        }
        idx[f.key] = i
    }
    return idx
}
```
- **[R2]** The lookup map holds indices (`map[string]int`), and the code never takes the address of
  a slice element. Neither the merge loop nor `checkKeys` needs anything beyond key membership, so
  `checkKeys` only uses `_, ok := fieldIndex[k]`.
- **[R2]** If the table contains a duplicate key, package initialization panics. Every `go test`
  run would then fail immediately, so the walk-order and map-lookup views of the table can never
  disagree.
- Adding a field means one struct field plus one `bind(...)` line. Generic `bind` makes the
  compiler check that the struct field type and the asserted `T` agree.
- `reflect` runs only at init, to produce type names for diagnostics. There is no reflection on the
  hot path.
- All package-level tables are built at init and are read-only afterwards. `Resolve` is safe for
  concurrent use.

### 2.3 Validators [R2]
```go
// validator checks one aspect of the effective Config. Each validator wraps its
// own sentinel with %w so callers can errors.Is on it.
type validator func(Config) error

var validators = []validator{
    validateLogLevel,
}

func validateLogLevel(c Config) error {
    switch c.LogLevel {
    case "debug", "info", "warn", "error", "":
        return nil
    }
    return fmt.Errorf("%w: %q", ErrInvalidLogLevel, c.LogLevel)
}

func validate(c Config) error {
    var errs []error
    for _, v := range validators {
        if err := v(c); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(errs...) // nil when errs is empty
}
```
- Adding a validated field later means adding one exported sentinel, one `validateX` function, and
  one entry in `validators`. The error path in `resolve` never changes.
- `errors.Join` reports every failing check at once. `errors.Is` matches each joined sentinel
  independently, and with a single failure the message is exactly that validator's message.

## 3. Algorithm
```
resolve(layers ...layer) (Config, error):
    var cfg Config                          // all fields start at Go zero values
    for each layer L, lowest → highest:
        checkKeys(L)                        // panic on any key not in fieldIndex (§4.3)
        for each f in fields (table order, deterministic):
            v, present := L.values[f.key]   // comma-ok: presence = key exists
            if !present { continue }        // absent: lower value shows through
            if !f.assign(&cfg, v) {         // present: overwrite, even with false/0/""
                panic(wrongType(L.name, f, v))
            }
    if err := validate(cfg); err != nil {   // effective values only
        return Config{}, err
    }
    return cfg, nil
```

## 4. How each rule is satisfied

### 4.1 Override rule
- **Present sets, zero values included.** Presence comes from `_, ok := m[key]`, never from comparing
  against a zero value. So `false`, `0`, and `""` are written like any other value (public tests 3, 4, 5).
- **Highest presenting layer wins.** Layers are applied bottom-up and each present value overwrites
  the earlier one. The last writer is the highest-precedence presenter (tests 2, 7).
- **Absent has no effect.** Missing keys are skipped, so whatever lower layers wrote stays in `cfg` (test 6).
- **No layer presents a field → Go zero value.** `cfg` starts as `Config{}` (tests 1, 6).
- **Nil maps** count as fully absent layers.

### 4.2 Wrong Go type panics
- Every present value in every layer is type-checked, including values that a higher layer later
  overrides. The spec makes no exception for overridden values. That is why the algorithm walks all
  layers bottom-up and never short-circuits top-down.
- The check is a strict assertion to the exact type (`bool`, `int`, `string`). These all panic:
  - `int64(3)`, `float64(3)`, `uint(3)`, or `"3"` given for an int field.
  - A named type such as `type Level string` given for `LogLevel`.
- **[R2] Nil is a chosen, documented contract:** a present key whose value is `nil` panics.
  - *Untyped nil* (`map[string]any{"LogLevel": nil}`): the key is present, so this is not
    "absent". The value is not a `string`, so it is a wrong type. The only way to say "absent" is
    to leave the key out.
  - *Typed nil* (e.g. `(*int)(nil)`, `(*string)(nil)`): its dynamic type is a pointer type, which
    is not the field type, so it is a wrong type.
  - The `Resolve` doc comment states this contract. If a future field has a pointer or interface
    type, "nil means absent" will still *not* be adopted. Presence stays tied solely to key
    existence, so one rule covers every field.
- The panic value is a descriptive string, e.g.
  `config: layer "env": key "MaxRetries" must be int, got int64`. Tests check only the layer name,
  the key, and the type names as substrings, not the exact wording (§6).
- Panics happen before validation. A call with both a type error and an invalid level panics; it
  does not return `ErrInvalidLogLevel`.

### 4.3 Illegal keys
- The spec says the only legal keys are the four field names. An unknown key, including a case
  variant like `"loglevel"`, is the same class of programmer error as a wrong type, so it panics.
  Ignoring it silently would hide typos like `"Timeout"`.
- `checkKeys(L)` iterates over the map and collects every key `k` where `fieldIndex[k]` is missing.
  If any are found, it sorts them, so the output does not depend on map iteration order, and panics
  with a message naming the layer and the sorted unknown keys, e.g.
  `config: layer "file": unknown key(s) ["Timeout"]`.

### 4.4 Validation
- `validate` runs once, after merging, on the effective `Config`. A presented `LogLevel` that a
  higher layer overrides never reaches a validator. Example: defaults `"trace"`, flags `"info"` →
  result `"info"`, no error.
- `validateLogLevel` accepts exactly `"debug"`, `"info"`, `"warn"`, `"error"`, and `""`. The empty
  string is legal both when presented explicitly and as the zero value when nothing presents
  `LogLevel`. Matching is exact and case-sensitive, so `"INFO"` and `" info"` are invalid.
- On failure the error is `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)`, possibly inside
  `errors.Join`, and `errors.Is(err, ErrInvalidLogLevel)` is true (test 8).
- On error, `Resolve` returns `Config{}`. This is the documented contract (§1).
- `Verbose`, `MaxRetries`, and `TimeoutSec` have no validators. Negative ints are accepted.

### 4.5 Other properties
- Input maps are only read. They are never mutated or retained.
- There is no access to the environment, files, or flags, and no `os` or `flag` import.
- There is no mutable package state, so concurrent calls are safe.

## 5. Maintainability ("gains more layers and fields")
- **New field:** one `Config` field plus one `bind` line. Key checking, type checking, presence
  semantics, and diagnostics all follow from the table. Tests enforce struct/table sync and key
  uniqueness, and init panics on duplicates.
- **New validated field:** one sentinel, one `validateX`, and one `validators` entry (§2.3).
- **New layer:** a new exported entry point that forwards to variadic `resolve`. `Resolve` itself
  is never changed (§1).
- Every exported identifier has a doc comment covering precedence, presence, panics (including the
  nil contract), and the error/zero-Config contract.

## 6. Test approach
Commands: `gofmt -l .` (expect empty output), `go vet ./...`, `go build ./...`, `go test -race ./...`.

1. **Public suite:** the shipped `resolve_public_test.go`, unchanged (8 cases).
2. **Internal table-driven tests** in `config_test.go`:
   - Override semantics for every field:
     - For each field, a present zero value in each higher layer overrides a non-zero value below it.
     - An absent key in higher layers lets the lower value through.
     - Mixed cases where different layers win different fields.
   - All layers nil, or all empty → `Config{}` and a nil error.
   - Each legal LogLevel (`debug`, `info`, `warn`, `error`, `""`) → no error. Also the case where
     LogLevel is never presented.
   - Invalid effective levels (`"trace"`, `"INFO"`, `" info"`, `"warning"`):
     - `errors.Is(err, ErrInvalidLogLevel)` is true.
     - The result is `Config{}`.
     - `err.Error()` contains the bad value.
   - An overridden invalid level is not validated: defaults `"trace"` + file `"debug"` → no error.
     The same holds when it is overridden by an explicit `""`.
   - An invalid level in a lower layer that shows through → error.
   - **[R2] Validator hook:** a test temporarily adds a stub validator that wraps a test-local
     sentinel and restores `validators` via `t.Cleanup` (not parallel). Then:
     - Both sentinels satisfy `errors.Is` when both checks fail.
     - Only the stub sentinel matches when LogLevel is valid.
     - The result is `Config{}`.
   - Panic tests, using a `mustPanic(t, fn) string` helper built on `recover`:
     - Wrong type for each key:
       - `int64`, `float64`, `uint`, `string` for the int fields.
       - `int`, `string` for `Verbose`.
       - `[]byte` and a named string type for `LogLevel`.
     - **[R2]** Untyped `nil` value and typed nil (`(*int)(nil)`, `(*string)(nil)`, `(*bool)(nil)`)
       for each relevant key → panic.
     - A wrong type in an overridden lower layer still panics.
     - A wrong type combined with an invalid level panics instead of returning an error.
     - Unknown keys and case variants panic.
     - **[R2]** Panic assertions use substrings only:
       - The layer name and the offending key appear.
       - For type errors, the expected and actual type names appear.
       - For several unknown keys, they appear in sorted order (checked by comparing
         `strings.Index` positions).
       - The surrounding wording is not asserted.
   - Inputs are not mutated: deep-copy before the call, then compare with `reflect.DeepEqual`.
   - Table/struct sync: reflection over `Config` checks that every exported field has exactly one
     `fields` entry with a matching key and type name, and the reverse.
   - **[R2]** Table key uniqueness:
     - `len(fieldIndex) == len(fields)`.
     - `indexFields` called directly on a slice with a duplicate key panics.
   - Concurrency: parallel `Resolve` calls under `-race`.
