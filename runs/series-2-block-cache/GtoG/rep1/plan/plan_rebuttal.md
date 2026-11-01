# Plan rebuttal

Point-by-point response to `plan/plan_findings.md`. Each finding is fixed in `plan/plan.md`. None is rebutted.

## Blocker: panic recover locks mu while the success path still defers Unlock

Fixed. `run` no longer installs a recover defer that locks `mu` around a body that also `defer c.mu.Unlock()`.

The Loader call sits in an inner function. That function's recover records `errLoaderPanic` and does not touch `mu`. After the inner function returns, `run` takes `mu` once, publishes or admits, and unlocks with an explicit `c.mu.Unlock()`. There is no deferred unlock. The recover never locks and never unlocks. If the Loader panicked, `run` re-panics only after that unlock. A later panic inside the admit section is a cache bug, not a Loader panic; it is not recovered by a second locker. `loads.Done` remains a defer on `run`, so it runs after the unlock and after a re-panic starts unwinding. `Close` cannot wedge on a double-lock, and an unlocked mutex is not unlocked again.

## Major: errPanic is a caller-visible error the API never declared

Fixed. The plan no longer treats a named panic sentinel as part of the contract callers may match.

`errLoaderPanic` is unexported. It is a join-safety signal so current waiters are not left blocked and `f.done` is closed. It is not exported, not wrapped as `ErrClosed`, and not documented as an `errors.Is` target. The panic path publishes a non-nil error and does not insert, so a later `Get` on an open cache calls the Loader again. That matches rule 3. Callers of a panicking Loader observe some non-nil error that is not `ErrClosed`. They must not depend on its text.

## Minor: evictLRU deletes through tail after unlink has cleared it

Fixed. `evictLRU` saves `victim := tail`, then unlinks `victim`, then `delete(resident, victim.key)`. The delete does not read `tail` after unlink. Capacity 1, the sole-node case, leaves `head` and `tail` nil and removes the saved key. It does not delete `resident[""]` and does not dereference a nil tail.

## Nit: cancel runs while mu is held

Fixed. `Close` sets `closed` and closes `started` under `mu`, unlocks, then calls `cancel`. A Done callback that calls `Get` or `Len` can take `mu` and observes `closed` already set. `cancel` is not called while `mu` is held. The plan also states the re-entry limit: a Done callback that calls `Close` and then needs the Loader to return in order for that `Close` to finish would wait on the owner, who is still inside `cancel`. The intended re-entry is `Get` and `Len`.
