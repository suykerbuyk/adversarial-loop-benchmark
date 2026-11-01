# Responses to plan findings

1. **Blocker — check wrong types in overridden entries: Fixed.** The revised plan validates every present entry in every layer before winner selection. Its test plan includes a wrong-typed lower-precedence entry masked by a correct higher-precedence value; it must still panic.

2. **Major — unknown keys were silently ignored: Fixed.** The revised plan treats any key outside the four-field descriptor table as an illegal programmer input and panics, identifying the layer and key. Tests cover an unknown key in any layer, including a typo.

3. **Major — use a maintainable data structure and check completeness: Fixed.** The revised plan specifies one field descriptor table as the source of legal keys, expected types, and assignments, plus an ordered layer descriptor slice. It adds a reflection test for a one-to-one match between descriptors and `Config` fields.

4. **Minor — specify and test Config returned with invalid-level error: Fixed.** The revised plan explicitly returns the resolved `Config` together with the wrapped validation error, and tests that contract. This follows the natural reading that `Resolve` returns the effective configuration and an error.

5. **Minor — improve diagnostics: Fixed.** Wrong-type panic messages include layer, key, expected type, and actual `%T`; unknown-key panics include layer and key. Invalid-level errors include the offending value with `%q` and wrap `ErrInvalidLogLevel`; the supplying layer may also be included as context.

6. **Minor — test exact type edge cases: Fixed.** The test approach covers present nil values, named types, `int32`, `int64`, and `float64` as applicable. Exact type assertions reject these without conversions.

7. **Minor — identify implementation package location: Fixed.** The plan states that implementation belongs at the module root in `package config`, alongside the shipped root-package tests.

8. **Nit — centralize and case-sensitively validate log levels: Fixed.** The plan uses one unexported allowed-level set or switch, with exact matching. Tests reject `"INFO"`, `" info"`, and `"info "`.

9. **Nit — broaden log-level test matrix: Fixed.** The revised test plan covers all five legal values, invalid values at different layer positions, and an invalid lower-layer value overridden by explicit `""`.
