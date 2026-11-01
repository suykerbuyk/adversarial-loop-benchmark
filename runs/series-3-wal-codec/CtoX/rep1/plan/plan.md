# Plan: walcodec v2 — nested `Batch` records (revision 3)

Status: plan only, nothing implemented. Revision 2 answered the round-1 adversary finding (one
`minor`); its changes are marked **[r2]**. The round-2 `plan_findings.md` reports **no remaining
blocker, major, or minor issue**. It confirms that the r2 boundary design (each nested parser gets
only its enclosing payload, CRC is checked before dispatch, three-index slices cap capacity) and the
coverage of the public tests and the fixed v1 fixture are adequate. Revision 3 therefore makes **no
design change**. It only re-confirms the starting-point facts in §0 (worktree still has no v1
source; fixture CRCs re-verified independently). See `plan_rebuttal.md`.

## 0. Starting point (important discrepancy)

`worktree/` currently contains only `go.mod`, `walcodec_public_test.go` and
`walcodec_v2_public_test.go`. It has **no v1 source files**. The converged v1 code lives in
`snapshot-v1/` (`walcodec.go`, `encode.go`, `decode.go`, identical `go.mod`). Step 0 of the
implementation is to copy those three `.go` files into `worktree/` unchanged. The v2 public test file
cannot compile until `Batch` exists, so the v1 baseline is confirmed by running the v1 suite with
the v2 test file temporarily moved aside. After that, every v2 change below is a diff against the snapshot.

The file layout stays the same: `walcodec.go` (types/constants/docs), `encode.go`, `decode.go`.

Verified facts this plan relies on (re-checked in r2; **[r3]** the worktree contents and both
fixture frames, including len, type, CRC, and exact consumption, were re-checked again, independently of Go):
- v1 `Decode` already checks the CRC over `type+payload` **before** `decodePayload` dispatch,
  and it checks `n > maxFrameLen || n == 0` before any slicing.
- The v2 public test's `v1Fixture()` is a valid version-1 stream. Its frames are len 17 / type 1 /
  crc 0x20745b79 and len 9 / type 2 / crc 0x93aa4197. Both CRCs match, and the frames consume
  the buffer exactly.
- **[r2]** In Go, `s[:2]` of a 6-byte slice can legally be re-sliced to `[:5]`. That re-slice
  silently reads the bytes that follow. A three-index slice `s[:2:2]` makes the same re-slice panic.
  This matters for the no-read-into-sibling rule (see §2.2).

## 1. What stays (frozen)

- Exported names/signatures: `Record`, `Put`, `Delete`, `Encode([]Record) ([]byte, error)`,
  `Decode([]byte) ([]Record, error)`, `ErrCorrupt`. Same `Kind()` values (1, 2).
- Frame shape `len(4) type(1) payload(len-1) crc(4)`, big-endian, CRC32-IEEE over type+payload,
  crc outside `len`, `len` cap 16 MiB checked before slicing/allocation, `len == 0` rejected.
- Put/Delete payload layouts, exact-consumption rule for payloads, nil/empty Value equivalence
  (decode always yields a non-nil zero-length slice), no aliasing of the input buffer.
- Empty input / nil → `ErrCorrupt`; empty recs → one-byte stream decoding to an empty non-nil slice.
- Encode policy: two passes. The first validates and sizes the output with overflow-checked
  arithmetic. The second makes a single allocation and runs a write pass that cannot fail.
  Encode accepts values and non-nil pointers. For bad input it returns a plain (non-`ErrCorrupt`)
  error and no output. It never emits a stream it would reject.
- All Decode failures return `(nil, ErrCorrupt)` (the bare sentinel, so `errors.Is` works).
- `readLenPrefixed`, `appendFrame`, `appendLenPrefixed`, `addStreamSize`, `putFrameLen`,
  `deleteFrameLen` are reused unchanged.

## 2. What changes

### 2.1 `walcodec.go`
- New constants: `version2 uint8 = 2`, `kindBatch uint8 = 3`, `maxDepth = 8`,
  `batchCountSize = 4`, and `minFrameSize = lenFieldSize + typeSize + crcFieldSize` (= 9). Every
  well-formed frame is at least 9 bytes, because `len >= 1`.
- The `version1` comment changes from "the only version" to "legacy version, decode-only
  (Put/Delete)". `version2` is "the version Encode writes".
- New exported type (the only surface addition):
  ```go
  // Batch groups records into one frame. Its payload is a nested framed
  // sub-stream: count(u32) followed by count inner frames. Batches may nest up
  // to 8 levels deep.
  type Batch struct { Records []Record }
  func (Batch) Kind() uint8 { return kindBatch }   // 3
  var _ Record = Batch{}
  ```
