# Plan Rebuttal — Round 2

Source note: `plan/plan_findings.md` is not on disk. The round-1 adversary transcript (`turns/p1_adv.json`) says it wrote the file and records exactly one finding, reproduced below. The adversary also stated: "No blocker or other major issue remains."

## Finding 1 — [major] Unknown keys panic; the spec only mandates a panic for wrong-typed values
> The plan makes unknown keys panic, but the spec only requires a panic for a present value of the wrong Go type; it does not define unknown keys as programmer errors or authorize rejecting them. This adds behavior beyond the fixed API contract and could reject caller-supplied maps that should simply have no effect.

**Verdict: rebutted, with a small maintainability fix.**

Reasons:
1. **The spec does define legality.** It says: "The only legal keys are `"Verbose"`, `"MaxRetries"`, `"LogLevel"`, `"TimeoutSec"`." A key outside that set is therefore illegal input by the spec's own wording. The same paragraph says how this package treats illegal input supplied by code: it is "a programmer error and must panic". Treating an illegal key the same way as an illegal value type is the consistent reading. Silently accepting a key the spec calls illegal would be the extra behavior.
2. **The spec never says unknown keys "have no effect".** "No effect" is defined only for an *absent field*, and fields are exactly the four legal keys. An extra map entry is not an absent field. It is something the spec does not describe at all, so neither reading gets explicit authority. Given that ambiguity, failing loudly is the safer choice.
3. **Ignoring them silently causes real bugs.** The keys are case-sensitive struct field names. A caller who writes `"Loglevel": "debug"` or `"Timeout": 30` in `flags` intends an override. If the key is ignored, the lower layer's value shows through with no signal, and the program runs with a wrong configuration that is hard to diagnose. The point of an override rule built on presence is that whatever the caller presents takes effect. A typo that quietly takes no effect works against that.
4. **No stated behavior or test is affected.** The API signature is unchanged. All eight public cases, and every input the spec describes (legal keys, any precedence pattern, nil or empty maps), behave exactly as specified. The change only affects inputs the spec calls illegal.
5. **Long-term maintenance (the spec's "gains more layers and fields" requirement).** When fields are renamed or added, strict keys catch callers who still send the old names. Ignoring keys would hide that drift.

Fix applied anyway (plan §3 and §4): the unknown-key check is moved into one documented function, `rejectUnknownKeys(layer)`. Its doc comment cites the "only legal keys" sentence. If the project later chooses lenient behavior, it is a one-function change, and a dedicated test (§5, "unknown key panics") marks the decision clearly in the suite.
