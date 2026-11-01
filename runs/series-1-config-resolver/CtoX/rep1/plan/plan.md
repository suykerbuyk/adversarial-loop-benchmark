# Implementation Plan: Layered Configuration Resolver (`config`)

Revision 3. This version addresses the round-2 adversary findings in `plan/plan_findings.md`
(one minor, two nits). The point-by-point response is in `plan/plan_rebuttal.md`. Changes from
revision 2:
- **[minor]** T13 now checks behavior, not just accepted types. Each row's setter is called on a
  `Config` whose fields all hold distinct sentinel values, and the test asserts that exactly the
  field named by the row's key changed, to exactly the value passed in. Swapped `MaxRetries` and
  `TimeoutSec` setters now fail the test (§6.2 T13). The setter design also ties key, type, and
  target together in one expression (§3.2).
- **[nit]** The package-level `fields` slice is gone. The table is built by
  `fieldTable() []field`, which returns a new slice literal on every call. There is no
  package-level table left to mutate (§3.2, §4).
- **[nit]** No blocker or major issue was raised, so nothing needed to change for this item.

Revision 2 (kept): the log-level set is a `switch` and unknown keys are found by a count check, so
there are no package-level maps. The `Resolve` doc comment states the full panic contract.

## 1. Layout

Module `config` (from `worktree/go.mod`, go 1.25). The package sits at the module root next to the
shipped `resolve_public_test.go`.

| File | Contents |
|---|---|
| `config.go` | Package doc, `Config`, `ErrInvalidLogLevel`, `Resolve`, internal `resolve` |
| `fields.go` | The field table (one descriptor per `Config` field) and typed setter helpers |
| `loglevel.go` | The set of legal log levels and `validateLogLevel` |
| `resolve_public_test.go` | Shipped as-is, unchanged |
| `resolve_internal_test.go` | Extra tests (section 6) |

Standard library only (`errors`, `fmt`).

## 2. Public API (exactly as the spec fixes it)

```go
// Config is the effective configuration produced by Resolve.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// ErrInvalidLogLevel is matched (via errors.Is) by the error Resolve returns when the
// effective LogLevel is not one of "debug", "info", "warn", "error", or "".
var ErrInvalidLogLevel = errors.New("config: invalid log level")

// Resolve combines the layers in increasing precedence: defaults < file < env < flags.
//
// A key present in a layer sets that field, even when the value is a zero value
// (false, 0, ""). A key absent from a layer (or a nil layer) leaves the lower value in
// place. A field that no layer presents gets its Go zero value.
//
// Only the effective LogLevel is validated. If it is not "debug", "info", "warn",
// "error", or "", Resolve returns an error matching ErrInvalidLogLevel.
//
// Resolve panics on programmer errors, in any layer, even one that a higher layer
// overrides:
//   - a value whose dynamic type is not exactly the field's type (bool for Verbose,
//     int for MaxRetries and TimeoutSec, string for LogLevel), including a nil value;
//   - a key other than "Verbose", "MaxRetries", "LogLevel", "TimeoutSec". Unknown keys
//     are never ignored and never reported as errors, so a typo cannot silently drop
//     a setting.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

Nothing else is exported. The signature stays fixed even when more layers are added later (see 5).

## 3. Internal data model

### 3.1 Layers
```go
type layer struct {
    name   string         // "defaults", "file", "env", "flags"; used only in panic messages
    values map[string]any // may be nil, which means every field is absent
}
```
`Resolve` builds `[]layer{{"defaults", defaults}, {"file", file}, {"env", env}, {"flags", flags}}`
from lowest to highest precedence and calls `resolve(layers []layer) (Config, error)`. Precedence
comes only from position in the slice, so adding a layer later means adding one slice entry (and
a parameter or a new entry point). The merge logic does not change.

### 3.2 Field table
```go
// field describes one Config field: its layer-map key and how to assign it.
type field struct {
    key string                      // map key, e.g. "MaxRetries"
    set func(c *Config, v any) bool // asserts the exact Go type, assigns, reports ok
}

// fieldTable returns the descriptors for every Config field, in declaration order.
// It returns a new slice on each call, so no shared table exists that could be
// modified by accident.
func fieldTable() []field {
    return []field{
        {"Verbose",    func(c *Config, v any) bool { return assign(v, &c.Verbose) }},
        {"MaxRetries", func(c *Config, v any) bool { return assign(v, &c.MaxRetries) }},
        {"LogLevel",   func(c *Config, v any) bool { return assign(v, &c.LogLevel) }},
        {"TimeoutSec", func(c *Config, v any) bool { return assign(v, &c.TimeoutSec) }},
    }
}