- Package doc: describe version 2, the Batch payload grammar, the depth convention (top level = 0),
  and that version 1 is still decoded (Put/Delete only). The `ErrCorrupt` doc adds "a Batch nested
  more than 8 levels deep, or whose inner frames do not exactly fill its payload".

### 2.2 `decode.go`: restructure around one shared frame reader

```go
func Decode(b []byte) ([]Record, error) {
    if len(b) < 1 { return nil, ErrCorrupt }
    var d decoder
    switch b[0] {
    case version1: d.allowBatch = false
    case version2: d.allowBatch = true
    default:       return nil, ErrCorrupt      // 0, 3, ... rejected before any frame
    }
    recs := make([]Record, 0)
    rest := b[1:]
    for len(rest) > 0 {                          // top level: frames until end of input
        rec, r, ok := d.readFrame(rest, 0)
        if !ok { return nil, ErrCorrupt }
        recs = append(recs, rec); rest = r
    }
    return recs, nil
}
```

`decoder` is a tiny value struct (`allowBatch bool`). There is no package-level mutable state.

**`readFrame(region []byte, depth int) (rec Record, rest []byte, ok bool)`** is the v1 loop body
lifted into a function. Its **[r2] contract** goes in its doc comment:
> `region` is exactly the enclosing region that frames may occupy: the stream remainder at top
> level, or the unread remainder of the enclosing Batch payload when nested. Never pass a larger
> slice. `rest` is always a suffix of `region`.

Steps, in this order:
1. `len(region) >= 4`, else fail. Read `n`.
2. Reject `n == 0 || n > maxFrameLen` before any slicing or allocation.
3. `len(region) >= 4 + int(n) + 4` (int is safe: n ≤ 16 MiB), else fail. This is the truncation
   check, and in nested context it is *also* the "inner `len` extends past the enclosing payload"
   check.
4. **[r2]** `body := region[4 : 4+n : 4+n]`, a three-index slice whose capacity is capped at `len`.
5. **CRC check over `body` against `region[4+n : 8+n]`, before any payload dispatch [r2: kept
   explicitly as an invariant].** A Batch body is therefore integrity-checked as a whole before
   any of its inner frames are parsed.
6. `rec, ok := d.decodePayload(body[0], body[1:], depth)`.
7. Return `rest = region[8+n:]`.

