# Response to Round-2 Plan Findings

## 1. [minor] T13 does not catch swapped same-type setters — **FIXED**

The finding is correct. The old T13 only checked that a setter accepted the right Go type, so
swapping the `MaxRetries` and `TimeoutSec` setters (both `int`) would have passed. Changes in
`plan.md`:
- **T13 now checks behavior (§6.2).** For each row, the test starts from a `Config` whose fields
  all hold distinct non-zero sentinel values. It calls `set` with a probe value that differs from
  every sentinel. It then asserts three things:
  - `set` returns `true`;
  - the field named by the row's key equals the probe;
  - every other field is unchanged.

  A swapped setter changes the wrong field and fails. T13 also asserts that a value of the wrong
  type returns `false` and writes nothing. Its coverage checks are tighter as well: no duplicate,
  empty, or extra keys, no nil setters, and table length equal to `NumField()`.
- **New T15** repeats the wiring check through the public `Resolve` path: one field presented
  alone means only that field is non-zero.
- **Design (§3.2).** Each row is now `func(c *Config, v any) bool { return assign(v, &c.X) }`, with
  a generic `assign[T any](v any, dst *T) bool`. Type inference from `&c.X` binds the asserted
  type and the destination in one expression, so a type/target mismatch does not compile. The
  only remaining risk is pairing a key with the wrong field of the same type, and the
  strengthened T13 covers exactly that.

## 2. [nit] `fields` is a package-level mutable slice — **FIXED**

Changing to a fixed-size array would not help. A package-level array is just as writable, and a
short literal would silently zero-fill rows. Instead, the package-level variable is removed:
- `fieldTable() []field` returns a new slice literal on each call.
- `resolve` calls it once per `Resolve` call and passes the result to `firstUnknownKey`.
- No shared table exists that internal code could change by accident or race on.

The row closures capture nothing, so the only cost is one small slice allocation per call. That
is negligible for configuration resolution. A T13 sub-check documents the property: changing one
returned table does not affect the next call. The only package-level variable left is the
spec-required `ErrInvalidLogLevel` sentinel, and §4 says so.

## 3. [nit] No blocker or major issue remains — **ACKNOWLEDGED**

This is informational and needs no action. No other part of the plan changed beyond items 1 and 2
and the cross-references they required (§4 algorithm, rule table, §5, the revision notes).
