# Code findings: blockcache (CtoC rep3), adversarial review r2

Checks I ran myself on Go 1.27.2: the worktree public test file is byte-identical to instrument/resolve_public_test.go (`cmp`). `go build`, `go vet` and `gofmt -l` are all clean. `go test -race -count=20 ./...` passes. I re-checked rules 1–9 and every claim in code_rebuttal.md against blockcache.go.

- **r1 major (Close deadlocks on a nested Get): fixed.** wait now returns ErrClosed as soon as the caller's ctx is Done while closing. closed.Store(true) runs before cancel() under mu, so a nested waiter woken by the cancelled cache ctx always sees closed and breaks the cycle with loads.Wait. I confirmed this with a throwaway probe (since deleted): 200 rounds of 20 concurrent Gets, where Loaders do nested Gets under lctx and Close fires at random points. No hangs and no races.
- **r1 minor (caller deadline held hostage by the join): fixed** by the same change. TestCloseBeatsCtx now checks it.
- **r1 panic handling: fixed.** `*panicError` keeps the panic value and the stack, and Unwrap works. A probe confirmed that errors.Unwrap reaches the error passed to panic.
- **Other rebuttal claims hold:** the atomic closed mirror is written only under mu, and the dead-fallback rationale and the unbounded-goroutine godoc are as described.

nit: blockcache.go:159-175. Take a waiter whose flight resolved successfully before Close began, but whose goroutine had not yet been scheduled out of the select. It returns ErrClosed rather than the value it already had, and if its ctx is live it also waits for every unrelated Loader to finish the join. Rule 7's "(a) if the cache closes" allows this, but the Get godoc could say that a result which arrived just before Close can still be replaced by ErrClosed.
nit: blockcache.go:296-298. panicError.Error() embeds the full goroutine stack (about 1 KB, many lines; measured at 970 bytes). Every waiter's err.Error(), and so every log line, carries it. Consider keeping Error() to one line and exposing the stack another way, e.g. a %+v Format or a Stack() method reachable via errors.As on a documented interface.
nit: blockcache.go:204-209. If a program runs with GODEBUG=panicnil=1, panic(nil) gives recover()==nil and is misreported as errLoaderExited (Goexit). This is an edge case: the default in go>=1.21 modules turns it into *runtime.PanicNilError.
nit: blockcache.go:28-33 (Loader godoc). It warns about a Loader Getting its own key, but not about mutual recursion (A's Loader Gets B while B's Loader Gets A). That pair hangs both flights and all their waiters until Close. This is inherent to single-flight, but worth a sentence in the godoc.
nit: blockcache.go:262-270. During the join, Len() still reports the old resident entries even though every Get already returns ErrClosed. This matches the spec's cancel→join→drop order, but a short comment on Len/Close would save a reader from thinking it is a bug.

Summary: no blocker or major issues remain. The r1 major and minor findings are fixed and covered by regression tests; only nits are left.
