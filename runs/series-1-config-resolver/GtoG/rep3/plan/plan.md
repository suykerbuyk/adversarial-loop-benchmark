# Implementation plan: layered configuration resolver

Package `config` (module path `config`, Go 1.25, as in `worktree/go.mod`) resolves four in-memory layers into one effective `Config`. No environment, file, or flag I/O. This revision answers the second `plan/plan_findings.md`.

## Public API

Live in package `config`. Signature and names are fixed by the spec and by `instrument/resolve_public_test.go` (`package config`, calls `Resolve`, compares `Config`, uses `errors.Is(..., ErrInvalidLogLevel)`). The exported signature stays frozen. A fifth layer is a breaking change to `Resolve` and is not added in this version.

```go
package config

import "errors"

// ErrInvalidLogLevel is returned when the effective LogLevel is not a legal value.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config is the effective configuration after layer resolution.
type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

// Resolve combines defaults, file, env, and flags in increasing precedence.
//
// A key present in a map sets that field, including zero values (false, 0, "").
// A key absent from a map leaves the field unchanged. A nil map is all-absent.
// If no layer presents a field, the effective value is that field's Go zero value.
//
// Legal keys are "Verbose", "MaxRetries", "LogLevel", and "TimeoutSec".
// A present value of the wrong Go type panics, including when a higher layer
// later overrides that field, including an untyped nil, and including a named
// type whose underlying type matches (type myInt int is not int). The panic
// names the key, the expected type, and the dynamic type (%T only).
// Unknown keys are ignored in v1 (see Unknown keys). That choice lives in
// noteUnknownKey and can be flipped without touching the merge walk.
//
// Only the effective LogLevel is validated, against "", "debug", "info",
// "warn", and "error", case-sensitively. A presented level that a higher
// layer overrides is never validated. Verbose, MaxRetries, and TimeoutSec
// are not validated. The LogLevel check is independent of any other field
// validator: an illegal effective LogLevel always yields an error for which
// errors.Is(err, ErrInvalidLogLevel) is true, even if another field is also
// invalid.
//
// When err != nil, the returned Config is unspecified. Callers must not
// read it. This version returns the merged struct (not a zeroed Config);
// that is an implementation detail, not a contract. The error wraps
// ErrInvalidLogLevel.
//
// Resolve only reads the maps. Callers must not mutate them during the call.
func Resolve(defaults, file, env, flags map[string]any) (Config, error)
```

- `ErrInvalidLogLevel` is a sentinel. Invalid effective levels wrap it with `fmt.Errorf("config: log level %q: %w", level, ErrInvalidLogLevel)` so `errors.Is(err, ErrInvalidLogLevel)` is true and the bad value appears in the message.
- Do not return a non-nil error for absent fields, zero values, empty `LogLevel`, or unknown keys.
- Layers may be `nil`. A nil map has no keys, so every field is absent on that layer. Public test 1 passes `nil` for file, env, and flags.

## Internal data model

Keep the public type as a plain struct. Internally, treat each field as a typed slot that is filled only when a layer presents that key.

### Field registry

One generic constructor binds a key to exactly one struct field. The apply closure writes through the pointer function the caller passed, so the binding is the constructor argument, not a hand-written assignment inside a custom `apply`.

```go
type field struct {
    key      string
    apply    func(dst *Config, v any) // panics on wrong dynamic type
    validate func(cfg Config) error   // optional; LogLevel does not use this for its spec check
}

func fieldOf[T any](key string, dst func(*Config) *T) field {
    return field{
        key: key,
        apply: func(cfg *Config, v any) {
            typed, ok := v.(T)
            if !ok {
                panic(fmt.Sprintf("config: key %q: got %T, want %s", key, v, typeName[T]()))
            }
            *dst(cfg) = typed
        },
    }
}

func typeName[T any]() string {
    var zero T
    return fmt.Sprintf("%T", zero) // "bool", "int", "string"; no value formatting
}
```

Registry (package-level slice, built once):

