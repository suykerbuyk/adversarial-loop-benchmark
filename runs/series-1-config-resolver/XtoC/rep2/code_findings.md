# Code findings — round 3 (adversarial review of worktree/config.go + config_test.go)

Verification (re-derived in a scratch copy at /tmp/rv3): go1.27.2. `go build ./...`, `go vet ./...`, and `gofmt -l .` are clean. `go test -race -count=1 -cover ./...` passes all 8 public tests and the 5 implementor tests, with 100% statement coverage. worktree/resolve_public_test.go is byte-identical to instrument/. No code_rebuttal.md exists; the implementor answered round 2 in code.
Behaviour probes, all correct per spec:
- Present zero values override, and negative ints are kept.
- An invalid lower LogLevel is overridden and accepted.
- An invalid top LogLevel returns a zero Config and an error that matches errors.Is(ErrInvalidLogLevel) and quotes the value.
- A wrong type in file, env, or flags panics, naming the layer, key, and wanted/got type. This includes nil.
- Unknown keys panic case-sensitively ("verbose").
- When a layer has several bad keys, which one panics is deterministic because keys are sorted.
- 64 concurrent Resolve calls are race-free. Package-level state is read-only after init.
Mutation probes (did config_test.go plus the public tests catch it?):
- Field-pointer swaps: caught, but only by public test 1.
- Broken LogLevel set check: caught.
- Duplicated key-literal drift: SURVIVED.
- `return cfg, err` instead of `Config{}, err`: SURVIVED.
- Validation loop hard-coded back to `[]string{"LogLevel"}`: SURVIVED.
- Per-layer `sort.Strings` removed: SURVIVED.
- Layer name dropped from the wrong-type panic: SURVIVED.
Round-2 status:
- #1 (major, sham validate hook): FIXED. Resolve now iterates `sortedFieldKeys` and calls every non-nil `validate`.
- #2 (free-form typeName): FIXED via `reflect.TypeFor[T]()`.
- #4 (swappable string params): FIXED. The setter takes only `layer`.
- #6 (copy shadowing): FIXED. Duplicate subtest names remain.
- #3 (test gaps): partly open (see 1–3 below).
- #5, #7, #8: unchanged nits.

1. [minor] Nothing tests the generic validation loop that fixed round-2 #1. Reverting Resolve to validate only `"LogLevel"` (mutation verified) passes every test. So the extensibility fix that closed the round-2 major has no regression guard. Add a test that installs a temporary descriptor with a validator in `fields`/`sortedFieldKeys`, restored via t.Cleanup, and asserts that its error is returned. Alternatively, factor validation into `validate(cfg Config, descs ...)` and test that directly.
2. [minor] The doc comment promises "Resolve returns a zero Config" on an invalid LogLevel, but no test asserts it. Changing `return Config{}, err` to `return cfg, err` survives (mutation verified). Callers may rely on the documented contract, so pin it with `if got != (Config{})` in TestLogLevelValidationAppliesAfterOverrides.
3. [minor] Each key is written twice: once as the map key and once as the `field("Verbose", ...)` argument used only in panic messages. Nothing ties them together. A typo or rename drift survives all tests (mutation verified) and makes panics report the wrong key. Pass the map key at registration time instead, e.g. build `fields` from a slice of descriptors whose key appears once, or have Resolve pass `key` into `set`. Alternatively, add a test that asserts every panic names the key that was presented.
4. [minor] Test coverage for wrong-type panics is weak. TestWrongTypesPanic only asserts that some panic happens. It never checks that the layer and key appear in the message (dropping the layer survives mutation), and it only tests the `defaults` layer. The field-to-key mapping (each key writes its own Config field) is protected only by public test 1 having distinct values. A reflect- or table-driven per-key test with distinct values would make that independent of the public suite.
5. [nit] The LogLevel descriptor is still built as `{set: field(...).set, validate: ...}`. It reaches into a constructed descriptor and discards the rest, and every future validated field will copy this shape. Prefer `field(...).withValidate(fn)` or an optional validator argument.
6. [nit] Validation returns on the first failing validator. Once more fields gain validators, a caller with several bad fields fixes them one per run. Consider `errors.Join` over all validator errors. errors.Is still works through Join.
7. [nit] The invalid-LogLevel error quotes the value but not the layer that supplied it. For a resolver whose whole job is layering, "flags" vs "file" is the most useful diagnostic. Tracking the winning layer per field is cheap; set() already receives `layer`.
8. [nit] TestWrongTypesPanic subtests are named only by key, which produces `MaxRetries#01` and `LogLevel#01`. Include the value's type (`%T`) in the subtest name.
9. [nit] Panics carry a plain string. A recovering caller cannot inspect layer, key, or kind without parsing the message. This is optional, since the spec only requires a panic.
10. [nit] Resolve allocates and sorts a key slice per layer on every call. This is negligible at 4 keys and justified by deterministic diagnostics. It is only worth revisiting if Resolve lands on a hot path. Note that dropping the sort is also untested (mutation survives), so the determinism guarantee is undocumented by tests.

No blocker or major issue remains. The implementation is functionally correct against every spec rule I could derive and probe. Build, vet, gofmt, and race-enabled tests are clean, and the round-2 major (finding #1) is fixed. What remains are test-guard gaps (1–4) and style nits.
