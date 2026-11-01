# Response to plan findings (round 1)

**1. major: rule 7(a) priority over 7(b) in the Close join window. FIXED.**
Close now closes a cache-wide `closedCh` in the same critical section that sets `closed`, before it calls `cancel()` and `loaders.Wait()`.
`wait` selects on `cl.done`, `ctx.Done()` and `closedCh`, then resolves in the fixed order (1) `closedCh` → `ErrClosed`, (2) `cl.done` → result, (3) `ctx.Err()`.
In the scenario you described (the Loader does `<-ctx.Done(); sleep 1s`, the waiter has a 100 ms deadline, Close runs at t=0), the waiter gets `ErrClosed`.
Behaviour for a load that resolved successfully, documented in the plan (§3, wait): a waiter that passed the post-wake check before Close began returns its bytes, because it is linearized before the Close.
A waiter still pending when Close begins gets `ErrClosed`, matching "wakes every current waiter with ErrClosed".
Close's return is still ordered after the join, so all its guarantees hold when it returns. Entries are now also dropped in that first critical section, so `Len()==0` as soon as any waiter has seen `ErrClosed`.

**2. minor: no tests for the close window or for Get during the join. FIXED.**
Added `TestCloseWindowPriority`: it uses your scenario plus a variant where Close starts at 50 ms, and asserts `ErrClosed`.
Added `TestGetDuringCloseJoin`: Close is parked in the join behind a Loader that is slow to cancel. Gets for a new key and for the in-flight key return `ErrClosed`, the Loader count does not change, and the test asserts that Close has not returned yet.

**3. minor: a recovered panic becomes an opaque string. FIXED.**
A panic now becomes an exported, documented `*PanicError{Key, Value, Stack}`. `Stack` comes from `debug.Stack()`, and `Unwrap` returns `Value` when it is an error. Callers can detect it with `errors.As`, so a bug is no longer indistinguishable from a transient failure.
It is still not cached, consistent with rule 3. Recovery is kept, rather than letting the panic crash the process, because an unrecovered panic in the load goroutine would take down the program. Without that, waiters would be stranded and Close would never finish its join, contradicting rule 9.

**4. minor: re-entrancy hazards. PARTLY FIXED, PARTLY REBUTTED.**
- *Own-key Get from a Loader:* fixed. Each Loader ctx carries a `loadTag{cache, key}` value. Get returns the exported `ErrSelfGet` when the ctx carries this cache's tag for the same key.
  If the Loader uses an unrelated ctx, such as `context.Background()`, the self-wait still ends on Close, because waiters now wake on `closedCh` without waiting for the Loader (fix 1). Close then completes. Both cases are tested in `TestSelfGet`.
- *Close from a Loader:* rebutted as unfixable in the implementation, and documented instead. Rule 8 requires Close to wait for every in-flight Loader call to return. A Loader that calls Close is asking Close to wait for itself, so any conforming implementation either deadlocks or breaks rule 8.
  Close takes no ctx, so there is nothing to tag, and Go has no supported way to identify the calling goroutine. The hazard is now stated in both the `Loader` and the `Close` doc comments, not only in the plan.
- *Key cycles across Loaders (A → B → A):* these cannot be detected, because each Loader runs under the cache ctx and not under the caller's ctx. They end through the callers' ctx or through Close. This is documented.

**5. nit: `closed || c.ctx.Err() != nil` is redundant. FIXED.** Only `closed` is checked now. A comment explains that `cancel()` is only called after `closed` is set under mu.

**6. nit: `inflight = map{}` in Close is dead code. FIXED.** It is removed and replaced with a comment stating invariant I7: inflight is empty after `loaders.Wait()`.

**7. nit: the `inflight[key] == cl` guard. FIXED.** The delete is now unconditional, with a comment citing I2: there is at most one call per key, and it leaves inflight only in its own runLoad.

**8. nit: a nil ctx panics. FIXED (documented).** The Get doc comment states that ctx must be non-nil and that a nil ctx panics, following the stdlib convention.
