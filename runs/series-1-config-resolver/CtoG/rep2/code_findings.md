No blocker or major issue remains.
minor: Adding a layer still requires a new Resolve parameter and every call site to change; resolve([]layer) is unexported, so "gains more layers" is an internal convenience, not an extension callers can use without an API break.
nit: checkKeys panics on keys other than the four field names, which the spec does not require (it only mandates panic for a wrong Go type); the policy is documented and reasonable, but stricter than the written rule.
nit: On ErrInvalidLogLevel, Resolve always returns Config{}, while the spec leaves the accompanying Config unspecified; the doc comment states the convention, so callers must not depend on a partial config.
