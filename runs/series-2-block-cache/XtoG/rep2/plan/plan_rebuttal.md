# Responses to plan findings

1. **Blocker — post-wake context priority. Fixed.** The plan now rechecks under the mutex in this order: closed; caller `ctx.Err()` only if the flight remains unresolved; resolved flight result. A completed flight therefore cannot be displaced by a caller cancellation that is already visible when the waiter reacquires the lock. Closure remains highest priority.

2. **Major — nil successful payload copy semantics. Fixed.** The cache-owned copy preserves nil, and each Get copies the stored value without converting nil to a non-nil empty slice. Tests will cover initial, coalesced, and resident-hit nil success.

3. **Major — clear flights on Close. Fixed.** After joining all loader goroutines, Close clears residents, the LRU list, and the flights map under the mutex before signaling `closeDone`. Completion also removes only its exact flight on all terminal paths.

4. **Minor — panic cleanup and WaitGroup lifecycle. Fixed.** The plan commits to recovering loader panics as terminal flight errors and requires terminal publication, exact-flight deletion, `done` closure, and WaitGroup release on every path. This prevents a recovered panic from stranding Close or leaving a stale flight.

5. **Minor — nil caller context. Fixed by explicit contract.** `Get(nil, key)` will panic immediately before taking actions that can register a flight. The plan no longer leaves this as an accidental panic in a wait select after starting work.

6. **Nit — `MoveToBack` on insertion. Fixed in wording.** New residents are inserted at the list back as MRU; `MoveToBack` is used for hits. No redundant move is specified for a newly pushed element.

7. **Nit — locked Len snapshot. Rebutted as compliant.** `Len` returns the mutex-protected resident count. This is stricter and clearer than the permitted one-critical-section lag, and admission/eviction under the same mutex guarantees the count never exceeds capacity.
