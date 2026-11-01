# Amendment: `walcodec` v2 — nested `Batch` records

> PUBLIC amendment, handed only after v1 converges. It extends the v1 task
> (`spec/walcodec-v1-cell-facing.md`); every v1 rule still holds except where this text changes it.
> The v1 exported surface is frozen: `Put`, `Delete`, `Record`, `Encode`, `Decode`, `ErrCorrupt`
> keep their names and signatures. The only addition to the surface is the new type `Batch`.

## The new requirement

> Add record type `Batch{ Records []Record }` (Kind 3). Its payload is **itself a framed sub-stream**:
> `[n uint32]` followed by `n` inner frames of the same `[len][type][payload][crc]` shape, and a `Batch`
> may contain `Put`, `Delete`, or nested `Batch` records (recursion). Every frame at every depth uses
> the same 16 MiB `len` cap; an inner `len` that extends past its enclosing payload is `ErrCorrupt`
> (truncated), never a read into the next outer frame. Nesting is capped at **depth 8**; a deeper nest
> is `ErrCorrupt`. Bump the format: `Encode` now writes `version = 2` and may include Batches. `Decode`
> must accept BOTH a version-1 stream (Put/Delete only) and a version-2 stream (all three types,
> nested). A version other than 1 or 2 is rejected (`ErrCorrupt`). Every existing v1 exported symbol
> keeps its name and signature; the new type is `Batch`. `Encode` now emitting version 2 for
> Put/Delete-only input is an allowed behavior change (see regression rule below).

```go
type Batch struct { Records []Record }   // Kind 3; payload is a nested framed sub-stream
```

`Batch{}.Kind()` is `3`.

### `Batch` payload, precisely
```
Batch payload = n(4) frame_1 frame_2 ... frame_n
```
- `n uint32` is the count of inner records; the `n` inner frames follow, each a full
  `[len][type][payload][crc]` frame.
- The `n` inner frames must consume the Batch payload **exactly**. Fewer or more than `n` well-formed
  frames, or trailing bytes, is `ErrCorrupt`.
- An inner `len` that would read past the enclosing Batch payload is `ErrCorrupt` (truncated); the
  decoder never reads into a sibling or outer frame.

### Depth cap (precise convention used by the grader and the reference)
Decoding (and encoding) carries a nesting depth. **The top-level stream is depth 0.** A `Batch`'s inner
sub-stream is decoded one level deeper than the Batch itself. A sub-stream at depth greater than
**8** is `ErrCorrupt`. Equivalently: up to 8 levels of `Batch` nesting are legal; a 9th nesting level
(a sub-stream that would sit at depth 9) is rejected. The encoder enforces the same cap and will not
emit a stream it would itself reject.

### Version rule
- `Encode` writes `version = 2`.
- `Decode` accepts `version = 1` (Put/Delete only) and `version = 2` (all three types). Any other
  version byte — including `0` and `3` — is `ErrCorrupt`, validated before any frame.

### v1 regression rule
The v1 **decode** behaviors must still hold under v2, tested against a **fixed v1-bytes fixture**, not
by replaying `Encode`'s first byte (v2's `Encode` writes `2`). A v1 fixture is a byte stream that
begins with `version = 1`; v2's `Decode` must accept it and return the same records a v1 decoder would.

## v2 public test cases (ship at phase 2; part of acceptance)
1. A `Batch` of `Put`/`Delete` round-trips.
2. A **nested** `Batch` (Batch-in-Batch) round-trips.
3. A v1-encoded (version-1) stream still decodes under v2 — asserted via a fixed v1-bytes fixture.
4. A mixed top-level stream (`Put`, `Delete`, `Batch`, ...) round-trips.
5. A truncated `Batch` payload → `ErrCorrupt`.
6. A stream with `version = 3` → `ErrCorrupt`.
7. A `Batch` nested past depth 8 → `ErrCorrupt`.
8. The v1 decode behaviors still hold via the fixed v1-bytes fixture (crc flip, truncation, the
   round-trip records), NOT by replaying v1 `Encode`'s first byte.
