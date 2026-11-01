# Plan rebuttal, round 3

## About the findings file

The round-2 `plan_findings.md` is not a list with one tagged finding per line. It is the reviewer's
working notes. Its conclusion is that **no blocker or major issue remains**. The reviewer calls the design
sound, accepts the round-2 rebuttals of findings 1 to 3, and lists a set of minor and nit items.

Below, every actionable item from that file is numbered in the order it first appears. Each one is marked
**Fixed** or **Rebutted**, with the reason. Plan changes are marked **[R3]** in `plan.md`.

---

## 1. "major:" on line 1 (Close/`closing` ordering, double-load): **Rebutted. The reviewer withdrew it.**

The line itself ends with "No double-load. Scratch the double-load theory." The follow-up timeline in the
findings shows that a waiter woken by `closing` and a late runLoad agree: nothing is stored and the waiter
gets ErrClosed. No defect is left. The plan still makes the same guarantees, and the R3 change to `wait`
(item 9) makes the decision stricter.

## 2. Round-1 findings 1 to 3 re-examined: **No action. The reviewer accepts the round-2 resolutions.**

- **Finding 1.** The reviewer does not re-raise it.
- **Finding 2.** The reviewer downgrades the unbounded `loads.Wait()` to at most a nit, and agrees that rule
  8 forbids a timeout. The documentation part of it is handled in item 4.
- **Finding 3.** The reviewer says the ErrClosed substitution is correct, given I6.

## 3. Len can stay non-zero while Close is joining ("borderline, not major"): **Rebutted, with the reasoning added to the plan.**

Rule 8 makes "drops all entries" an effect of Close. That effect is visible once Close returns: the drop's
critical section comes before `close(closeDone)` and before Close returns. Every `Len()` that starts after
Close returns sees 0, and so does every later `Len()`, because I4 blocks new admissions.

A `Len()` running at the same time as Close that sees entries is ordered before the drop. The spec never
requires `Len()==0` while Close is still joining, and the capacity bound (the only hard bound on `Len`) is
never exceeded. Dropping entries *before* the join would buy nothing: late loads are discarded anyway. It
would only add a second clear. `plan.md` §3 Close now states this argument.

## 4. minor: the "Close blocks on a cancel-ignoring Loader" warning sits on `Loader`, not on `Close`: **Fixed.**

The `Close` doc comment now says:
- Close blocks until every in-flight Loader call returns.
- A Loader that ignores cancellation therefore blocks Close, and any concurrent second Close.

See §1 of `plan.md`.

## 5. minor: `wait`'s three selects are easy to "simplify" into one select: **Fixed.**

`wait` was restructured anyway (item 9). The code must now carry a comment saying two things:
- Collapsing it into one `select` is incorrect, because Go picks at random among ready cases.
- The `switch` order (resolved > closed > own ctx) is the rule-7 contract.

`plan.md` §3 `wait` and the review checklist both require this comment.

## 6. minor / nit: `entry.val` and `call.val` alias the same array: **Fixed (documented).**

I5 now states the aliasing explicitly. The plan requires a comment at the single assignment site
(`admit(key, own); cl.val = own`) and on both struct fields. The comment says neither field may ever be
written, and that splitting them requires a separate copy for each.

## 7. minor: the stress oracle is too loose to catch broken single-flight or a cached error: **Fixed.**

The stress test now checks two more things.
- **Single-flight.** A per-key atomic "loads in progress" counter is checked when the Loader is entered. Any
  value above 1 fails the test, which pins rule 2 under the real scheduler, not only under synctest.
- **Errors not cached.** Errors are injected behind an atomic flag. After phase 1 the flag is turned off, and
  every key is fetched once with a live ctx. Each fetch must succeed, so an error that had been cached would
  fail it (rule 3).

The allowed outcomes are also tightened: `ctx.Err()` is accepted only for a Get whose ctx was cancelled.

## 8. nit/minor: no test for a hit with an already-cancelled ctx: **Fixed.**

Test added. A hit with a cancelled ctx returns the bytes, with nil error and no Loader call. A later
eviction then shows the hit still marked the key MRU. The `Get` doc comment and the `Get` algorithm notes now
say that a hit ignores the caller's ctx.

## 9. nit: race window in `wait`, where `done` closes after the non-blocking check but before `return ctx.Err()`: **Fixed (design change).**

The reviewer's suggested nested select still leaves the same window. The plan closes it properly instead.