| Key | T | Pointer | Row validator |
| --- | --- | --- | --- |
| `Verbose` | `bool` | `func(c *Config) *bool { return &c.Verbose }` | nil |
| `MaxRetries` | `int` | `func(c *Config) *int { return &c.MaxRetries }` | nil |
| `LogLevel` | `string` | `func(c *Config) *string { return &c.LogLevel }` | nil |
| `TimeoutSec` | `int` | `func(c *Config) *int { return &c.TimeoutSec }` | nil |

`LogLevel`'s allow-list is **not** a row `validate`. It is a dedicated check after the generic validator loop (see Log level validation). A later field that needs its own rule still adds a `validate` on its row. That row validator cannot swallow `ErrInvalidLogLevel`.

A missing registry row leaves the new struct field at zero. The registry-binding test constructs each row through `fieldOf` with an explicit pointer function and fails if that pointer writes any field other than `key`.

### Layer order

The exported function is a thin wrapper. The walk lives in one unexported function so a later `ResolveLayers` does not fork it.

```go
func Resolve(defaults, file, env, flags map[string]any) (Config, error) {
    return resolve([]map[string]any{defaults, file, env, flags})
}

func resolve(layers []map[string]any) (Config, error)
```

Index 0 is lowest precedence. The last element is highest. Walk layers from lowest to highest. For each layer, scan keys in a stable order (below). For each registered field, if the key is present (`v, ok := layer[key]; ok`), apply `v`. Later layers overwrite earlier ones. Absence (`!ok`) does nothing, so a lower value shows through. If no layer presents the key, the field stays at the Go zero value of a freshly allocated `Config`.

This is presence-based override, not "non-zero wins." `false`, `0`, and `""` are applied when the key is present.

### Type rule (programmer error → panic)

On a present key, `fieldOf[T].apply` type-asserts to the field's exact Go type `T`. The comma-ok form is used so the panic text is stable; a failed assert still panics and is not recovered and is not returned as an `error`.

- `Verbose`: `T` is `bool`. Not `*bool`, not numeric stand-ins.
- `MaxRetries`, `TimeoutSec`: `T` is `int`. Not `int32`, `int64`, `float64`, `uint`, and not a defined type `type myInt int`.
- `LogLevel`: `T` is `string`. Not a defined `type level string`.
- Untyped nil (`any(nil)` stored under a legal key) fails every assert. It panics even if a higher layer overrides the field, because each present value is applied as it is seen.

JSON-decoded numbers are out of scope; the caller supplies Go values. The spec calls a wrong type a programmer error.

Panic text uses `%T` for the dynamic type and the expected type name. It does not format the value with `%#v` or `%v`. A huge, cyclic, or panicking `GoString` must not hide the type error or unbound the crash log. Planned text: `config: key %q: got %T, want %s`.

### Unknown keys

Assumption, not a spec mandate. The spec says the only legal keys are the four names, and it mandates a panic only for a present value of the wrong Go type. It does not define extra-key behavior. Panicking would freeze a crash into the public contract and would break the "gains more fields" requirement: a producer that already emits a new key would crash every older binary until both sides ship together.

v1 policy: ignore unknown keys. They do not set a field and they do not panic. Membership and the ignore-versus-panic choice both live in one function. The walk does not filter keys before calling it.

```go
// noteUnknownKey is the only unknown-key policy, including membership.
// v1 ignores keys that are not in known. A later revision can panic here
// without changing resolve's walk. known is the registry key set.
func noteUnknownKey(key string, known map[string]struct{}) {
    if _, ok := known[key]; ok {
        return
    }
    // v1: ignore
}
```

`resolve` builds `known` once from the registry. For each layer it sorts **every** key in the layer lexicographically and passes each key plus `known` to `noteUnknownKey`. A legal key is a no-op inside that function. A later "warn" or "panic" revision sees every key and decides; it does not reimplement membership in the walk.

Wrong-type panics on legal keys still happen in registry order, before the unknown-key pass, so a bad `Verbose` still wins over a bad `TimeoutSec`. `noteUnknownKey` is not on that path: a legal key with a wrong type is a type error, not an unknown-key decision.

