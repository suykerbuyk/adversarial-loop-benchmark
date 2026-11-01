# Rebuttal of plan findings

Each finding in `plan/plan_findings.md` is fixed in `plan/plan.md`. None is rebutted. The previous plan described a locked snapshot and then specified a waiter loop that did not implement it. This revision deletes that loop. The snapshot function is the only waiter return path.

## major: The waiter loop does not implement the locked snapshot, so rule 7(b) never returns and a done ctx busy-spins

**Fixed.**

The previous loop checked `closed`, then a non-blocking receive on `f.done`, then `root.Err()`, then unlocked and selected. It never read the caller ctx under the lock and never returned `ctx.Err()`. A Get whose ctx was already Done, or became Done while the load was in flight, woke on `ctx.Done()`, looped, saw `f.done` still open, and selected the already-closed channel again. That is a busy-spin. `synctest.Wait` does not treat it as a durable wait, the 5s deadline fires, and rule 7(b) is never delivered while the load is still in flight.

`plan.md` now has one function, `snapshot`, which holds `mu` for the whole decision, in this order:

1. `closed` → `ErrClosed`.
2. Else `f.done` still open and `ctx.Err() != nil` → return that error. The caller must not `select` again.
3. Else `f.done` closed → the published result, copied in this same hold.
4. Else `waiting == true`.

`wait` returns immediately when `waiting == false`. The `select` on `f.done`, caller `Done`, and `root.Done()` has no return arm. An already-done joiner hits step 2 on the first call, before any `select`, while the Loader is still blocked. A caller that becomes done while parked wakes, re-enters `snapshot`, and returns `ctx.Err()` if `f.done` is still open.

`closed` and the receive on `f.done` are the same lock hold. `Close` sets `closed` only under `mu`, so the flag cannot change between those two reads. A success copy happens only when that hold saw `!closed`. The previous loop's closed check and `f.done` receive were sequential steps that a later edit could split; they are no longer a second code path beside the prose.

The private test for this is required: under `synctest`, a cancelled `Get` of an in-flight key returns `context.Canceled` while the Loader is still blocked and the call count is still 1. A busy-spin fails that test via the 5s deadline.

## major: `append([]byte(nil), src...)` does not preserve a non-nil empty payload

**Fixed.**

The previous plan claimed `append([]byte(nil), empty...)` is non-nil empty and used that expression for the stored copy and for every `Get` return. `append` returns the destination unchanged when the source length is 0, so a nil destination stays nil for `nil`, `[]byte{}`, and `make([]byte, 0)`. A non-nil empty Loader result would have been stored and returned as nil.

The only copy is `cloneBytes`: if `src == nil` return nil; otherwise `make([]byte, len(src))` and `copy`. `make` with length 0 returns a non-nil slice, so a non-nil empty payload stays non-nil and does not alias the Loader's header. `bytes.Clone` is an allowed equivalent. `append([]byte(nil), src...)` is forbidden in the plan's constraint list. The stored clone and every successful `Get` return go through this function. A required private test checks both headers. Public tests use `bytes.Equal` and will not catch a collapse; that is why the private test is required.

## minor: The `root.Err` arm is a second return path the snapshot forbids

**Fixed.**

That arm is deleted. It is not in `snapshot` and not in `wait`. `root.Done()` remains in the `select` only as a wake. After the wake, `snapshot` decides. Today `Close` sets `closed` before `cancel`, so a waiter woken by `root.Done()` sees `closed` and returns `ErrClosed` from step 1.

The arm is not kept as a backup. A later edit that cancels `root` without setting `closed` must not make every waiter return `ErrClosed` and skip a load result rule 7 requires while the cache is open. The plan states that invariant: `cancel` runs only after `closed` is set, and only from the first `Close`. The repair for a future cancel-without-close is to set `closed` first, not to resurrect the arm.

## minor: `joining.Add` is not paired with `joining.Done` if `go` never runs

**Fixed.**

The previous pairing added under `mu`, unlocked, started the goroutine, and `Done` only inside that goroutine. If `go` panics on the Get stack, the goroutine does not exist, `waiters.Done` still runs, and `joining` stays elevated. `Close` has set `closed` and blocks in `joining.Wait`. That is a rule 9 deadlock.

`Add` stays in `Get`, in the same hold as the flight insert, before unlock. That is required for single-flight (the flight is visible before any other Get can miss) and for the WaitGroup rule (`Close` cannot reach `Wait` before `Add`, because both the flag and the `Add` use `mu`). Moving `Add` into the new goroutine and publishing the flight only after that goroutine is known to be running re-opens one of those two races, so that alternative is not the one used.

The alternative the finding allows is the one specified: `Done` on the Get side if `go` is not reached. `spawn` sets `started = true` only after `go` returns. Its defer, if `started` is still false, calls `finish` with a non-nil error (joiners unblock, nothing is admitted) and `joining.Done`. The panic then continues, and `Get`'s `waiters.Done` runs. A failed start cannot leave `Close` blocked in `joining.Wait`. An unrecoverable runtime throw still kills the process; the recoverable panic is the deadlock this closes.

## nit: The `panic(nil)` note is wrong on this toolchain

**Fixed.**

The previous justification said `panic(nil)` makes `recover()` return nil and fails `TestNewPanics`. That was the pre-1.21 rule. This module is `go 1.25`. On Go 1.21 and later, `recover()` of `panic(nil)` returns a non-nil `*runtime.PanicNilError`, so `TestNewPanics` would pass. The plan no longer cites that rule.

`New` still panics with a non-nil string (`"blockcache: capacity < 1"` or `"blockcache: nil Loader"`) because the message is the precondition failure. That choice does not depend on how `recover` treats nil.
