# Plan rebuttal, round 3

Point-by-point response to the round-2 `plan_findings.md`. Section and test numbers refer to the
revised `plan.md`.

## major: `run`'s `closed` branch publishes `ErrClosed` instead of the Loader outcome. **REBUTTED, with a deterministic test added**
The finding's reader W did its `closing` recheck before Close's first section and its `cl.done`
recheck after `run`'s `closed` branch. Its interval contains this order:
`W.closing-check < Close.section1 < close(cl.done) in the closed branch < W.done-check`.
Rule 7(c) is the "else" arm: it applies only if the cache has not closed and ctx is not Done while
the load is in flight. For W to linearize to (c), there has to be an instant in its interval where the
load has resolved and the cache is still open. No such instant exists. Before Close.section1 the load
is unresolved, and after it the cache is closed. So the only answers consistent with a linearization
point are `ErrClosed` (any point after Close) or W's `ctx.Err()` (a point before Close, if ctx was
Done). Returning the Loader's `([]byte, error)` would hand out a result that exists only after Close,
as if the cache were still open. That breaks rule 7's priority. It also breaks rule 8's "wakes every
current waiter with ErrClosed", because W was a current waiter when Close ran. The finding's "skip
admit but publish the outcome" would therefore *introduce* a violation. The finding says that "rule 8
does not authorize rewriting the call's published outcome". But nothing is rewritten: the call
resolves for the first time in that branch, and what it resolves to is decided by the state the
spec makes authoritative (closed), not by the discarded Loader return. The "error-result half of test
8b" concern does not apply. That test involves no Close, so the Loader error passes through unchanged
there. Plan changes: section 3 (`run`) now gives this linearizability argument in full. New test 8e
uses `testHookWait` to put a Close (with a ctx-honouring Loader resolving in the `closed` branch)
exactly between W's two rechecks. It asserts `ErrClosed` for both the success and error Loader
variants, then `Len()==0`, then no further Loader call.

## minor: test 8b does not exercise the live race. **FIXED**
Added an unexported test seam, `cache.testHookWait func(cl *call)`. It is nil in production, set only
by internal tests right after `New` and before any Get, and called inside `wait` between the
`closing` recheck and the `cl.done` recheck. The new test 8b uses it to put resolution exactly in
that window, after `ctx.Done()` has already fired. The hook does `close(release); <-cl.done`. The
waiter must return the bytes (error variant: the Loader error). Test 8c covers the opposite case,
where cancellation happens with the load still unresolved at the `cl.done` check, so the answer is
`ctx.Err()`. Test 8e covers Close landing in the window. The old pre-resolved and pre-cancelled test
stays as 8d. It covers the case where both first-select cases are ready.

## minor: dead re-admit branch in `admit`. **FIXED**
Deleted. `admit` now just evicts the back if full, then pushes to the front. Its precondition, "key
not resident", is stated next to it. I2 now carries the structural argument for that precondition:
a key enters `inflight` only after an `items` miss in the same critical section, and enters `items`
only through the unique owning call, which deletes the `inflight` entry in that same section.

## nit: `clone` returns non-nil for an empty success. **FIXED**
This is now documented in the package doc comment (plan section 1) and in section 5. A successful
empty or nil block comes back as a non-nil `[]byte{}`. Errors always come with a nil slice, and callers
must test `err`. Test 16 pins this down.

## nit: `loaderPanicError.Error` uses `fmt.Sprint` on an arbitrary value. **FIXED**
`Error()` now returns `fmt.Sprintf("blockcache: loader panicked (panic value of type %T)", e.value)`.
`%T` formats only the dynamic type via reflection and never calls the value's `Error`/`String`
methods, so it cannot re-enter user code or re-panic. The raw value stays in the unexported `value`
field for debugging. Test 15 uses a panic value whose `String()` panics and checks that formatting
the error is safe.
