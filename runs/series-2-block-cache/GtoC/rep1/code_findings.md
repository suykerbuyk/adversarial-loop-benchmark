# Code findings (adversarial review, round 1)

minor: A panicking Loader crashes the whole process. runLoad (blockcache.go:214-218) runs the Loader on a goroutine owned by the cache, with no recover. The deferred wg.Done runs, then the panic ends the program. The spec does not cover this, but a library cache should either turn the panic into an error sent to every waiter (as x/sync/singleflight does by re-raising it in the caller) or say in the Loader doc that a panic kills the process. Today neither happens, and no test covers it.
nit: A Get whose ctx is already Done still starts a load on a miss (blockcache.go:193-198). I confirmed this: a pre-cancelled Get returns context.Canceled, yet the Loader runs and the key becomes resident (Len 1). With a fast Loader, the same call can also return the data instead of ctx.Err(), depending on whether publish wins the race to mu before wait reads it. Both outcomes are allowed by rules 7(b)/(c), and the extra load is allowed by rule 7, but the behavior is not deterministic and not documented. Either check ctx.Err() before committing the flight, or note the behavior in Get's doc.
nit: A waiter whose load already finished successfully (it woke on f.done) still returns ErrClosed if Close takes mu before the waiter reads the result (decide: closed beats finished). This matches the priority in rule 7(a), but the caller loses a value that existed before Close. Worth one sentence in the Get/Close docs so nobody "fixes" it.
nit: The comments repeat themselves. The wg Add/Wait invariant appears in full twice (cache doc lines 62-69 and the wg field doc lines 93-97). The Close-from-Loader deadlock note also appears twice (interface lines 25-27 and method lines 333-334). The synctest reasoning for not using sync.Once (lines 87-90) explains test tooling inside production struct docs. Keep one copy of each invariant next to the code it governs, so future edits cannot make them disagree.
nit: Most extra-test Loaders ignore their ctx (for example, a bare `<-release` in TestOrphanSuccessPopulates and TestCloseJoinsDiscardsAndWakes). That breaks the Loader contract ("MUST return promptly when its ctx is Done"). It is deliberate where a test checks that Close joins, but in the other tests it means a regression in cancellation would hang instead of failing. Prefer `select { case <-release: case <-ctx.Done(): }` except where non-cooperation is the point being tested.

Verification performed (go1.27.2):
- go build, go vet, and gofmt -l are clean.
- go test -race -count=200 passes.
- go test -count=500 -cpu=1,2,8 passes.
- The public test file is byte-identical to instrument/resolve_public_test.go.
- My own adversarial tests passed under -race -count=3:
  - Rule 7(b): ctx cancel during a load returns ctx.Err(), and the orphaned load still populates the cache with exactly one Loader call.
  - A zero-waiter success that finishes after cancellation is discarded. Close does not return before the Loader does, Len is 0 afterwards, and no goroutines leak.
  - A 200-iteration chaos run (16 goroutines doing Gets with mixed deadlines and erroring keys, a goroutine polling Len, and 3 concurrent Closes): Len never exceeded capacity, no Loader was running when Close returned, Len was 0 after Close, outcomes were limited to {value, ErrClosed, DeadlineExceeded, loader error}, and there were no deadlocks or races.

I re-derived these from source:
- Rules 1-6: copies happen at publish and at every return, MRU is set on hit and on admit, the tail is evicted before insert under mu, and the error path does not store.
- Rule 7: the decide priority is closed > finished > callerErr.
- Rule 8: closed is set under mu before loadCancel, wg.Add happens only under mu while !closed, and so it cannot race wg.Wait. A second Close blocks on joined outside mu.
- Rule 9: mu is never held across a Loader call, a channel wait, or wg.Wait.

No blocker or major issue remains.