**[r2] Why the boundary cannot leak (the finding's concern), stated as two layers:**
- *Logical layer.* Nested parsing only ever receives `body[1:]` of the Batch frame. That is the
  Batch payload, and it excludes the Batch's own crc, every sibling, and every outer frame. All
  bounds checks compare against `len(...)` and never against `cap(...)`. An inner frame whose `len`
  runs past the payload fails check 3, even when real bytes follow in memory.
- *Mechanical layer.* Because of step 4, `body` (and therefore every Batch payload and every inner
  region carved from it) has `cap == len`. A future refactor that forgets a bounds check and
  re-slices past `len` therefore **panics** (and is caught by tests) instead of silently reading
  the Batch crc or a sibling frame. The same three-index capping is applied in `decodeBatch` when
  handing the post-count remainder onward (`p[4:len(p):len(p)]`, technically redundant because the
  parent is already capped, kept for local clarity).
- *Test layer.* The sibling-overrun test in §5 is **mandatory**, not optional. It fails if either
  layer regresses.

**`decodePayload(kind, p []byte, depth int) (Record, bool)`**: the Put/Delete cases are unchanged.
New case `kindBatch`: allowed only when `d.allowBatch`. In a version-1 stream, type 3 is an
unknown tag → `ErrCorrupt`. Otherwise it returns `d.decodeBatch(p, depth+1)`. Any other kind → not ok.

**`decodeBatch(p []byte, depth int) (Record, bool)`**, where `depth` is the depth of the
sub-stream it parses:
1. `if depth > maxDepth { return nil, false }` runs first, before reading any byte. Recursion is
   therefore bounded at 9 levels of stack regardless of input.
2. `len(p) >= 4`, else fail. `count := Uint32(p)`; `rest := p[4:len(p):len(p)]`.
3. Early sanity: `if uint64(count) > uint64(len(rest)/minFrameSize) { fail }`. If `count` frames
   cannot fit, this check rejects the batch. It also bounds the preallocation, so a forged
   `count = 0xFFFFFFFF` cannot trigger a huge allocation.
4. `recs := make([]Record, 0, count)`. Loop exactly `count` times calling
   `d.readFrame(rest, depth)`. Any failure (truncation, overrun of the payload, bad crc, bad len,
   unknown/disallowed type, over-depth child) → fail.
5. After the loop, `len(rest) != 0` → fail. This covers more frames than `count`, and trailing bytes.
6. Return `Batch{Records: recs}`, a value (not a pointer, which the public tests type-assert) with
   a non-nil, possibly empty slice.

Cost: each nesting level re-CRCs its bytes, so worst case is about 9× the input size of CRC
work. That is acceptable, and it is bounded by the depth cap.

### 2.3 `encode.go`: recursive size/validate plus recursive write

- `Encode` writes `version2` as the first byte. This is an allowed behavior change, and no public
  test asserts 1.
- The validation pass becomes recursive: `frameBodyLen(r Record, depth int) (uint64, error)`,
  where `depth` is the depth of the stream that `r` sits in (top level = 0):
  - `Put`/`*Put`/`Delete`/`*Delete`: unchanged (nil pointer → error).
  - `Batch` / `*Batch` (nil `*Batch` → error): if `depth+1 > maxDepth`, return the error
    `"batch nesting exceeds depth 8"`. Otherwise
    `n = typeSize + batchCountSize + Σ(lenFieldSize + frameBodyLen(child, depth+1) + crcFieldSize)`.
    Child errors propagate, wrapped with the inner index (e.g. `record 2: batch record 0: ...`).
    The sum bails out as soon as it exceeds `maxFrameLen`, so the walk is bounded and cannot
    overflow `uint64`.
  - The final check `n > maxFrameLen` → error is unchanged, and it applies at every depth.
  - Since each inner frame is ≥ 9 bytes and a Batch frame is ≤ 16 MiB, `len(Records)` always
    fits in the `uint32` count.
  - Cyclic input (a `Records` slice containing a Batch over the same slice, or a `*Batch` that
    reaches itself) stops at the depth check with an error, instead of recursing forever.
  - Shared sub-batches (DAG input) are bounded by the early `> maxFrameLen` bail-out. Each
    successfully sized child adds ≥ 9 bytes, and a failing child aborts the whole walk.
- Write pass: factor the existing type switch into `appendRecord(buf []byte, r Record) []byte`,
  which handles all six value/pointer cases. Add:
  ```go
  func appendBatch(buf []byte, b Batch) []byte {
      return appendFrame(buf, kindBatch, func(p []byte) []byte {
          p = binary.BigEndian.AppendUint32(p, uint32(len(b.Records)))
          for _, r := range b.Records { p = appendRecord(p, r) }
          return p
      })
  }
  ```
  `appendFrame`'s length-placeholder backpatch already works for nested frames. Each level
  backpatches its own `start` offset and computes its CRC only after its inner frames are complete,
  so no sizes are recomputed while writing. Encode keeps its single exact-size `make`. The write
  pass cannot fail, because validation already rejected every bad input.
- Encode doc update: Encode writes version 2 and accepts Put, Delete, and Batch values and non-nil
  pointers, with Batches nested at most 8 levels. It returns an error (not `ErrCorrupt`) for a nil
  record anywhere in the tree, a nil pointer, an unknown type, over-deep nesting, any frame over
  16 MiB at any depth, or an oversize stream.

## 3. How each v2 rule is satisfied

| Rule (amendment) | Mechanism |
|---|---|
| `Batch{Records}` type, Kind 3, only surface addition | §2.1; `var _ Record = Batch{}`; checked with `go doc -all` |
| Payload = `n(u32)` + `n` inner frames, same frame shape | `decodeBatch` uses the same `readFrame`; `appendBatch` uses the same `appendFrame` |
| Batch may contain Put/Delete/Batch (recursion) | `decodePayload`/`appendRecord`/`frameBodyLen` are recursive over all three kinds |
| Same 16 MiB cap at every depth | `readFrame` checks `n > maxFrameLen` at every depth before slicing; the encoder checks every frame |
| Inner `len` past enclosing payload → `ErrCorrupt`, never read into the next frame | `readFrame` receives only the enclosing region (contract); `len`-based bound check; **[r2]** `cap==len` capping makes any accidental overrun panic; mandatory sibling-overrun test |
| `n` frames consume the payload exactly; fewer/more/trailing → `ErrCorrupt` | loop exactly `count` times, then the `len(rest) != 0` check; early `count` vs bytes check |
| Depth: top level 0, sub-stream one deeper, sub-stream depth > 8 → `ErrCorrupt` | `decodeBatch(p, depth+1)` checks `depth > 8` first; 8 nested Batches decode, 9 fail |
| Encoder enforces the same cap and never emits what it rejects | `frameBodyLen` rejects `depth+1 > 8`, the same convention as the decoder |
| `Encode` writes version 2 | first byte `version2` |
| Decode accepts v1 (Put/Delete only) and v2 (all types) | version switch; `allowBatch` is false for v1, so type 3 in a v1 stream → `ErrCorrupt` |
| Other versions (0, 3, …) → `ErrCorrupt`, before any frame | the version switch runs before the frame loop |
| v1 regression via a fixed v1 fixture | the v1 path is the unchanged frame logic with `allowBatch=false`; the v1 public suite still passes because all its cases are version-agnostic |

## 4. Public test cases → expected path

1. Batch round-trip: encoded by `appendBatch`, decoded by `decodeBatch` (depth 1).
2. Nested Batch: a depth-2 sub-stream, with both levels consumed exactly.
3. v1 fixture: version 1 is accepted, and the Put and Delete decode (fixture CRCs verified, §0).
4. Mixed top level: the top-level loop dispatches all three kinds; `Put{Value:nil}` decodes to empty.
5. Truncated Batch (last 2 bytes dropped): the top-level `readFrame` fails the length check → `ErrCorrupt`.
6. `b[0] = 3` → rejected by the version switch.
7. 9 hand-built nested Batches → the 9th Batch's sub-stream is at depth 9 → `ErrCorrupt`.
8. Fixture crc flip → crc mismatch; fixture minus 3 bytes → truncated frame.

The full v1 suite (`walcodec_public_test.go`) also stays unchanged and green.

## 5. Additional private tests (new file `walcodec_v2_internal_test.go`)

Helper names must avoid the public files' `beU32`, `mkFrame`, `recEq`, `recsEq`, `be2`,
`mkFrame2`, `putPL2`, `batchPL2`, `recEq2`, `recsEq2`, `v1Fixture*`. Use e.g. `tFrame`, `tBatchPL`.

Mandatory **[r2]** boundary tests (guarding the finding):
- **Sibling overrun**: version-2 stream = `[Batch frame][Put sibling frame]`. The Batch payload is
  `count=1` plus an inner frame whose `len` is enlarged so that it extends past the Batch payload
  but would fit inside the bytes of the Batch crc plus the sibling frame. The Batch crc is
  recomputed so the outer frame is valid. Expect `ErrCorrupt`. This fails if nested parsing were
  handed the outer remainder.
- **Overrun into the enclosing crc only**: the same idea, but the inner `len` overshoots the Batch
  payload by 1–4 bytes, into the Batch's own crc, with no sibling. Expect `ErrCorrupt`.
- **Nested overrun at depth 2**: an inner-inner frame overruns its depth-2 Batch payload into a
  depth-1 sibling inside the same outer Batch. Expect `ErrCorrupt`.
- **CRC-before-dispatch**: a Batch frame whose inner bytes are valid but whose *outer* crc is
  flipped → `ErrCorrupt`. Also, a Batch whose payload is garbage (bad count) *and* whose outer crc
  is wrong → `ErrCorrupt`, with no panic and no allocation proportional to the forged count.

Other cases:
- Exactly 8 nested Batches decode and round-trip. 9 are refused by `Encode` (non-`ErrCorrupt`
  error, nil output) and by `Decode`.
- `Batch{}` (empty) and `Batch{Records: nil}` round-trip to a zero-length Records slice.
- Version 0, 3, 255 → `ErrCorrupt`; a version-1 stream containing a valid type-3 frame → `ErrCorrupt`.
- Batch `count` larger than the frames present; smaller (an extra frame); trailing garbage bytes;
  payload shorter than 4 bytes; `count = 0xFFFFFFFF` with a tiny payload (no big allocation).
- Inner crc flip with a recomputed valid outer crc → `ErrCorrupt`. Inner `len == 0` and inner
  `len > 16 MiB` → `ErrCorrupt`. Unknown inner type → `ErrCorrupt`.
- A `*Batch` pointer is accepted. A nil `*Batch`, or a nil record inside a Batch → Encode error.
- A self-referential Batch (cyclic slice) → Encode returns an error and terminates.
- A Batch whose total body exceeds 16 MiB → Encode error, even if each child is under the cap.
- Decoded records don't alias the input (mutate the input after Decode; the records are
  unchanged), including values inside a Batch.
- A small fuzz target `FuzzDecode` (seeded with public-test streams): Decode never panics and either
  returns `ErrCorrupt` or records that re-encode and decode to equal records. Run briefly in
  verification. Its value is that it turns any missing bounds check into a caught panic, thanks to
  the `cap==len` capping.

## 6. Verification checklist

`gofmt -l .` must be empty, then `go vet ./...` clean, `go build ./...`, and `go test ./...`
(v1 public + v2 public + private). Also `go test -race` and `go test -fuzz=FuzzDecode -fuzztime=30s`.
The only exported identifiers added are `Batch` and its `Kind` method; check with `go doc -all`.
A code review checks that every `readFrame` call site passes either the top-level remainder or a
Batch-payload remainder, and nothing wider.
