# Code Findings — adversarial review, round 3 (CtoC/rep3)

What I ran myself with go1.27.2 in worktree/: `gofmt -l .` printed nothing, and `go build ./...` and `go vet ./...` are clean. `go test -race -count=1 -cover ./...` passes at 100.0%, and `go test -count=3 -shuffle=on ./...` passes too. worktree/resolve_public_test.go is byte-identical to instrument/ (checked with diff). staticcheck is not installed. I checked every round-2 rebuttal claim against config.go and config_internal_test.go, and ran extra probe tests in a throwaway copy (/tmp/probe4).

Round-2 items, checked again:
- R2 #1 (unknown keys echoed in full): fixed and verified. `unknownKeys` sorts the keys, applies `quoteTrunc` to each, and caps the list at maxEchoKeys with "and N more". `TestEchoedTextIsTruncated` pins the exact message.
- R2 #2 (validateLogLevel echoes the value): truncation is added and the choice is documented. Accepted.
- R2 #3 (errors.Join branch was uncovered): fixed and verified. `validate(c, bs)` is covered by `TestValidateJoinsMultipleErrors`, and coverage is 100%.
- R2 #4 (cross-field validation): acknowledged and the extension point is documented. Accepted.
- R2 #5 (byKey pointers): fixed and verified. It is now `knownKeys map[string]struct{}`, and production code keeps no pointers into `bindings`.
- R2 #6 (TestBindingsWiring sample choice): fixed and verified. The sample is built with `reflect.New(f.Type).Elem()`, so named types keep their exact type.

Spec conformance, re-derived from source and probes: present zero values override; absent fields show through; a field no layer presents gets its zero value; only the effective LogLevel is validated (an invalid level that a higher layer overrides gives no error); `errors.Is(err, ErrInvalidLogLevel)` is true; and wrong types panic, including int64, uint, float64, named string, untyped nil and typed *bool, even in layers that are later overridden. All eight public cases pass.

New findings:
[nit] config.go quoteTrunc(): invalid UTF-8 is rendered differently depending on length. A short value is escaped faithfully (probe A2: `"\xff"`). A value longer than maxEchoRunes goes through `[]rune(s)`, which turns each bad byte into U+FFFD. %q then prints U+FFFD as a literal character (probe A/B: `"����…"...`), so the message no longer shows the real bytes. Possible fix: find the byte offset of rune maxEchoRunes with `utf8.DecodeRuneInString` (or a `for i := range s` loop), then quote `s[:off]`. The bytes stay as they were and the escaping matches the short case.
[nit] config.go quoteTrunc(): bounding the output does not bound the work. `utf8.RuneCountInString` scans the whole string, and `[]rune(s)` allocates 4 bytes per rune of the whole input. Benchmark: a 16 MiB invalid LogLevel costs about 64 MiB and about 40 ms per Resolve call. This only happens on the error path and the caller already holds the string, so the impact is small. The byte-offset approach in the previous finding fixes this too: O(maxEchoRunes) work and no copy of the whole string.
[nit] config.go Resolve doc comment: the exported godoc refers to the unexported constants `maxEchoRunes` and `maxEchoKeys`, which users of the package cannot see in rendered docs. Say "32 runes" and "5 keys" (or "a short prefix" and "a few keys") instead.

Probes that found nothing (behaviour is correct):
- A wrong type in a lower layer panics before an unknown key in a higher layer, deterministically (probe C).
- A typed nil pointer panics with `*bool` (probe D).
- An empty-string key is reported as an unknown key `""` (probe F).
- An invalid level returns the effective Config alongside the error, as owned in R1 #3 (probe E).
- The happy path costs 7 allocations per call, which is fine.
- The package has no mutable shared state. `bindings` and `knownKeys` are read-only after init, and no races were seen under -race.
- Extensibility holds: a new field is one struct field plus one `bind(...)` line, guarded by `TestBindingsCoverConfigFields` and `TestBindingsWiring`. A new layer is one more `layer{}` entry, because the internal `resolve` takes a `[]layer`.

No blocker or major issues remain. The three findings above are all nits.
