# Response to plan findings (round 2 → round 3)

## R2-F1 (major): the reentrant-Close workaround does not meet rule 8, and the plan should say the spec cannot be satisfied there. **FIXED (accepted).**

The finding is right. A Close that returns while its calling Loader is still running has not
"waited for every in-flight Loader call to return". The round-2 plan claimed compliance where
none was possible.

Round 3 (plan §3.7, "Close called from inside a Loader"):
- **States the conflict.** A Loader that synchronously calls Close on its own cache makes
  rule 8 (join every Loader call) and rule 9 (no deadlock) contradict each other. No
  implementation can satisfy both in that case.
- **Resolves it with the spec's own Loader contract.** The Loader must "return promptly when
  its ctx is Done". Close cancels that ctx before it joins, so a Loader parked inside Close is
  breaking its contract. That is the same kind of violation as a Loader that ignores ctx, and in
  that case rule 8 itself requires Close to block.
- **Keeps rule 8 literally.** `Close` joins every Loader call ever started, with no exclusions.
- **Documents the use as forbidden** in the `Loader` and `Close` doc comments. It is not
  presented as supported or deadlock-free. Rule 9's guarantee is stated as covering all
  contract-respecting use.
- **Removes the round-2 machinery and its tests:** `active`, `loadersInClose`, `drained`, the
  exclusion condition, and the reentrant-Close tests. The join is a plain `sync.WaitGroup`.
  Adding to it is ordered before every `Wait` by the `closed` flag under `mu` (invariant I4).
- **Keeps allowed reentrancy deadlock-free.** A Loader may still call `Get` on other keys, and
  `Len`. Waiters are released by `closing` when Close starts, and a test covers this.

## R2-F2 (major): the `goid()` fallback is not actually safe; a parse failure deadlocks. **FIXED (accepted).**

The finding is correct. Falling back to "not recognized" brings back exactly the deadlock the
heuristic was meant to prevent, so the fallback was not safe. Basing correctness on the
undocumented format of `runtime.Stack` was also a poor foundation.

Round 3 removes `goid()`, `goid.go`, `loaderGs`, and every use of `runtime`. Now that R2-F1 is
resolved by documenting the case and keeping rule 8 literally, nothing needs goroutine
identity. No correctness path depends on a heuristic any more.

## R2-F3 (minor): Loader panic recovery adds unspecified behaviour. **FIXED (accepted).**

Round 3 removes the `recover`. The package policy is now explicit (plan §3.6, and the `Loader`
doc comment):
- A Loader must not panic.
- If it does, the panic is not recovered. It terminates the program with its original value and
  stack, the same as any unrecovered goroutine panic in Go.

Effects of this policy:
- Panic behaviour is never silently lost or turned into an error.
- The no-hang argument is unaffected, because a crashed process cannot leave a waiter or Close
  blocked.
- A user who wants panics turned into errors can wrap their own Loader. That choice stays with
  the user.

The round-2 "Loader panic" test is removed. It is listed under "Not tested" as documented
forbidden use.
