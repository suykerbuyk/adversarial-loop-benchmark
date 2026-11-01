# Response to adversary findings

- **Blocker — cancellation wakeup treated as proof the flight is unresolved:** Fixed. `Get` now reacquires the mutex after every wakeup and checks current state in priority order: closed first, caller cancellation only if the flight is still unresolved, then the resolved result. It does not infer flight state from the selected channel.
- **Major — completion can admit after Close clears entries:** Fixed. Completion admission is guarded by the mutex-protected `closed` flag, which is set before cancellation and cannot be bypassed by a late Loader return. Close also clears residents again after joining all loaders, before it returns.
- **Major — waiter result copy and Loader slice ownership unspecified:** Fixed. The Loader's bytes are copied before publishing to the flight. Every waiter copies the immutable flight-owned bytes while holding the cache mutex, so callers get independent slices and no completion or eviction can mutate the source during copying.
- **Minor — concurrent Close may return before the first Close joins:** Fixed. Every Close caller waits on the same WaitGroup. Observing the shutdown signal is not treated as completion of Close. Close performs its final resident clear after the join.
- **Minor — Len synchronization unspecified:** Fixed. Len takes the cache mutex while reading the resident count.
- **Nit — waiter might re-lookup the key after waking:** Fixed. A joining Get saves and retains the flight pointer across its wait and consumes that flight's outcome without key re-lookup.
- **Nit — duplicate LRU element on update:** Fixed. Completion replaces bytes and moves the existing element to MRU if the key is resident; it pushes a new element only when absent, then evicts from the tail as needed.
