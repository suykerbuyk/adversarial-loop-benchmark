# Plan rebuttal

Point-by-point response to the second `plan/plan_findings.md`.

## major: Unknown-key policy is still split

**Fixed.**

`noteUnknownKey` now owns membership and the ignore-versus-panic choice. Signature is `noteUnknownKey(key string, known map[string]struct{})`. It returns immediately when `key` is in `known`; the v1 body ignores every other key. The walk no longer builds an "unknown only" slice. For each layer it collects every key, sorts that slice, and passes each key plus the registry set into `noteUnknownKey`. A later panic or warn inside that function sees legal keys and decides; it does not reimplement membership in the walk. Wrong-type panics on legal keys stay in the registry-order apply pass, which runs before this call, so a type error is still not an unknown-key decision.

## minor: The registry-binding test does not lock the pointer the constructor was given

**Fixed.**

The binding test constructs each row with `fieldOf` and the explicit pointer function (`func(c *Config) *T { return &c.<Field> }`), applies a non-zero value, and fails if reflect shows any field other than `key` changed. A second case is not a package row: `fieldOf("MaxRetries", func(c *Config) *int { return &c.TimeoutSec })` must fail that same check. Calling only the stored `apply` of a production row is no longer the proof.

## minor: Validator order is an accident of registry order

**Fixed.**

`LogLevel`'s allow-list is not a row `validate`. After the merge, row validators may run, but `checkLogLevel` always runs afterward and its error is the one returned when the effective level is illegal. A row validator must not wrap `ErrInvalidLogLevel`. A second validated field prepended in the table cannot make `errors.Is(err, ErrInvalidLogLevel)` false while `LogLevel` is still illegal. A test feeds an illegal level plus a failing row validator and asserts the sentinel.

## nit: Stable scan is only half-locked

**Fixed.**

The two-wrong-type test asserts the recovered string contains `Verbose` and does not contain `TimeoutSec`. A message that names every bad key fails.

## nit: Error-path doc comment vs the value this version returns

**Fixed.**

The `Resolve` doc comment still says the returned `Config` is unspecified and callers must not read it. It also says this version returns the merged struct, and that this is an implementation detail. The algorithm returns `cfg`, not `Config{}`. No test locks that half.
