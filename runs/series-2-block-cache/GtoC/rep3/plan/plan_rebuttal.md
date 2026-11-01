# Rebuttal of plan findings

blocker: Fixed. The wait loop now returns after `ctx.Err()` when the cache is still open and the flight is still unresolved. Order is closed, then resolved, then `ctx.Err()`, then `select`. The `ctx.Err()` arm re-snapshots once so a Close or a resolution that landed after the first snapshot still wins, then returns. It does not loop and does not `select` on an already-closed caller context, so a cancelled waiter cannot busy-lock `mu`. Rule 7(b) is met. Tests 1, 3, and 14 depend on that return.

major: Fixed. Private tests stay in `worktree/cache_test.go` as package `blockcache`, and every test is named `TestImpl_...` and every helper `impl...`. The plan forbids redeclaring `testCtx`, `mustGet`, `assertPanics`, and any other public helper, and forbids a `Test` name that does not start with `TestImpl_`.

minor (panic / Goexit): Fixed. `publish` uses `defer mu.Unlock()` and a `published` flag under `mu`. There is one normative goroutine shape: an outer `wg.Done` defer and one inner finisher. The finisher publishes if `!published`, with a `loaderFailure` that records a panic value or "exited without returning". `runtime.Goexit` still runs defers, so the finisher publishes, closes `done`, and `wg.Done` runs. A panic inside `publish` unlocks before the finisher retries. The two-defer sketch is gone. Test 16 locks Goexit.

minor (Close drop timing): Fixed. The first Close critical section sets `closed`, replaces `entries` with an empty map, clears `head` and `tail`, and cancels, before `wg.Wait`. `Len` is 0 while a Loader is still running. `publish` discards when `closed` is set, so the map cannot refill. After `wg.Wait`, if `inflight` is not empty, Close closes any unpublished `done` channels and clears the map. It does not panic and does not block. That branch is specified as defensive and unreachable when the finisher contract holds.

minor (already-done wording): Fixed. The cold-key already-Done behavior is stated as an implementation choice, not as a spec requirement. The plan requires that sentence in the package comment and as an append to the verbatim `Get` doc comment, and says not to flip it as a bug fix. Test 13 locks the choice.

nit (doc comments): Fixed. The `Cache` methods, `Loader`, and `New` keep the spec comments verbatim. The re-entry note and the already-Done choice are appended, not substituted.

nit (panic error): Fixed. The panic is stored in an unexported `loaderFailure` with the original value and `debug.Stack()`, both included in `Error()`. `fmt.Errorf("%v", rec)` is not used.

nit (map pre-size): Fixed. `New` allocates `entries` and `inflight` with no capacity hint. `New(math.MaxInt, load)` must not pre-size. No `min(capacity, N)` hint either.

nit (test 9 drafting): Fixed. Test 9 is only the simpler form: capacity 1, one in-flight miss, `Len() == 0` until success, then `Len() == 1`. The optional alternate is removed.

nit (prior rounds): Acknowledged. Those items stay fixed. This round also closes the wait-loop exit that the previous replacement omitted, and finishes the panic finding that was only partly fixed.