// assign stores v in *dst if v's dynamic type is exactly T, and reports whether it did.
func assign[T any](v any, dst *T) bool {
    t, ok := v.(T)
    if ok {
        *dst = t
    }
    return ok
}
```
Binding key, type, and target:
- In each row, the expression `&c.X` sets both the destination and, through type inference, the
  required type `T`. A row cannot assert one type and write a field of a different type: that
  would not compile.
- The only mistake the compiler cannot catch is pairing a key with the wrong field of the same
  type, such as `"MaxRetries"` with `&c.TimeoutSec`. T13 (§6.2) catches this by running each
  setter and checking which field changed.
- No `reflect` is used at run time, and there is no per-field type switch. Adding a field means
  adding one struct field and one row.

Why a function and not a package variable (round-2 nit):
- Go has no immutable composite constants. A package-level slice, or even an array, can still be
  written by later code in the package.
- `fieldTable()` returns a new slice literal on each call, so there is no shared table to change.
  Any change to behavior has to be an edit to the literal itself.
- The row closures capture nothing, so they are static functions. The only cost is one small
  slice allocation per `Resolve` call, and `resolve` calls `fieldTable()` once per call, not once
  per layer. That cost is negligible for configuration resolution and buys a table that cannot
  be mutated and is safe for concurrent use without further thought.

No key-lookup map exists. Unknown keys are detected by counting (§4): if a layer has more entries
than the number of table keys found in it, it contains an unknown key. Only then is the layer
scanned to name the offending key in the panic message.

### 3.3 Log levels
```go
// validateLogLevel reports whether s is a legal effective LogLevel. A switch keeps the
// legal set immutable by construction: there is no package state to write to.
func validateLogLevel(s string) error {
    switch s {
    case "debug", "info", "warn", "error", "":
        return nil
    }
    return fmt.Errorf("%w %q (want one of \"debug\", \"info\", \"warn\", \"error\", or \"\")",
        ErrInvalidLogLevel, s)
}
```
Matching is exact and case-sensitive ("INFO" is invalid), since the spec lists exact values.

## 4. Algorithm and how each rule is satisfied

```
resolve(layers):
    fields := fieldTable()             // built once per call; nothing shared
    var c Config                       // every field starts at its Go zero value
    for _, l := range layers {         // lowest -> highest precedence
        seen := 0
        for _, f := range fields {     // table order, so the result does not depend on map order
            v, present := l.values[f.key]   // indexing a nil map is safe: present == false
            if !present { continue }
            seen++
            if !f.set(&c, v) {
                panic(fmt.Sprintf("config: layer %q: key %q: wrong type %T", l.name, f.key, v))
            }
        }
        if seen != len(l.values) {     // the layer has a key that is not in the table
            panic(fmt.Sprintf("config: layer %q: unknown key %q", l.name, firstUnknownKey(l.values, fields)))
        }
    }
    if err := validateLogLevel(c.LogLevel); err != nil { return Config{}, err }
    return c, nil
