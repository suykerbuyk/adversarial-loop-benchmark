# Response to plan findings

1. **Major — waiter cancellation versus resolved flight: Fixed.** `Get` now checks shutdown first, then performs a non-blocking `flight.done` check, and returns the resolved result before considering caller cancellation. `ctx.Err()` is returned only while the flight is still unresolved. The test plan asserts both sides of this boundary, including result winning when resolution precedes cancellation.

2. **Minor — Close wake versus Loader join order: Fixed with an explicit lifecycle distinction.** Close signals `closedCh` at shutdown start so blocked Gets can promptly return `ErrClosed`; Close itself waits for every Loader before it returns, and only then closes `closeDone`. This preserves the interface guarantees that Close joins Loaders and that waiters wake with ErrClosed, while making clear that waiter wake does not imply Loader completion. The test checks Close does not return before the Loader exits and late success is not resident.

3. **Minor — abnormal Loader exit: Fixed.** Completion and WaitGroup decrement are deferred. A panic is re-raised after cleanup; `runtime.Goexit` still runs deferred cleanup and publishes a terminal internal error so the flight cannot strand waiters. This avoids silently swallowing panics while ensuring the flight map and join count are cleaned up.

4. **Minor — missing deterministic priority and Close-join tests: Fixed.** The test plan now includes barrier-controlled rule 7 cases and a Loader held across Close, verifying Close waits and late success is discarded.

5. **Minor — waiter select/context and closure recheck invariant: Fixed.** The plan specifies the three wait events as `flight.done`, caller `ctx.Done()`, and `closedCh`; it explicitly excludes the cache-scoped context from the wait and describes the non-blocking closure recheck independent of `closeDone`.

6. **Nit — nil/zero-length success representation: Fixed.** Successful results are copied using `make` and `copy`, so nil and empty Loader slices return a non-nil zero-length slice.

7. **Nit — shared flight/resident slice mutability: Fixed.** The plan states that published cache-owned bytes are immutable and every hit or waiter copies before returning; no internal in-place mutation is allowed.

8. **Nit — reentrant Loader behavior: Fixed.** The Loader documentation will explicitly warn that calling Close from that Loader deadlocks on its own join, and recursively calling Get for its own key waits on its own unresolved flight.

9. **Nit — closure recheck under mutex: Fixed.** The plan uses a non-blocking receive from `closedCh` for the waiter recheck. Since it closes in the same critical section that sets `closed`, this detects shutdown without reacquiring the state mutex.
