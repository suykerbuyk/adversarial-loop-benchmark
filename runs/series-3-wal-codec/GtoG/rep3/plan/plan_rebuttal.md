# Rebuttal: `plan/plan_findings.md`

Each finding is accepted. The revised plan is `plan/plan.md`. No implementation was written.

## Blocker — depth legal-maximum contradiction

Accepted. The previous plan's table was right (nine wraps fail because the inner sub-stream is depth 9; the ninth Batch frame itself sits at depth 8). The next sentence was wrong. It said a Batch whose own frame sits at depth 8 is the legal maximum, and that "a Batch frame at depth 8 whose payload is another Batch" is `ErrCorrupt`. That second case is eight nesting levels and must encode and decode. An implementor who followed that sentence would reject a legal stream and would also violate "the encoder will not emit a stream it would itself reject."

Revised rule, one check only: reject when the sub-stream depth at entry is greater than 8. A Batch frame decoded at depth 7 whose payload is another Batch is legal. Eight wraps succeed. Nine wraps fail. The contradictory sentence is deleted. The depth section now has two tables (nine wraps fail, eight wraps succeed) and the public-test mapping for `TestBatchNestedPastDepth8` states that a hand-built eight-wrap stream must decode.

## Major — `encodePut` / `encodeDelete` do not compile on 32-bit

Accepted. `snapshot-v1/walcodec.go:92` and `:107` compare `len(...)` with untyped `math.MaxUint32`. On `GOARCH=386` that is a compile error. The previous "What stays" bullet required a `uint64` compare and then told the implementor to leave the helpers as in v1, including those checks. That contradiction is removed.

"What stays" now says those two comparisons do not stay. The encode section spells the required form: `uint64(len(r.Key)) > math.MaxUint32` and `uint64(len(r.Value)) > math.MaxUint32`, and `uint32(len(...))` only after that compare. Step 4 of the implementation list is this rewrite, not a note that the snapshot lines are fine.

## Major — `encodeBatch` count check is after `encodeAt` and does not compile on amd64

Accepted. The previous snippet called `encodeAt` first, then compared `len(b.Records) > math.MaxUint32`, then wrote `uint32(len(b.Records))`. On amd64 that comparison does not compile. On 386 it is vacuous, so it did not prevent a wrapped count. The prose claimed the check prevented the wrap; the order did not.

Revised `encodeBatch`: `uint64(len(b.Records)) > math.MaxUint32` returns `ErrCorrupt` before `encodeAt`. `uint32(len(b.Records))` is written only after that compare. The recursive depth rejection stays inside `encodeAt` (`depth > 8` before any frame write).

## Minor — `takeBytes` failure mode was described incorrectly

Accepted. After the 4-byte guard, remaining is at least 0. On 32-bit `int(0xFFFFFFFF)` is `-1`, and `-1 > remaining` is false, so the snapshot check does not reject. The panic is the next line: `end := start + int(n)` is `start-1`, and `payload[start:end]` panics. The previous plan said the negative `int` "passes" the comparison as if that were the whole bug.

The decode section now states that panic path and the required order: reject unless `uint64(n) <= uint64(len(payload)-(i+lenSize))`, then convert `n` to `int` and slice. Step 5 tells the implementor to rewrite `takeBytes` that way, not to keep `int(n) > remaining`.

## Minor — `decodeBatchPayload` trusts `n` before the walk

Accepted as a maintenance hazard, not a spec miss. Exact fill still holds if the walker consumes the payload and then compares `uint64(len(inner))` to `n`. A later `make([]Record, n)` would allocate from an attacker-controlled `uint32`.

`decodeBatchPayload` now requires, before the walk, `uint64(n) <= uint64(len(rest))/9` (minimum frame is 9 bytes). The result slice is still grown by appending parsed frames. `make([]Record, n)` and `make([]Record, 0, n)` are forbidden. The exact-fill check remains after the walk.

## Nit — contradictory CRC slice

Accepted. The sketch contained `buf[off+8+int(n) : off+8+int(n)]` with a "wait" comment, then the real slice later. An implementor who transcribed the first expression would read a zero-length CRC and fail every frame. The dead expression is deleted. The only CRC slice in the sketch is `buf[off+4+int(n) : off+8+int(n)]`.

## Nit — two version-byte paths in `encodeAt`

Accepted. The empty-input special case `return []byte{2}` and the later `if depth == 0 { buf = []byte{2} }` could drift apart. The special case is removed. `encodeAt` writes the version byte in one place: `if depth == 0 { buf = []byte{2} }`, which covers empty and non-empty top-level input. Nested calls leave `buf` empty and write no version byte.

## Status

No finding is disputed. The blocker and both majors are fixed in the revised plan. The two minors and two nits are fixed in the same revision.