```

| Spec rule | How it is satisfied |
|---|---|
| Present means set, including `false`/`0`/`""` | Presence is decided only by the comma-ok `v, present := m[key]`. The value is never compared to its zero value. |
| Highest-precedence presenting layer wins | Layers are applied lowest to highest, and each present value overwrites the earlier one, so the last writer is the highest presenting layer. |
| Absent means no effect; lower value shows through | `!present` → `continue`; `c` keeps whatever a lower layer wrote. |
| No layer presents the field → Go zero value | `c` starts as `Config{}`. |
| nil layer maps (tests pass `nil`) | Indexing a nil map returns `present == false`, and ranging over it does nothing, so a nil layer is fully absent. |
| Wrong Go type for a key → panic | Exact type assertion through `assign[T]`. A failure panics, and the message names the layer, key, and dynamic type. This covers `int64`/`float64` for int fields, named types such as `type B bool`, and an explicit `nil` value (`v.(T)` fails for a nil interface). Because every layer is applied, a wrongly typed value panics **even when a higher layer overrides it**. A programmer error is never hidden by precedence. |
| Only the four keys are legal | An unknown key (such as the typo `"Loglevel"`) is the same kind of programmer error, so it panics too. Silently ignoring it would hide typos. The spec only *requires* panicking for wrong types. Panicking on unknown keys is a stricter design choice, consistent with "the only legal keys are…". It is **stated explicitly in the `Resolve` doc comment** (§2), so callers do not assume unknown keys are ignored or returned as errors. Detection uses the count check. `firstUnknownKey` returns the lexicographically smallest key that is not in the table, so the panic message is deterministic. |
| Validate only the effective `LogLevel` | Validation runs once, after merging, on `c.LogLevel`. Overridden values are never looked at, so `file:"trace"` overridden by `flags:"info"` → no error. |
| Legal values include explicit `""` | `""` is a case in the `validateLogLevel` switch. `""` from no layer at all (zero value) is also valid, which follows from the same check. |
| Invalid → `errors.Is(err, ErrInvalidLogLevel)` | The error is wrapped with `%w`. The message includes the bad value for diagnostics. |
| Other fields not validated | No checks on `Verbose`/`MaxRetries`/`TimeoutSec`. Negative ints are accepted. |
| No env/file/flag I/O | The package imports only `errors` and `fmt`. |

Other behavior:
- **Return value on error:** `Config{}` with the error, following the Go convention that a result
  is meaningless when `err != nil`. The public tests ignore the config on error.
- **Input maps are never mutated** (read-only access). The only package-level variable is
  `ErrInvalidLogLevel`, a sentinel the spec requires as a `var`. The field table is built for each
  call (§3.2) and the log-level set is a `switch` (§3.3). With no shared mutable state, `Resolve`
  is safe for concurrent use.
- **Panic determinism:** layers are processed in precedence order, and within a layer, fields are
  processed in table order. Type checks for known keys come before the unknown-key check, and
  `firstUnknownKey` picks the smallest unknown key. The same input therefore always produces the
  same panic message.

## 5. Maintainability and extension
- **New field:** add it to `Config` and add one row to `fieldTable`. The unknown-key check needs no
  change, because it counts against the table. If the field needs validation,
  add a validation step after the merge, next to `validateLogLevel` (validation always runs on
  effective values).
- **New layer:** add one entry to the layer slice at its precedence position. `resolve` stays the
  same.
- Doc comments on every exported identifier and on `resolve`/`fieldTable`/`assign`. They explain
  present-vs-absent semantics and the panic contract, so callers know that wrong types and
  unknown keys are bugs, not runtime errors.

## 6. Test approach

### 6.1 Public suite
`resolve_public_test.go` is used unchanged and covers spec cases 1–8.

### 6.2 Internal suite (`resolve_internal_test.go`, package `config`, table-driven where natural)
- T1 All four layers nil → `Config{}`, nil error.
- T2 Empty non-nil maps → same as T1.
- T3 Overridden invalid LogLevel is not validated: `defaults:"trace"`, `flags:"info"` → `"info"`,
  no error. Also `file:"bogus"` overridden by `env:""` → `""`, no error.
- T4 Each legal level (`debug`,`info`,`warn`,`error`,`""`) presented by each layer → no error, and
  that value is effective.
- T5 Invalid levels (`"trace"`, `"INFO"`, `" info"`, `"Debug"`) → `errors.Is(err,
  ErrInvalidLogLevel)`, the returned config is `Config{}`, and the message contains the bad value.
- T6 Zero-value overrides for every field across every pair (lower, higher) of layers:
  `true→false`, `3→0`, `"info"→""`, `30→0`.
- T7 Pass-through: a field present only in a middle layer survives higher layers that are absent
  for it but present for other fields.
- T8 Mixed fields: each field won by a different layer at the same time (Verbose from defaults,
  MaxRetries from file, LogLevel from env, TimeoutSec from flags) → all four correct.
- T9 Wrong-type panics, checked with a `mustPanic(t, func())` helper using `recover`:
  `Verbose:"true"`, `Verbose:1`, `MaxRetries:int64(1)`, `MaxRetries:1.0`, `LogLevel:[]byte("x")`,
  `TimeoutSec:"30"`, a named type `type myInt int`, and an explicit `nil` value. Each case is run
  in each layer position.
- T10 A wrong type in an *overridden* lower layer still panics (`defaults{"MaxRetries":"3"}`,
  `flags{"MaxRetries":4}`).
- T11 Unknown key panics (`"Loglevel"`, `"timeout"`), alone and alongside valid keys, in each layer
  position. The panic message names the key and layer. With two unknown keys in one layer, the
  smaller one is always reported (run it several times to check determinism).
- T12 Input maps are not mutated: deep-copy before the call, then compare with `reflect.DeepEqual`
  after.
- T13 Table/struct sync guard. This checks behavior, not just accepted types:
  - **Coverage:** with `reflect`, every exported field of `Config` has exactly one row in
    `fieldTable()` with the same key. There are no duplicate keys, no empty keys, no nil
    setters, and no row whose key is not a `Config` field. Table length equals
    `NumField()`.
  - **Correct target:** for each row, build `base`, a `Config` whose fields hold distinct
    non-zero sentinel values (`Verbose: true`, `MaxRetries: 101`, `LogLevel: "base"`,
    `TimeoutSec: 202`). Copy it to `c`, and call `row.set(&c, probe)`, where `probe` is a value
    of the matching field's type that differs from every sentinel. It is built from the field
    type with `reflect`: `false` for bool, `987` for int, `"probe"` for string. A field of
    another kind fails the test, which forces whoever adds it to extend the guard. Assert that
    `set` returns `true`, that `reflect` field `row.key` of `c` equals `probe`, and that every
    other field of `c` equals the same field of `base`. If `"MaxRetries"` is wired to
    `&c.TimeoutSec`, then `MaxRetries` is unchanged and `TimeoutSec` changed, and the test
    fails.
  - **Wrong type rejected without writing:** for each row, call `set` with a value of a
    different type (for example `struct{}{}`). Assert that it returns `false` and that `c`
    still equals `base`.
  - **Fresh table:** change an element of one `fieldTable()` result, for example set its `key`
    to `"x"`. Then assert that a second call still returns the original keys. This documents
    that no shared table exists.
- T14 Concurrency smoke test: run `Resolve` from many goroutines with shared input maps under
  `go test -race`.
- T15 Setter wiring end to end: each field is presented alone, with a distinct value, in a
  single layer. Assert that only that field of the result is non-zero. This repeats the T13
  wiring check through the public `Resolve` path.

### 6.3 Gates
`gofmt -l .` (empty), `go vet ./...`, `go build ./...`, `go test -race ./...`. All must pass.
