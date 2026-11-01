# Response to plan findings (round 1)

**1 (blocker): WaitGroup bubble association. FIXED, but the stated mechanism is partly wrong.**
In the round-1 plan, `Close` never calls `WaitGroup.Add`. Only `Get` does, under `mu`. In
`TestSingleFlight` the cache is created, used and closed entirely inside one bubble, and no Get on
that cache ever happens outside it. So the described failure does not occur in the public suite. The
underlying concern is still real: `sync.WaitGroup.Add` is fatal when the group is used both inside
and outside a bubble (confirmed in the go1.27 `sync/waitgroup.go`). Removing the WaitGroup costs
nothing, so I did. Loads are now counted by an `active int` under `mu`. A `drained` channel created
in `New` is closed exactly once: by Close if `active == 0`, otherwise by the `complete` that takes
`active` to 0 while `closed`. A second Close waits on the same `drained`. See plan §2, §3.2, §3.5.
I also documented that a Cache must not cross bubble boundaries, which already applies to its
channels.

**2 (major): discard gated on `c.ctx.Err()`. FIXED (no behaviour change).**
The cache context comes from `context.Background()` and only Close cancels it, after setting
`closed`, so the two conditions were already equivalent: there is no parent cancel and no deadline.
The gate is now `c.closed` alone, which says what is meant and stays robust if someone later
changes how the context is derived. See plan §3.2.

**3 (major): Get could start a load after Close. REBUTTED; already correct, now stated explicitly.**
In round 1, §3.1 already did the closed check, the `items`/`inflight` lookup, the insert, the
counter increment and the goroutine spawn inside one `mu` critical section, with no unlock between
them. Close sets `closed` under the same `mu`. So no Close can fall between the check and the insert.
The plan now says this in words and gives the two possible orders. See plan §3.1.

**4 (minor): closing/done sampling race in `wait`. REBUTTED, with the reasoning added to the plan.**
If the waiter sees `closing` not yet closed and then sees `done` closed, there are two cases:
(i) `done` was closed before Close's critical section. The load resolved before Close began, so the
Get linearizes before Close and returning the value is correct. Re-checking `closing` can never
remove the window between the last check and `return`, so the extra check would only move the
window, not close it. (ii) `done` was closed after Close set `closed`. Then `complete` took the
discard branch and set `cl.err = ErrClosed`, so the waiter returns `ErrClosed` through the done path
anyway. Neither case returns a value that violates rule 7(a) or rule 8. See plan §3.1.

**5 (minor): `ErrClosed` stamped on the call. REBUTTED.**
The stamp is exactly what makes case (ii) of finding 4 correct. It is written only when *this*
cache's `closed` flag is set, so it cannot mean "some other cache closed". It also cannot outlive the
closing signal in a way that matters: `closing` is already closed, and the call has been removed
from `inflight`, so no later Get can reach it. A Loader that itself returns `ErrClosed` is handled
as an ordinary Loader error under rule 3, and the waiters are entitled to receive it. The cache
never stores `ErrClosed` as a cached value, only on a call that is already dead. See plan §3.2, §5.

**6 (nit): defensive overwrite in `admit`. FIXED.**
The branch is gone. A resident key in `admit` now panics with an internal-error message, so a
published entry is never mutated. See plan §3.3.

**7 (nit): recover in `callLoader`. FIXED IN PART.**
- Adopted: the error now includes the panic value (`%v`) and `debug.Stack()`.
- Adopted: the `recover` covers only the user's Loader call. Cache code (`complete`, `admit`) runs
  outside it, so a cache bug such as a nil map write or a double channel close is not recovered and
  crashes loudly.
- Added: completion runs in a `defer` in `runLoad`, so a Loader that calls `runtime.Goexit` cannot
  leave waiters and Close hanging.
- Rebutted: "do not recover runtime panics" in general. Inside the recover's scope, a runtime panic
  can only come from the user's Loader, which is a Loader bug. Letting it escape a background
  goroutine kills the process, and skipping completion would deadlock every waiter and Close, which
  violates rule 9.

See plan §3.2.
