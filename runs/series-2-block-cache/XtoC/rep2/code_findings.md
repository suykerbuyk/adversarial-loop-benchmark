# Code findings: blockcache (XtoC rep2), adversarial review round 3

Verification (re-derived from source, go1.27.2): `go build` and `go vet` are clean, and `gofmt -l` is empty. worktree/blockcache_public_test.go is byte-identical to instrument/resolve_public_test.go, and it passes under `go test -race -count=5`. I ran 8 adversarial tests of my own (in a scratch copy, not committed), each under `-race -count=3`, and all passed:
(1) A Loader that calls runtime.Goexit gives an error and does not hang. A later Get reloads, and Close returns.
(2) A Loader panic becomes an error, and a later Get reloads.
(3) Strict LRU with recency on hits: with a,b,a,c, b is evicted and a is not.
(4) Close window: the waiter's ctx is cancelled, then the Loader ignores ctx. Close does not return before the Loader returns, and neither does a second Close that joins it. The late result is discarded, Len is 0, and a later Get returns ErrClosed.
(5) An orphaned load (the caller's ctx was already cancelled) fills the cache with exactly one Loader call.
(6) When the Loader returns (bytes, err), the Get gets (nil, err) and nothing is stored.
(7) The cache does not keep the Loader's slice, and mutating a returned slice does not change the cache.
(8) Stress: 64 goroutines × 300 Gets with random deadlines, random load errors, mutation of the returned bytes, a Len poller that never saw Len above capacity, and a Close partway through the run.
Round-2 status: every round-2 item that changes behaviour or structure is fixed. Cleanup now runs in a defer with a `returned` flag, so Goexit no longer hangs Close or leaves the key stuck in inflight. The waiter outcome is simplified to closed -> ErrClosed; f.done -> result; else ctx.Err(), and the default branch can only be reached when ctx is Done, so it can no longer return (nil, nil). The redundant closed||ctx.Err() check is gone, with a comment on the invariant. Eviction uses `>=`. The Cache/Get/Len/Close docs now cover ErrClosed, lag, the capacity bound, discarding late results, and orphaned loads. Panic recovery and re-entrancy are documented on Loader. There is still no code_rebuttal.md.

[nit] cache.go invokeLoader: a recovered panic is still formatted with %v. A panic(err) value can't be inspected with errors.Is/As, and neither the stack (debug.Stack) nor the key is captured, so a production panic is hard to diagnose. Wrap error values with %w and include the key, plus the stack if wanted.
[nit] cache.go runLoad: all of the publication logic (normalising the outcome, discarding on close, LRU eviction plus insert, inflight cleanup, close(f.done), loads.Done) is in one large deferred closure. It is correct, but it would read and test more easily as a named `c.finish(key, f, value, err)` method plus a small `c.admit(key, value)` LRU helper, which would also be the natural extension point for Set/Resize.
[nit] cache.go Get hit/resolve paths: copyBytes runs while the global mutex is held, so with large blocks every Get, Len and load completion waits behind a memcpy. There is also no limit on load concurrency (one goroutine per distinct missing key). Both are acceptable under the spec; noted for scalability and extensibility.
[nit] worktree: the only tests are the public suite. Nothing in the repo covers the behaviours that were hardest to get right (Goexit/panic recovery, discarding results in the close window, orphaned-load population, Close joiners), so a regression in any of them would go undetected. Add focused tests.
[nit] Process: no code_rebuttal.md was written for any round, so there is no point-by-point record of what was accepted or disputed. I checked each fix against the source myself (above).

Verdict: No blocker or major issues remain. No minor issues remain either; only nits are left. I checked each of these against the source and confirmed it in testing:
- Single-flight.
- Copying on ingest and on every return.
- Error wins over bytes.
- LRU eviction and insert in one critical section, so Len is never above capacity.
- Results discarded after cancellation, in both the zero-waiter and close-window cases.
- WaitGroup Add only while open, so there is no Add/Wait race.
- Close joins every Loader before returning, and is idempotent, with joiners waiting on closeDone.
- Waiter priority: ErrClosed, then ctx.Err while in flight, then the resolved result.
- Recovery from Loader panic and Goexit.
