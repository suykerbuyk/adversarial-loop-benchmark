# Response to plan findings (round 1 → round 2)

1. **major — §5.2 returns `Config{}` on ErrInvalidLogLevel.** **Fixed.** Accepted: spec.md says
   `Resolve` "returns the effective `Config` and an error", so this is not a silent area. `resolve`
   now returns the merged `c` together with the error (plan §3, §4 row "returns the effective
   Config", §5.2). The doc comment says the Config is diagnostic-only when `err != nil`. The §6
   test now asserts that the full effective Config (the offending LogLevel plus the other three
   resolved fields) comes back with the error.

2. **minor — coverage test misses same-type mis-binding.** **Fixed.** Added a "Binding wiring" test
   (plan §6). For each `Config` field found via reflect, it calls that key's setter on a zero
   Config with a distinct non-zero value of the field's type, then asserts that exactly the named
   field changed and all other fields stayed zero. The coverage test now also checks declaration
   order and that keys have no duplicates.

3. **minor — unknown-key panic goes beyond the spec, so it must be documented.** **Fixed (decision
   kept).** The unknown-key panic stays, for the reasons in §5.1. `Resolve`'s doc comment now lists
   both panic conditions explicitly, unknown key and wrong dynamic type (including overridden
   values) (plan §1 item 3, §5.1). The unknown-key panic test is kept.

4. **nit — panic message prints the value with `%#v`.** **Fixed.** Panic messages now contain only
   the layer, the key, the dynamic type (`%T`) and the expected type, never the value (plan §2.2).
   A test asserts that the value does not appear in the recovered message.

5. **nit — validation is hard-coded apart from the field table.** **Fixed.** Each `binding` now has
   an optional `validate`, built by `bind(key, field, check func(T) error)`, so a field's type,
   storage and value rules are declared on one line. Post-merge validation loops over `bindings`.
   `bindings` is now an ordered slice, with a `byKey` index built from it, so validation order is
   deterministic. Failures are combined with `errors.Join`, which keeps `errors.Is` working and
   reports every invalid field once there is more than one validator (plan §2.2, §3).

6. **nit — map order makes panic messages nondeterministic.** **Fixed.** Every panic test uses exactly
   one bad key and asserts only that a panic happens (at most a substring check for the key name or
   type), never the exact full message (plan §6).
