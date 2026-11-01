# Response to plan findings

1. **Blocker — caller-context watcher prevents durable synctest blocking: fixed.** Get no longer registers `context.AfterFunc`, derives a context, or selects on `ctx.Done()`. It waits only on the flight completion channel, which is created with the flight inside the cache operation. After waking it rechecks `closed`, then cancellation state, then the result. This removes the outside-bubble cancellation channel from the wait path.

2. **Major — waiter state retained by a loader that does not return: fixed.** The design removes the per-Get waiter set entirely. Each caller owns only its local stack state and drops it when returning; the flight and loader join are separate. Close joins loader goroutines through the WaitGroup. A loader that does not return after cancellation violates the explicit Loader contract, so Close cannot finish its required join in that case, but no registered waiter table is pinned by it.

3. **Minor — concurrent Close lacks a shared completion state: fixed.** `shutdownDone` is initialized in New. The first Close performs cancellation, join, and resident cleanup, then closes this channel once. Any other Close that observes `closed` waits on the same channel before returning.

4. **Nit — copying under the mutex stalls other operations: fixed.** Hits retain the immutable resident slice while holding the mutex, update MRU, unlock, and copy afterward. Flight results are likewise immutable and copied after unlocking.