### Stable key scan

Map iteration order is unspecified. Control flow never ranges the map.

1. Registered fields, in registry order (`Verbose`, `MaxRetries`, `LogLevel`, `TimeoutSec`). A present wrong type panics here, so the first bad known key in registry order wins. Two wrong-type keys in one layer panic on the first in that order. The panic text names that key and does not name the later bad key.
2. Then every key in the layer, collected and sorted lexicographically, each passed to `noteUnknownKey(key, known)`. Sorting happens before the policy runs. The walk does not drop known keys first.

Ranging the map is used only to collect keys into a slice that is then sorted.

### Log level validation (effective value only)

After all layers are applied:

1. Run each row's `validate` if non-nil. Today every row's `validate` is nil. A future field may set one. If one returns an error, remember it; do not return yet.
2. Always run the `LogLevel` allow-list, independent of step 1 and independent of registry order:

```text
legal := "", "debug", "info", "warn", "error"
```

If `cfg.LogLevel` is not in that set, return an error wrapping `ErrInvalidLogLevel`. This path is the only one that wraps that sentinel. A row validator must not wrap `ErrInvalidLogLevel`. If both a row validator and the allow-list fail, the allow-list error is the one returned, so `errors.Is(err, ErrInvalidLogLevel)` stays true while `LogLevel` is illegal. The spec validates only the effective `LogLevel`; other field failures are not part of the public error contract in v1.

Do not validate a presented level that a higher layer overwrote. Do not validate `Verbose`, `MaxRetries`, or `TimeoutSec` (negative timeouts and retries are allowed).

Empty string is legal, whether it is the zero value from total absence or an explicit `""` from some layer. Those two cases are indistinguishable in the result, and both must succeed.

Comparison is exact and case-sensitive. `"Info"`, `"DEBUG"`, `"trace"`, and strings with surrounding space are invalid.

### Error return value

The spec says `Resolve` returns the effective `Config` and an error. Public test 8 discards the value, so returning `Config{}` is a choice, not a requirement.

Contract: when `err != nil`, the returned `Config` is unspecified. Callers must not read it. This version returns the merged `cfg` (the struct after the layer walk, including an illegal `LogLevel`). The doc comment states both sentences so a later edit does not treat "unspecified" as a requirement to zero the struct. Tests assert `errors.Is` and do not assert the struct on the error path. Do not add a test that locks the unspecified half.

## How each stated rule is satisfied

1. **Four ordered layers, higher overrides lower.** `Resolve` accepts `(defaults, file, env, flags)` and passes them to `resolve` in that order, so flags beat env, env beats file, file beats defaults, for each field independently.
2. **In-memory only.** No `os.Getenv`, no file reads, no `flag` package. Layer names are precedence labels only.
3. **Present vs absent.** Presence is `comma-ok` on the map, not a nil-pointer or a non-zero check. A present zero value overwrites. An absent key does not.
4. **No layer presents a field → Go zero.** Start from `Config{}` and only assign presented keys.
5. **Wrong type panics.** `fieldOf[T]` asserts to exact `T`, including on a layer a higher layer overrides. Untyped nil and named types fail the assert. No coercion. No recover. Panic text is `%T` only, and names only the first bad key in registry order.
6. **Illegal keys.** Spec does not define the behavior. v1 ignores them inside `noteUnknownKey`, which also owns membership. Documented as an assumption in the `Resolve` comment.
7. **Validate only effective `LogLevel`.** Dedicated allow-list after the merge, against the five legal strings, always run, and always the error returned when the effective level is illegal. Overridden invalid levels are never inspected. Row validators cannot preempt this error.
8. **`errors.Is` contract.** The allow-list error wraps the exported sentinel `ErrInvalidLogLevel`.
9. **Other fields unvalidated.** Their `validate` is nil. No range checks on ints or bools.
10. **Extensibility.** A new field is a `Config` member plus one `fieldOf` row (and an optional `validate` on that row). The binding test constructs the row through `fieldOf` and catches a pointer aimed at the wrong member. A new layer is not a change to `Resolve`; it is a future caller of the same unexported `resolve([]map[string]any)`. The exported signature stays four positional maps. Unknown-key policy stays in `noteUnknownKey`.
11. **Unspecified config on error.** Doc comment states the contract and which value this version returns (the merged struct). No test locks it.