- **Fast path.** If the first select wakes on `cl.done`, the result is returned directly.
- **Slow path.** If it wakes on `closing` or `ctx.Done()`, `wait` takes `mu` briefly and snapshots two
  things: whether `cl.done` is closed (a non-blocking receive) and `c.closed`. It then releases `mu` and
  decides in priority order: resolved (c), then closed (a), then own ctx (b).

This snapshot is consistent:
- runLoad closes `done` under `mu`.
- Close sets `closed` and closes `closing` under `mu`.

So "load still in flight and cache still open" is exactly true at the moment of the decision, and the
decision is linearizable.

There is no deadlock risk. The critical section holds only a non-blocking receive and a bool read, `wait`
holds no other lock, and the result is still copied outside `mu`. The hot path, waking on `done`, never takes
the lock. White-box tests (i) to (v) cover every branch.

## 10. nit: no test for a normal Loader error when the caller's ctx is also cancelled: **Fixed.**

White-box case (v) was added: `done` closed with a normal Loader error, cache open, ctx cancelled. The
expected result is that Loader error, since (c) beats (b).

## 11. nit: no test for a Get whose ctx is already cancelled before a cold-key miss: **Fixed.**

Test added. The Loader blocks. A Get with a pre-cancelled ctx returns `context.Canceled`, and the Loader was
entered exactly once. After the release, the value is resident (`Len()==1`) and a live Get hits.

The plan now also states why such a Get must still start the load. It runs under the cache ctx (rule 7),
and starting it means a cancelled caller can neither split single-flight nor stop the value from being
cached.

## 12. nit: no test for concurrent admissions entering recency in completion order: **Fixed.**

A "completion-order recency" test was added. Capacity 2. `x` and `y` load concurrently, and `y` is released
first. Loading `z` must then evict `y`; `x` still hits. `admit`'s notes in the plan now state the
completion-order semantics.

## 13. minor (robustness): if the first Close panicked between `closed = true` and `close(closeDone)`, every later Close would hang: **Fixed.**

The first Close now runs `defer close(c.closeDone)` right after it releases `mu`. Later Closes are released
on every exit path. This costs nothing, even though nothing in that span can realistically panic.

## 14. nit: unbounded in-flight goroutines should be commented so nobody "fixes" them: **Fixed.**

`plan.md` §2 now requires a code comment on `inflight`. It explains that in-flight loads are uncapped
because rules 5 and 7 require it: they are not counted, not evictable, and orphaned loads are not leaks.

## 15. nit: document that callers must call Close: **Fixed.**

The `Close` doc comment now says callers must call Close when they are done. Close releases the
cache-scoped context and joins the load goroutines.

## 16. nit: the panic wrap uses `%v`, so an `error` panic value cannot be matched: **Fixed.**

`callLoader` now wraps with `%w` when the recovered value is an `error`, and uses `%v` otherwise. The key is
still never included. The panic test now also asserts `errors.Is` for an error panic value.

## 17. Note: the "Loader mutates its returned slice after returning" test could be racy: **Fixed (test clarified).**

The reviewer concluded this was not a defect, provided the mutation happens after Get returns. The plan now
says so explicitly. The test overwrites the slice only *after* `Get` has returned, so the copy is
synchronized before the write. The runLoad notes in `plan.md` also say that a Loader which keeps writing to
its returned slice from another goroutine is breaking the rule-4 handoff.

## 18. Note: `closing` and `closed` duplicate one signal and could drift apart: **Fixed (documented as an invariant).**

New invariant I7: `closing` is closed exactly when `closed` becomes true, in the same critical section, at
the single site in Close. The code must have a comment there saying the two change together. Since item 9,
`wait`'s decision reads `c.closed` under `mu`. `closing` is only a wake-up signal, so a drift could at worst
delay a wake-up. It could not cause a wrong result.

## Items confirmed correct, no action

The reviewer re-derived the following and found them correct:
- hit-path clone safety;
- waiter visibility of `cl.val`;
- deadlock-freedom of a second Close;
- Get during Close;
- `errors.Is(ErrClosed)`;
- New panics;
- single-flight under synctest;
- error not cached;
- LRU order;
- rule 3 with bytes plus an error;
- Loader receives the cache ctx;
- orphaned loads admit;
- (a) over (b), and (c) over (a);
- WaitGroup Add/Wait ordering;
- `close(cl.done)` under `mu`;
- `clear` and `list.Init` on go 1.25;
- `admit` termination for capacity ≥ 1;
- rule-8 "has not yet passed the closed check".

All of these hold unchanged in round 3.
