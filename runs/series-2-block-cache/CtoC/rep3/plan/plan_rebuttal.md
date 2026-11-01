# Response to plan_findings.md (round 1 → round 2)

All nine findings were accepted and fixed. None were rebutted.

1. **major: the Close order (wake before join), and `run` closing `cl.done` early. Fixed.**
   - Close is now: set `closed=true` and call `cancel()` under `mu`; release `mu`; `loads.Wait()`; then, under `mu`, drop the entries and `close(closedCh)` (§3.7). This adds invariant I6: `closedCh` closes only after the join.
   - `run`'s closed branch still closes `cl.done` with a fallback `ErrClosed`, so the flight always resolves. But `wait` no longer hands that error out directly. When it sees `closed==true` it blocks on `closedCh` (after the join) before returning `ErrClosed` (§3.3). So no waiter sees `ErrClosed` while a Loader is still running.
   - Test 14 is the finding's exact scenario (a Loader-side flag checked by the waiter after `ErrClosed`). Test 12 covers a Loader that returns success during the join.

2. **minor: when does "closes" begin for rule 7(a)? Fixed, with an explicit decision.**
   - "Closes" begins at `closed=true`. After the first `select`, `wait` reads `closed` under `mu`. If it is set, the waiter waits for the join and returns `ErrClosed`. So (a) beats (b) even when the waiter's ctx expires during the join.
   - The resulting short overrun of the caller's deadline is bounded by the Loader contract, and is documented in Get's godoc (§3.3).
   - Test 13 covers it end to end, and test 11 covers it with simultaneous readiness.

3. **minor: Goexit or panic leaves the flight unresolved. Fixed.**
   - Resolution now runs in a defer (`resolve`), guarded by a `returned` flag. `runtime.Goexit` becomes `errLoaderExited`, and a panic is recovered into `fmt.Errorf("blockcache: loader panicked: %v", r)`.
   - `loads.Done` is deferred first, so it runs after `resolve`. The policy is documented on `Loader` (§3.4, §1).
   - Tests 6 and 7 cover it.

4. **minor: test 9 could not reliably hit the close-window path. Fixed.**
   - New deterministic synctest test 16: Close is parked in the join (detected with `synctest.Wait`), then the Loader is released. It asserts that Close returned nil, `Len()==0`, the waiter got `ErrClosed`, and a later Get gets `ErrClosed` with no new Loader call.
   - Test 17 is the zero-waiter variant.
   - To make "Close parked" durably blocking, the shared join now uses a channel instead of `sync.Once`. A second Close inside `Once.Do` waits on a mutex, which synctest does not treat as durably blocked, so test 18 would hang. `loads.Wait()` is durably blocking, because its `Add` calls happen inside the bubble.

5. **minor: no deterministic priority tests. Fixed.**
   - Test 11 calls `wait` directly with several channels ready at once (the closing, resolved and ctx-done combinations), 1000 iterations each, so `select`'s randomness is covered.
   - Tests 12 and 13 cover (a)>(c) and (a)>(b) end to end with synctest.
   - The suite runs under `-race -count=50`.

6. **minor: the pre-cancelled-miss policy was undocumented. Fixed.**
   - The policy is kept and now stated in Get's godoc (§1, §3.2).
   - Test 9 pins it: a pre-cancelled miss triggers exactly one load and warms the cache.

7. **nit: the dead defensive branch in `admit`. Fixed.**
   - It is replaced with an invariant panic (§3.5).

8. **nit: nil ctx behaved inconsistently. Fixed.**
   - Get panics with `"blockcache: nil Context"` at entry on every path. This is documented and covered by test 24.

9. **nit: (nil, nil) behaviour was not pinned. Fixed.**
   - It is documented on Get (an empty non-nil block) and covered by test 5.