## Algorithm

```text
func Resolve(defaults, file, env, flags map[string]any) (Config, error):
    return resolve([]map[string]any{defaults, file, env, flags})

func resolve(layers []map[string]any) (Config, error):
    var cfg Config
    known := set of registry keys
    for _, layer := range layers:
        if layer == nil: continue
        for each field in registry:                 // stable order; type errors
            if v, ok := layer[field.key]; ok:
                field.apply(&cfg, v)                // panics on wrong T; %T only
        keys := every key in layer
        sort keys                                   // before policy
        for _, key := range keys:
            noteUnknownKey(key, known)              // membership + v1 ignore
    var rowErr error
    for each field in registry:
        if field.validate != nil:
            if err := field.validate(cfg); err != nil && rowErr == nil:
                rowErr = err
    if err := checkLogLevel(cfg.LogLevel); err != nil:
        return cfg, err                             // always wins; wraps sentinel
    if rowErr != nil:
        return cfg, rowErr
    return cfg, nil

func noteUnknownKey(key string, known map[string]struct{}):
    if key in known: return
    // v1: ignore

func checkLogLevel(level string) error:
    if level in {"", "debug", "info", "warn", "error"}: return nil
    return fmt.Errorf("config: log level %q: %w", level, ErrInvalidLogLevel)
```

## File layout

Under the module root (`worktree/` in this task; the package directory that already has `go.mod` and the copied public test):

- `resolve.go` — `ErrInvalidLogLevel`, `Config`, `fieldOf`, registry, `noteUnknownKey`, `checkLogLevel`, `resolve`, `Resolve`.
- `resolve_public_test.go` — already shipped; do not edit it. It must stay `package config`. The implementation must not depend on the test file.
- `resolve_test.go` — extra tests in `package config`. Keep them separate from the shipped file so the public suite is unchanged.

No subpackages. No external dependencies.

## Test approach

Shipped suite (`go test ./...`) covers the eight acceptance cases. Do not modify those tests. Additional tests in `resolve_test.go` lock the rules the public suite does not fully pin:

