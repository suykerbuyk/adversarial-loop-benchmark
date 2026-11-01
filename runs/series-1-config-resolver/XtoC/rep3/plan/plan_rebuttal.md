# Response to Adversary Findings

1. **Major — unknown keys silently ignored: fixed.** The revised plan requires checking every key in every input layer against the supported-key registry and panicking on unsupported keys. The panic names the key and layer. The test plan covers unknown keys across layers, including spelling and case variants.
2. **Minor — field knowledge not centralized: fixed.** The internal model specifies one descriptor registry keyed by field name, with an explicit typed `apply` function per field. Resolution, type assertions, and unknown-key validation use that registry. It explicitly avoids reflection.
3. **Minor — overridden wrong-type inputs: fixed.** The plan requires scanning and type-checking every present value in every layer, including lower-precedence values later overridden. A dedicated test covers a mistyped lower value overridden by a correctly typed higher one.
4. **Minor — panic diagnostics: fixed.** Checked assertions panic with the field key, layer name, expected type, and actual type. Layer names are paired with maps in the ordered layer slice.
5. **Minor — Config returned on validation error: fixed.** The API documentation contract and plan specify `Config{}` on invalid effective `LogLevel`, with a test pinning that behavior.
6. **Minor — source location: fixed.** The plan locates the package implementation in root-level `config.go`, consistent with the root package and shipped public test.
7. **Minor — nil maps and nil-valued present key: fixed.** The tests cover nil layer maps and a present `LogLevel: nil`, which must panic as a type mismatch.
8. **Nit — input mutation/retention: fixed.** The plan explicitly states that `Resolve` reads but neither mutates nor retains the input maps, with a test checking they remain unchanged.
9. **Nit — sentinel and wrapped invalid value: fixed.** The plan sets the sentinel message to `config: invalid log level` and wraps it using `fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)`.
