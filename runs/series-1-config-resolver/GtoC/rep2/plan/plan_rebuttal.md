# Plan rebuttal

Point-by-point response to `plan/plan_findings.md`. Each item is fixed in `plan/plan.md` unless marked rebutted.

1. **major: return zero Config on validation failure.** Fixed. `Resolve` returns the merged effective `Config` together with the error. `validate` no longer substitutes `Config{}`. Tests 3 and 5 assert the returned `LogLevel` (and, in test 5, other merged fields) on the error path. Public test 8 still only checks `errors.Is`.

2. **minor: highest-layer-only scan is not equivalent.** Fixed. The equivalence claim is removed. The plan states that a per-field highest-presenter scan would miss wrong types in overridden layers and, if it loops only over catalog keys, unknown keys. The required algorithm is the sequential full scan.

3. **minor: no test for wrong type overridden by a higher layer.** Fixed. Test 13: defaults `{"MaxRetries": "3"}`, flags `{"MaxRetries": 1}` must panic.

4. **minor: unknown-key panic credited to the spec.** Fixed. Labeled a design decision. The spec's explicit panic sentence covers wrong Go types only. `doc.go` records the decision. The plan no longer says "Spec: programmer error must panic" for unknown keys.

5. **minor: no test for illegal LogLevel overridden by explicit `""`.** Fixed. Test 9: defaults `"trace"`, flags `""` → `""`, nil error.

6. **minor: forgotten catalog row is silent.** Fixed. Test 16 uses reflection to require exactly one catalog row per exported `Config` field, with a matching key and kind, and no catalog key that is not a field.

7. **minor: `kind` and `apply` can drift.** Fixed. `fieldSpec` is `key` + `kind` only. `apply` type-checks from `kind` and writes by key. No per-field closure with its own assertion.

8. **minor: growth plan breaks the exported signature.** Fixed. Unexported `resolve(layers ...map[string]any)` owns the merge. Exported `Resolve` is a fixed four-argument wrapper. New layers are new wrappers around `resolve`, not a change to `Resolve`.

9. **nit: public test file contradicted.** Fixed. File layout lists existing `worktree/resolve_public_test.go` and says do not edit or replace it. The "copied into the package" wording is gone.

10. **nit: inline validation vs `validate`.** Fixed. One path: `return cfg, validate(cfg)`. The inline switch that returned `Config{}` is gone.

11. **nit: doubled `config:` prefix.** Fixed. Error is `fmt.Errorf("%w: %q", ErrInvalidLogLevel, lvl)`, which formats as `config: invalid log level: "trace"`.

12. **nit: non-mutation test checks keys only.** Fixed. Test 14 clones with `maps.Clone` and compares with `maps.Equal` (keys and values).

13. **nit: legal levels listed in three places.** Fixed. One unexported `legalLogLevels` set. Sentinel comment points at it. The legal-level test iterates that set.
