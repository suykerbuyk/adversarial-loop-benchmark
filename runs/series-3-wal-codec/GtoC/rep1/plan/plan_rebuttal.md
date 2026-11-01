# Rebuttal: plan findings for `walcodec` v2

All six findings are accepted. The revised plan in `plan/plan.md` states the closed rule in each case. Nothing below disputes a finding. Rules accepted in earlier rounds stay in force, including no allocation sized from `n`, exact consumption by at-most-`n` plus an end check, the short-payload guard, early abort in `frameSize`, reserve-and-backfill, the 386 compile constraint, depth and version on one `decoder` value, and CRC checks at every enclosing frame.

## Major

**Off-by-one in the Batch cap check.** Accepted. `frameSize`'s Batch `total` starts at `5` (type byte + `n`) and grows by each child's full frame (`4 + childLen + 4`). That `total` is type+payload, the same quantity `putFrameLen` and `deleteFrameLen` return and the same quantity the length word stores. The check is `total > maxFrameLen`. Equality is legal.

A `Delete` child with key length `16<<20 - 17` makes type+payload `16777217`. The size pass returns `errFrameTooLarge` before `Encode` calls `make`. Key length `16<<20 - 18` makes type+payload `16777216` and round-trips, with the length word equal to `maxFrameLen` and `len(b) == maxFrameLen+9`.

The same pair is tested one level down. `frameSize(inner, 1)` accepts key length `16<<20 - 18` with result `maxFrameLen`, and rejects key length `16<<20 - 17` with `errFrameTooLarge`. `Encode` of an outer `Batch` around either inner returns `errFrameTooLarge` with `testing.AllocsPerRun == 0`: the legal inner frame already fills the cap, so the outer type+payload is `maxFrameLen+13` and cannot be emitted; the illegal inner fails in the child call before the outer sum is updated.

`streamSize` does not add a second top-level cap test. The backfill `frameLen > maxFrameLen` check stays as a drift guard so a later edit cannot emit a length word above the cap. The boundary tests measure the size pass: the `maxFrameLen+1` input returns a nil buffer, `errFrameTooLarge`, and zero allocations. The `len(Encode(x)) == streamSize` assertion is kept and is not treated as the check for this bug, because a wrong comparison can still be self-consistent with the bytes written.

## Minor

**Self-referential Batch construction.** Accepted. `Batch` is a value type. `b := Batch{}; b.Records = []Record{b}` stores a copy whose `Records` is nil and is a legal depth-1 nest. The plan specifies the cyclic construction only:

```go
recs := make([]Record, 1)
b := Batch{Records: recs}
recs[0] = b
```

`Encode([]Record{b})` must satisfy `errors.Is(err, errTooDeep)`. The cycle reaches depth 9 after 9 calls, before any size cap is hit. The value-copy construction is not used in the plan or the tests.

## Nit

**Short-payload parenthetical.** Accepted. Type+payload lengths 1, 2, 3, and 4 are the type byte plus 0–3 payload bytes. The plan says 0–3. The tests cover payload lengths 0, 1, 2, and 3, each with a valid outer CRC, prefixed with version 2. Each returns `ErrCorrupt` and does not panic.

**`decodeAtMost` limit left open.** Accepted. The walker is split into `decodeUntilEnd(span)` and `decodeExactly(span, n uint32)`, sharing `nextFrame`. There is no limit argument and no sentinel. `decodeExactly` counts with a `uint32` and does not convert `n` to `int`. It is called only after `uint64(n)*13 <= len(rest)`. On `GOARCH=386`, `int(uint32(0xFFFFFFFF)) == -1`, so a `-1` sentinel is not used even though the bounds check would currently run first.

**Size-pass total not observable.** Accepted. The size pass is the unexported helper `streamSize(recs []Record) (int, error)`, which `Encode` calls to choose `cap`. The test asserts `len(b) == n && cap(b) == n` for an empty Batch, a nested Batch, and a mixed stream, with `n` taken from `streamSize`.

**`appendBatch` had no default arm.** Accepted. The child switch has `Put`, `Delete`, `Batch`, `nil`, and `default`. The nil and default arms return `errUnknownRecord` wrapped the same way as the v1 top-level loop (`%T`, or the word `nil`). They do not skip the child. `n` is already written as `len(Records)`, so a skipped child would emit a count that disagrees with the frames that follow. `frameSize` has the same arms, so a drift between the two passes cannot emit that stream.