1. **All fields absent** (`nil, nil, nil, nil`) → zero `Config`, nil error. Confirms `""` LogLevel from absence is legal.
2. **Independent fields.** `defaults` sets all four; `flags` sets only `Verbose`; the other three equal defaults. Public test 6 checks one pass-through; this checks the other fields stay put.
3. **Mid-layer zero does not block a higher non-zero, and a higher absence does not undo a lower zero.** Example: defaults `MaxRetries=3`, file `MaxRetries=0`, flags absent → `0`. Example: defaults `TimeoutSec=9`, env absent, flags `TimeoutSec=1` → `1`.
4. **Highest layer wins when all four present the same field** — already public for `TimeoutSec`. Repeat once for `Verbose` (`true` then `false` then `true` then `false`) so bool and int paths both stay presence-based.
5. **Overridden illegal level is not validated.** defaults `LogLevel="trace"`, flags `LogLevel="info"` → success, `"info"`. defaults `LogLevel="nope"`, file `LogLevel=""` → success, `""`.
6. **Each illegal effective value fails `errors.Is`.** `"trace"` (public), plus `"Info"`, `"DEBUG"`, `" fatal"`, and a non-empty unknown string. Legal set `"debug"`, `"info"`, `"warn"`, `"error"`, `""` each succeeds when it is the effective value. On the error path, assert only `errors.Is`. Do not assert the returned `Config`.
7. **Wrong type panics, including when a higher layer would have overridden it.** `MaxRetries` presented as `int64(1)` or `float64(1)` on `defaults` while `flags` presents `MaxRetries` as `int(1)` → still panics. `Verbose` as `string`; `LogLevel` as `int`. `any(nil)` under a legal key panics. `type myInt int` presented as `MaxRetries` panics. Use `recover` in the test. Assert the panic value is a string that contains the key and does not contain a `%#v` dump of the value. Do not require a particular unknown-key panic.
8. **Unknown keys are ignored.** A layer with `"Extra"` and `"verbose"` plus a legal `Verbose: true` yields `Verbose == true` and a nil error. This locks the v1 assumption. If `noteUnknownKey` later panics, this test is the one that changes.
9. **Registry binding through the constructor.** For each production row, do not call the stored `apply` in isolation as the only check. Construct a fresh row with `fieldOf` and the same pointer function the registry uses (`func(c *Config) *T { return &c.<Field> }`). Apply one non-zero value. Use `reflect` to fail if any field other than `key` changed. Then reflect over `Config`'s exported fields and fail if any name has no registry row.
10. **Negative binding, not compiled into the package.** In the test, `fieldOf("MaxRetries", func(c *Config) *int { return &c.TimeoutSec })`, apply `7`, and fail the test if reflect shows any field other than `MaxRetries` changed. This case is not a package row. It fails, which proves the check sees the pointer the constructor was given.
11. **Nil vs empty map.** Both are all-absent.
12. **Two wrong types in one layer panic on the registry-order key, and only that key.** Layer presents `TimeoutSec` as `"no"` and `Verbose` as `1`. Recover and assert the panic string contains `Verbose` and does not contain `TimeoutSec`. A message that names every bad key fails this test.
13. **Validation error does not satisfy unrelated sentinels** and is non-nil only for bad effective `LogLevel`.
14. **LogLevel error is not order-dependent.** A test-local copy of the post-merge check (or a small unexported helper exercised directly) feeds an illegal `LogLevel` plus a failing row validator and asserts `errors.Is(err, ErrInvalidLogLevel)`. This locks the dedicated allow-list so a future row validator prepended in the table cannot steal the sentinel.

Run:

- `go test ./...`
- `go vet ./...`
- `go build ./...`

All three must be clean. Tests use only the standard library (`testing`, `errors`, `reflect`, `fmt` only if needed). Prefer not to assert exact panic text beyond containing the key and excluding the later bad key.

## Non-goals

- Reading process environment, config files, or `flag.FlagSet`.
- Coercing numeric types or defined types to their underlying type.
- Validating integers.
- Returning errors for unknown keys or wrong types (wrong types panic; unknown keys are ignored).
- Exporting a multi-layer entry point in v1. `resolve` is unexported until a spec adds `ResolveLayers`.
- Mutating the caller's maps. `Resolve` only reads them.
- Concurrency inside `Resolve`. No mutex in v1.
- A test that locks the `Config` value returned with a non-nil error.

## Risks and decisions

- **Unknown keys are ignored.** This is an assumption. The spec names the legal keys and mandates a panic only for a wrong Go type. Ignoring keeps older binaries compatible when a producer starts emitting a new field. Membership and the ignore-versus-panic choice both live in `noteUnknownKey`. The walk sorts every key and passes each one; it does not pre-filter. Documented on `Resolve`.
- **Config on validation failure is unspecified to callers, and this version returns the merged struct.** Stated in the `Resolve` doc comment. Callers must use the error. Tests do not lock the struct.
- **`int` not `int64`.** Public tests pass untyped constants that become `int` inside `map[string]any` composite literals. The assert must be `int`, matching those tests. A named `type myInt int` is a wrong type and panics. Platforms where `int` is 32-bit still accept the spec type `int`.
- **Case-sensitive log levels.** Spec lists lowercase literals only.
- **Exported signature stays four maps.** Layer growth goes through unexported `resolve([]map[string]any)`, not a second walk and not a signature change in this version.
- **LogLevel validation is not a row validator.** Putting it on the row would make error priority an accident of table order. The dedicated check always reports `ErrInvalidLogLevel` when the effective level is illegal.
