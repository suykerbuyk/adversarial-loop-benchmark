# Plan rebuttal

Point-by-point response to `plan/plan_findings.md`. The findings file states that no blocker or major issue remains and that re-derivation found no actionable defects. There is no tagged finding to fix. The plan is kept, with two clarifications written into `plan/plan.md` so the next implementation pass cannot regress the Close join or the synctest contract.

## 1. No tagged findings

**Rebutted.** `plan/plan_findings.md` contains one sentence: no blocker or major issue remains, and re-derivation against the specification and public tests found no actionable defects. There is no `blocker`, `major`, `minor`, or `nit` line to apply. The public API, data model, per-rule mapping, and test approach in the previous plan stay in force.

## 2. Close join held under `closeMu` (clarification, not a finding)

**Fixed in the plan anyway.** The previous Close sketch took `closeMu` with `defer Unlock` around `wg.Wait`, which already made an overlapping Close wait for the same join. The text now states that explicitly and forbids the nearby mistake of unlocking `closeMu` before `Wait` and re-locking only to read `closed`. That mistake would let a second Close return while the first was still joining, which rule 8 forbids. The overlapping-Close private test now uses `synctest.Wait` to show the second Close is still blocked until the Loader returns.

## 3. No timers in the cache (clarification, not a finding)

**Fixed in the plan anyway.** `TestSingleFlight` calls `synctest.Wait` while Gets are parked, and `t.Cleanup` runs `Close` inside the same bubble. A `select` or wait that includes `time.After` or a `time.Timer` is not a durable block, so `Wait` would not return and the bubble could deadlock. The plan now requires a plain `wg.Wait` and a three-way `select` on caller ctx, `flight.done`, and `base.Done()` only. Caller deadlines stay in the caller's context.
