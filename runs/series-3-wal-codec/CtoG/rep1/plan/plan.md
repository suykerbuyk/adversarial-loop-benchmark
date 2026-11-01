# Plan: `walcodec` v2: nested `Batch` records

Phase 2, implementor plan, **round 3**. This revision responds to `plan_findings.md` (p2 r2). The
point-by-point response is in `plan_rebuttal.md`. Sections changed since r2:
- §2.1: new `minRecordBody` constant.
- §2.2: one `corruptAt` helper, so every frame, payload and batch error carries offset **and** depth.
  `batch` now takes the Batch frame's own depth. The inner-len-past-payload case is named.
- §2.3: a child guard against `minRecordBody`, a single `walcodec:` prefix rule, and a rewritten
  bounded-work proof with exclusive-byte accounting.
- §5: an empty-node fan-out test, a derived cap-boundary formula, depth-in-message tests and a prefix test.

## 0. Baseline and file layout

**Worktree state.** The worktree holds only `go.mod`, `walcodec_public_test.go` (the v1 public suite) and
`walcodec_v2_public_test.go` (byte-identical to `instrument/`). The converged v1 implementation
(`walcodec.go`, `doc.go`) exists only in `snapshot-v1/`. `snapshot-v1/go.mod` is identical to the
worktree's.

- **Baseline:** the first implementation step copies `snapshot-v1/walcodec.go` and `snapshot-v1/doc.go`
  into the worktree. They are then modified as described below.
- **Files after v2:**
  - `doc.go`: format section rewritten for v2.
  - `walcodec.go`: modified.
  - `walcodec_internal_test.go`: new, holding the v2 internal tests and fuzz targets.
  - Both public test files: untouched.
- **Name collisions.** The internal test must not redeclare any helper from either public file:
  `beU32`, `mkFrame`, `recEq`, `recsEq`, `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `recEq2`, `recsEq2`,
  `v1Fixture`, `v1FixtureRecords`. It reuses them instead.

## 1. What stays unchanged

- **Exported surface.** `Record`, `Put`, `Delete`, `Encode`, `Decode` and `ErrCorrupt` keep their names,
  signatures and documented semantics. The **only** new exported identifier is `Batch`, plus its `Kind`
  method.
- **Frame shape.** `len(4) type(1) payload(len-1) crc(4)`, big-endian.
  - `len` counts type+payload only.
  - The crc is CRC-32/IEEE over type+payload.
  - The cap is 16 MiB, checked before anything sized by `len` is read or allocated.
  - `len == 0` is corrupt.
- **Payloads.**
  - The Put and Delete payloads are unchanged.
  - Parsing stays strict: trailing bytes inside a payload are corrupt.
  - A nil or empty `Value` decodes to a non-nil, length-0 `[]byte`.
- **Decode contract.**
  - Every failure returns a `nil` slice and an error wrapping `ErrCorrupt`.
  - Decoded records never alias the input.
  - A version-only stream gives an empty, non-nil slice.
  - `Decode(nil)` and `Decode([]byte{})` are corrupt.
  - Frame-level errors carry the absolute byte offset, as in v1. They now also carry the depth (§2.2).
- **Encode contract.**
  - Accepted types: `Put`, `*Put`, `Delete` and `*Delete` (non-nil), now joined by `Batch` and `*Batch`.
  - Dispatch is on concrete type, never on `Kind()`.
  - Every failure returns a `nil` slice and an error that does **not** wrap `ErrCorrupt` (the v1
    convention).
  - Every error starts with `walcodec: `.
  - An empty `recs` gives a one-byte stream.

## 2. What changes

### 2.1 New type and constants
```go
// Batch groups records into one frame whose payload is a nested framed
// sub-stream. Records may hold Put, Delete and further Batch values, nested
// at most 8 deep.
type Batch struct{ Records []Record }

// Kind returns 3, the wire tag for Batch.
func (Batch) Kind() uint8 { return kindBatch }
```

New unexported constants:

- `version2 byte = 2` and `kindBatch uint8 = 3`.
- `maxDepth = 8`.
- `frameOverhead = lenSize + crcSize` (= 8): the bytes a frame occupies outside its own `len`.
- `minRecordBody = 1 + u32Size` (= 5): the smallest `len` (type+payload) of any frame that can decode
  successfully. Every decodable payload carries at least one 4-byte length or count field:
  - Delete: `klen`.
  - Put: `klen` + `vlen`, so ≥ 9.
  - Batch: `n`.
- `minRecordFrame = frameOverhead + minRecordBody` (= 13): the smallest full frame on the wire.

The comments on these constants state the derivation. They also state that the constants are used
**only** as feasibility bounds (on a decoded count, on an encoder child budget) and never as a framing
rule.

`version1` stays. The `Record` doc comment is updated to list `Batch` and `*Batch` among the encodable
concrete types.

### 2.2 Decode: one frame reader over bounded regions, with absolute offsets and depth

**One error helper.** A single helper builds every error raised inside a frame:

```go
// corruptAt reports a failure at absolute offset off in a stream at the given depth.
func corruptAt(off, depth int, format string, args ...any) error
    // -> "walcodec: corrupt stream: <detail> at offset <off> (depth <depth>)", wrapping ErrCorrupt
```

- `frame`, `payload` and `batch` return errors **only** through `corruptAt`, or by passing a callee's
  error through unchanged. None of them calls the plain `corrupt`.
- So no message at any depth can drop the offset or the depth.
- Plain `corrupt` stays only for the two stream-level checks that come before any frame: "missing version
  byte" and "unknown version %d".
- `depth` is always the depth of the **stream that holds the failing frame**. The top level is 0.

```go
type decoder struct{ allowBatch bool } // false for version-1 streams

func Decode(b []byte) ([]Record, error) {
    if len(b) == 0 { return nil, corrupt("missing version byte") }
    if b[0] != version1 && b[0] != version2 { return nil, corrupt("unknown version %d", b[0]) }
    d := decoder{allowBatch: b[0] == version2}
    recs := []Record{}
    for rest, off := b[1:], 1; len(rest) > 0; {
        rec, r, err := d.frame(rest, off, 0) // top-level stream is depth 0
        if err != nil { return nil, err }
        off += len(rest) - len(r)
        recs, rest = append(recs, rec), r
    }
    return recs, nil
}

// frame reads one frame from the front of region. region is a stream at the
// given depth; off is the absolute offset of region[0] in the caller's buffer.
// It returns the record and the bytes after the frame.
func (d decoder) frame(region []byte, off, depth int) (Record, []byte, error)
    // 1. len(region) < lenSize   -> corruptAt(off, depth, "truncated frame header")
    // 2. n > maxFrameLen         -> corruptAt(off, depth, "frame len %d exceeds 16 MiB cap", n)
    // 3. n == 0                  -> corruptAt(off, depth, "empty frame")
    // 4. uint64(len(region)-lenSize) < uint64(n)+crcSize
    //        -> corruptAt(off, depth, "truncated frame: len %d needs %d bytes, %d remain in region", n, n+crcSize, len(region)-lenSize)
    //    This is the amendment's "inner len past enclosing payload" case. At depth ≥ 1, region is
    //    exactly the enclosing Batch payload's unread tail, so the check never consults an outer or
    //    sibling frame. The code comment names this case, so the top-level and nested truncation paths
    //    stay one check with one message format. Any later split must keep corruptAt.
    // 5. crc mismatch            -> corruptAt(off, depth, "crc mismatch")
    // 6. rec, err := d.payload(body[0], body[1:], off, depth)
    //    (off is the frame's offset, so payload errors point at the frame that holds them)
    // return rec, region[lenSize+n+crcSize:], err

func (d decoder) payload(kind uint8, p []byte, off, depth int) (Record, error)
    // kindPut, kindDelete: the v1 parsing, unchanged. Malformed payloads give
    //            corruptAt(off, depth, "malformed put payload") (or "malformed delete payload").
    // kindBatch: if !d.allowBatch -> corruptAt(off, depth, "batch record in version-1 stream");
    //            otherwise return d.batch(p, off, depth)
    // default:   corruptAt(off, depth, "unknown record type %d", kind)

// batch decodes the payload p of a Batch frame. The frame is at absolute offset
// off in a stream at the given depth; its sub-stream is at depth+1.
func (d decoder) batch(p []byte, off, depth int) (Record, error)
    // 1. depth+1 > maxDepth      -> corruptAt(off, depth, "batch sub-stream depth %d exceeds 8", depth+1)
    //    This is the same predicate the encoder uses (§2.3 step 1).
    // 2. len(p) < u32Size        -> corruptAt(off, depth, "truncated batch count")
    // 3. n := Uint32(p); p = p[u32Size:]; sub := off + lenSize + 1 + u32Size   // absolute offset of inner frame 1
    // 4. uint64(n) > uint64(len(p))/minRecordFrame
    //        -> corruptAt(off, depth, "batch count %d cannot fit in %d bytes", n, len(p))
    // 5. recs := make([]Record, 0, n)    // n ≤ len(p)/13, bounded by step 4
    // 6. for i := uint32(0); i < n; i++ {
    //        rec, r, err := d.frame(p, sub, depth+1); err -> return nil, err   // passed through unchanged
    //        sub += len(p) - len(r); p = r; recs = append(recs, rec)
    //    }
    //    Running out of bytes before n frames surfaces as frame step 1 or step 4, at that inner frame's
    //    absolute offset and depth+1.
    // 7. len(p) != 0             -> corruptAt(sub, depth+1, "%d trailing bytes after %d batch records", len(p), n)
    // 8. return Batch{Records: recs}, nil  // Records is non-nil even when n == 0
```

- **Offsets and depth.** Every error raised by `frame`, `payload` or `batch` carries the **absolute**
  offset in the original buffer and the stream depth. A WAL operator can therefore locate the failing
  byte and its nesting level directly.
- **Error contract.** Errors propagate unchanged: they all wrap `ErrCorrupt`, and `Decode` returns `nil`
  for every one of them.
- **Stack.** Recursion is at most 9 `batch` levels deep (the 9th is rejected at step 1), so there is no
  stack risk.
- **Bound on the capacity hint.** Step 4 limits each hint to `len(p)/13` entries. A failing decode can
  hold at most one in-progress hint per nesting level. The nested payloads lie inside one another within
  one ≤16 MiB top-level frame. So the transient hint memory is at most about 9 × (16 MiB/13) × 16 B,
  which is linear in the input. A hostile `n = 0xFFFFFFFF` is rejected before `make`.
- **Decode work.** Every inner frame is a distinct byte range of the input; decoding cannot alias. The
  crc pass covers each byte once per enclosing frame, so the cost is at most 9 × the input size.
- **Inner integrity.** Inner bytes are covered by the outer crc. Each inner frame's crc is also checked
  independently.

### 2.3 Encode: single pass, backfilled `len`, depth and a running byte budget

Encode appends each frame in one pass. The `len` field is backfilled after the body is written. Every
recursive call receives a **budget**: the largest `len` (type+payload) its frame may have. The budget is
checked **before** writing, and for a Batch child **before recursing**. Nothing is measured after the
fact.

**Error prefix rule.** Internal helpers (`appendFrame`, `appendLeaf`, `appendBatch`) return errors with
**no** `walcodec:` prefix. A Batch wraps a child failure as `batch record %d: %w`. `Encode` is the only
place that adds the prefix: it wraps every helper error, exactly once, as `walcodec: record %d: %w`. So
every Encode error starts with `walcodec: ` exactly once, whatever the depth or the leaf that failed. No
error can lack the prefix and none can carry it twice. §5 tests this.

```go
func Encode(recs []Record) ([]byte, error) {
    out := []byte{version2}
    for i, r := range recs {
        var err error
        if out, err = appendFrame(out, r, 0, maxFrameLen); err != nil {
            return nil, fmt.Errorf("walcodec: record %d: %w", i, err) // the single prefix site
        }
    }
    return out, nil
}

// appendFrame appends r's full frame to out. depth is the depth of the stream
// holding r. limit is the largest len (type+payload) the frame may have:
// maxFrameLen at the top level and less inside a batch.
// On error it returns (nil, err); callers never touch out after an error.
func appendFrame(out []byte, r Record, depth, limit int) ([]byte, error)
    // switch on concrete type:
    //   Put / non-nil *Put       -> appendLeaf(out, kindPut, key, val, true, limit)
    //   Delete / non-nil *Delete -> appendLeaf(out, kindDelete, key, nil, false, limit)
    //   Batch / non-nil *Batch   -> appendBatch(out, x.Records, depth, limit)
    //   default (nil, nil pointer, foreign type) -> return nil, fmt.Errorf("unsupported record type %T", r)

func appendLeaf(out []byte, kind uint8, key string, val []byte, hasValue bool, limit int) ([]byte, error)
    // size := frameBodySize(key, val, hasValue)   // uint64, as in v1
    // size > uint64(limit) -> return nil, fmt.Errorf("encoded size %d exceeds frame cap (%d bytes available)", size, limit)
    //   This is checked before any byte is appended, so a huge Value is never copied.
    // Then write exactly as v1 does: len=uint32(size), kind, key, [value], crc.

func appendBatch(out []byte, recs []Record, depth, limit int) ([]byte, error)
    // 1. depth+1 > maxDepth -> return nil, errors.New("batch nesting exceeds depth 8")
    //    (the same predicate Decode applies in batch step 1)
    // 2. used := minRecordBody             // type + count = 5
    //    used > limit -> return nil, errors.New("batch exceeds frame cap")
    // 3. uint64(len(recs)) > uint64(limit-used)/minRecordFrame -> return nil, errors.New("batch exceeds frame cap")
    //    The children cannot fit, so this fails fast before any work. It also makes uint32(len(recs)) safe.
    // 4. start := len(out); out = append(out, 0,0,0,0, kindBatch); out = AppendUint32(out, uint32(len(recs)))
    // 5. for j, c := range recs {
    //        childLimit := limit - used - frameOverhead   // the largest len the child's frame may have
    //        if childLimit < minRecordBody -> return nil, fmt.Errorf("batch record %d: batch exceeds frame cap", j)
    //          No record of any kind, including an empty Batch, can fit in less than 5 bytes of len.
    //          So this fails before recursing and never rejects an encodable input.
    //        before := len(out)
    //        out, err = appendFrame(out, c, depth+1, childLimit)
    //        if err != nil -> return nil, fmt.Errorf("batch record %d: %w", j, err)
    //        used += len(out) - before         // the child's FULL frame: len + type + payload + crc, ≥ 13
    //    }
    // 6. // invariant: used == len(out)-start-lenSize ≤ limit ≤ maxFrameLen
    //    PutUint32(out[start:], uint32(used)); out = AppendUint32(out, crc32(out[start+lenSize:]))
```

The v1 `"walcodec: encoded stream too large"` (total > MaxInt) check goes away along with the sizing
pass. `append` cannot produce a slice longer than memory allows, and v1 never placed a bound on the
top-level stream either.

#### Cap arithmetic: what the outer `len` counts

A Batch payload is `n(4) frame_1 … frame_n`, and each inner frame is a **full**
`[len][type][payload][crc]` frame. The inner crcs are therefore part of the outer payload and are
counted by the outer `len`. Only the outer frame's own trailing crc lies outside it. The public helper
`mkFrame2(3, batchPL2(1, cur))`, where `cur` already ends in its crc, confirms this.

- Outer `len` = `1 + 4 + Σ(4 + len_i + 4)` = `used` after step 5. This is exactly
  `len(out)-start-lenSize` before the outer crc is appended.
- The constraint enforced is `used ≤ limit ≤ 16 MiB`, which is the decoder's own `n ≤ maxFrameLen` check
  on that frame.

**Worked example.** `Batch{Records: []Record{Delete{}}}`.
- Inner frame: `len` = 5 (type + `klen`), so the frame is 13 bytes on the wire.
- Outer `len` = 1 + 4 + 13 = 18. The outer frame is 26 bytes.

#### Error returns

Every exit from `appendFrame`, `appendLeaf` and `appendBatch` on failure is `return nil, err`, including
the depth, count, budget, child-guard and child failures. `Encode` returns `nil, err` whenever any of them
fails. No caller reads `out` after an error, so a partly written buffer can never escape. Example message:
`walcodec: record 2: batch record 0: batch record 3: unsupported record type *foo.T`.

#### Bounded work against hostile (aliased or cyclic) input

A `*Batch`, or a `Records` slice shared across nodes, can make the record graph cyclic or
exponentially aliased, and leaves can be empty Batches. The bound is proved by counting bytes that each
visit **exclusively** writes.

1. **Bounded output per top-level frame.** A child is entered only with `childLimit` = remaining room
   − 8. Its full frame is at most `childLimit + 8`, so `used ≤ limit` holds after every child. Every
   byte is appended only after the check that covers it. So one top-level frame writes at most
   `maxFrameLen + frameOverhead` bytes, **on success and on failure**.
2. **Each successful visit writes ≥ 13 bytes of its own.** Every successfully appended frame writes its
   own `len` (4), `type` (1), `crc` (4) and its first count or length field (4: Batch `n`, Delete or Put
   `klen`). That is 13 bytes no other visit writes.
   - **This includes an empty Batch.** It is 4 + 1 + 4 + 4 = 13 bytes, and step 5 adds those 13 bytes
     to the parent's `used`, because `len(out) - before` measures the child's full frame. An empty child
     does **not** add 0.
3. **Failed visits are few.** A visit that fails aborts the whole Encode at once. Along the abort path at
   most one visit per depth level is in progress, so at most 9 visits are unfinished.
4. **Result.** Per top-level frame, record visits ≤ `(maxFrameLen + 8) / 13 + 9`, about 1.29 M, with or
   without aliasing and whatever the leaf kind. The `childLimit < minRecordBody` guard also stops the
   descent before recursing once fewer than 13 bytes remain, so a nearly exhausted budget costs no
   recursive call.
5. **Cycles.** The depth check (step 1) runs before anything is written. The first descent errors at the
   9th nesting level, so a cycle costs O(depth).
6. **CRC cost.** Each byte is checksummed once per enclosing frame, at most 9 times. The total is
   therefore ≤ 9 × 16 MiB of crc work per top-level frame.
7. **Inherent linear cost.** A top-level `recs` that repeats one record N times produces N × its size of
   output. That is linear in N and identical to v1.

#### Why one pass

- The written `len` is measured from bytes actually written, so a size calculation and the writer cannot
  disagree.
- v1's ignored error from the second pass disappears.
- Recursive sizing does not have to be duplicated.
- **Trade-off:** v1 preallocated the exact total; v2 uses amortised `append`.

#### No casts before checks

- `uint32(size)` comes after `size ≤ limit ≤ maxFrameLen`.
- `uint32(len(recs))` comes after the step-3 bound.
- `uint32(used)` comes after the invariant `used ≤ limit`.

Each cast has a one-line comment naming its guard.

### 2.4 `doc.go`

Rewrite the format section to state:

- `version` is 1 or 2, and Encode writes 2.
- Type 3 is a Batch, with payload `n(4) frame{n}`.
  - The inner frames are full frames, crcs included, and are counted in the outer `len`.
  - They must consume the payload exactly.
- The 16 MiB cap applies to every frame at every depth. An inner frame may not extend past its
  enclosing payload.
- Depth convention:
  - The top-level stream is depth 0, and a Batch's sub-stream is one level deeper.
  - A depth greater than 8 is rejected by both Decode and Encode.
- A version-1 stream may contain only types 1 and 2.
- A decoded Batch has non-nil `Records`.
- Decode errors name the absolute offset and stream depth. Encode errors start with `walcodec: ` and do
  not wrap `ErrCorrupt`.

## 3. How each v2 rule is satisfied

| Amendment rule | Mechanism |
|---|---|
| `Batch{Records}` exists, `Kind()==3` | §2.1 |
| Batch payload = `n uint32` + n full inner frames | `appendBatch` writes `n`, then each child through the same `appendFrame`. `decoder.batch` reads `n`, then calls the same `decoder.frame` n times |
| Batch may contain Put/Delete/Batch (recursion) | `appendFrame` and `decoder.payload` handle all three kinds at every depth |
| Same 16 MiB `len` cap at every depth | Decode: `decoder.frame` step 2 runs for every frame, inner or outer. Encode: every frame is written under `limit ≤ maxFrameLen`, and the outer `len` counts inner crcs exactly as the decoder measures it (§2.3, cap arithmetic) |
| Inner `len` past enclosing payload → `ErrCorrupt` (truncated), never a read into the next outer frame | `decoder.frame` step 4, which is named as this case. It receives only the Batch payload's unread tail and checks `n+crc` against **that** region's length |
| Inner frames consume the payload exactly; fewer or more than n, or trailing bytes → `ErrCorrupt` | Fewer: frame step 1 or 4 before frame n. More, or trailing bytes: batch step 7. An infeasible n is rejected early by `n > len/13` |
| Depth cap 8; top level = depth 0; sub-stream one deeper; depth > 8 corrupt | Decode: `batch` rejects `depth+1 > 8`, where `depth` is the Batch frame's stream depth. Encode: `appendBatch` rejects `depth+1 > 8`, the identical predicate. Both accept 8 nested Batches and reject 9 |
| Encoder never emits what it would reject | Same depth predicate, and the budget enforces the decoder's exact `len ≤ cap` rule on every frame (§2.3) |
| Encode writes `version = 2` | `out := []byte{version2}` always, including for Put/Delete-only and empty input |
| Decode accepts v1 (Put/Delete only) and v2 (all types) | The version switch. `allowBatch` is false for v1, so a type-3 frame in a v1 stream is corrupt |
| Any other version (0, 3, …, 255) → `ErrCorrupt`, before any frame | The version check comes right after the empty check, before any frame is touched |
| v1 regression via fixed v1 bytes | The frame and payload code for Put/Delete is unchanged, so the v1 fixture decodes to the same records. The v1 crc, truncation and oversize paths go through the same `decoder.frame` checks |
| Surface frozen except `Batch` | §1. Verified in §5 with an exported-identifier listing |

### Public v2 cases walked through

1. **Batch round-trip.** `[2][type-3 frame: n=2, Put frame, Delete frame][crc]` decodes to
   `Batch{[Put, Delete]}`.
2. **Nested round-trip.** The outer Batch frame is at depth 0 and its sub-stream at depth 1. The inner
   Batch frame is at depth 1 and its sub-stream at depth 2, which is ≤ 8.
3. **v1 fixture.** Version 1 is accepted and the frames are parsed as in v1, giving `Put{"alpha","one"}`
   then `Delete{"beta"}`.
4. **Mixed top level.** Frames come back in order. `Put{Value:nil}` decodes to an empty slice, and
   `recEq2` compares with `string()`, so they match.
5. **Truncated Batch** (`b[:len-2]`). The outer frame's `n+crc` exceeds the remaining top-level region,
   so frame step 4 reports it at offset 1, depth 0.
6. **version = 3.** Rejected by the version check.
7. **9 nested Batches.** The k-th Batch frame from the outside sits in the stream at depth k−1. The 9th
   sits at depth 8, so `batch` sees `depth+1 = 9 > 8` and rejects it. All crcs are valid because the
   test builds the stream by hand.
8. **Fixture behaviours.** The round-trip works as in case 3. A crc flip on the last byte gives a crc
   mismatch on the Delete frame, and `[:len-3]` truncates the Delete frame.

The v1 public suite still passes. None of its cases asserts the written version byte. Its hand-built
cases 3 and 5 use version 1, which v2 accepts, so they take the same paths as in v1.

## 4. Behaviour decisions not dictated by the amendment

- **`*Batch` is accepted by Encode** (non-nil), for symmetry with `*Put` and `*Delete`. Decode always
  returns `Batch` values, never pointers.
- **Decoded `Batch.Records` is non-nil** (`[]Record{}`) when n=0. `Batch{}` and `Batch{Records: nil}`
  encode as n=0 with frame `len` 5.
- **Encode errors do not wrap `ErrCorrupt`.** This covers the depth, cap/budget and unsupported-type
  errors, following the v1 convention.
- **A version-1 stream containing type 3** is `ErrCorrupt`.
- **An inner frame with `len > 16 MiB`** is reported as a cap error first. It is also necessarily a
  truncation; either way the result is `ErrCorrupt`.
- **Error message text changes.** v1 messages now gain a `(depth 0)` suffix, and payload messages gain an
  offset. No test or contract depends on the exact text, only on `errors.Is(err, ErrCorrupt)`.

## 5. Tests (`walcodec_internal_test.go`, new) and verification

**Golden bytes**
- `Encode([Put{"alpha","one"}, Delete{"beta"}])` equals `v1Fixture()` with byte 0 set to 2.
- `Encode([Batch{[Delete{}]}])` equals `[2] + mkFrame2(3, batchPL2(1, mkFrame2(2, be2(0))))`, with outer
  `len` = 18 (§2.3 worked example).
- A two-level nested golden, built from `mkFrame2` and `batchPL2`.

**Versions**
- Each byte 0–255 as the version of an otherwise valid stream: only 1 and 2 are accepted.
- `[1]` and `[2]` each decode to an empty slice.
- A Batch frame inside a version-1 stream is rejected.

**Depth**
- Exactly 8 nested Batches:
  - Encode succeeds and Decode round-trips it.
  - A hand-built 8-deep stream decodes.
- 9 nested Batches:
  - Encode returns a nil slice and a non-`ErrCorrupt` error.
  - The hand-built 9-deep stream is `ErrCorrupt`, and its message names `depth 8` and the absolute
    offset of the 9th Batch frame.

**Batch framing.** Each of these is `ErrCorrupt` with a nil slice:
- n larger than the frames present.
- n smaller than the frames present (trailing frames).
- Trailing garbage after n frames.
- A payload shorter than 4 bytes.
- An inner `len` exactly one byte past the payload, while a sibling frame follows in the outer stream.
  This proves there is no read into the next outer frame. The message must contain "truncated frame",
  the inner frame's absolute offset and `depth 1`.
- An inner `len` > 16 MiB.
- An inner crc flip, with the outer crc recomputed so that it is valid.
- An inner unknown type.
- An inner `len == 0`.
- A malformed inner Put.

**Offsets and depth in messages (r2 minor)**
- A table test covers one corruption of each kind at depth 2: truncated header, cap, empty frame,
  truncation, crc, malformed payload, unknown type, batch count, trailing bytes and depth exceeded.
  Each was built with every enclosing crc made valid.
- For each case the test computes the expected absolute offset from the construction, then asserts that
  the message contains both `at offset <off>` and `(depth <d>)`.

**Count bound**
- `n = 0xFFFFFFFF` with a small payload is rejected, and allocations are bounded (`testing.AllocsPerRun`).
- A batch of k `Delete{""}` frames (13 bytes each) with `n = k` exactly fills the bound
  `n == len(p)/13`. It must decode.
- The same payload with `n = k+1` is corrupt.

**Truncation**
- Every proper prefix of a nested-batch stream is `ErrCorrupt`.

**Encode error contract**
- A nil slice is returned for each failure kind:
  - an unsupported type at the top level
  - an unsupported type at depth 3
  - a nil `*Batch`
  - depth 9
  - a leaf over budget
  - a batch over budget
  - the step-3 count bound
  - the child guard (`childLimit < minRecordBody`)
- For every one of these, `strings.HasPrefix(err.Error(), "walcodec: ")` holds,
  `strings.Count(err.Error(), "walcodec:") == 1`, and `!errors.Is(err, ErrCorrupt)` (r2 nit 1).

**Cap boundary.** Skipped under `-short`.
- The value length is **computed in the test from the layout**, never written as a bare literal. For a
  Batch holding one Put with key `k`:
  - inner Put `len` = 1 (type) + 4 (`klen`) + len(k) + 4 (`vlen`) + vlen
  - inner full frame = 4 (len) + inner `len` + 4 (inner crc; inner crcs **are** inside the outer `len`)
  - outer `len` = 1 (type) + 4 (`n`) + inner full frame = 22 + len(k) + vlen
  - so `vlen = (16<<20) − 22 − len(k)`, which for `k = ""` is `16 MiB − 22`.

  The test writes this as a helper `capVlen(keyLen int) int` with the derivation in its comment, and runs
  it with `k = ""` and `k = "key"`.
- For each, the vlen case:
  - encodes and decodes;
  - **asserts that the outer `len` field reads exactly `16<<20`**. A wrong constant, such as one that
    drops the inner crc or ignores the key, fails this assertion even if Encode succeeds.
- The `vlen + 1` case fails Encode with a nil slice.
- A hand-built frame with outer `len = 16 MiB + 1` fails Decode.
- Together these pin Encode's limit to the decoder's.

**Bounded work**
- A cycle through `*Batch`, and a cycle through a shared `Records` slice, each return an error quickly.
- **Empty-node fan-out (r2 major).** At each level, one shared `Records` slice of 16 aliased children.
  There are 8 Batch levels, the most the depth cap allows, and the level-8 nodes are **empty
  `Batch{}`**. That is (16^8 − 1)/15 ≈ 2.9 × 10^8 logical nodes, or ≈ 3.7 GB if fully encoded:
  - Encode returns a nil slice and a cap error.
  - It finishes within 2 s.
  - Total allocation, measured as a `runtime.MemStats.TotalAlloc` delta, stays under 64 MiB.

  Exclusive-byte accounting (§2.3) caps the work at about 1.29 M visits. A design whose empty children
  added nothing to `used` would instead walk billions of nodes and time out.
- **Empty-node success control.** The same shape at fan-out 5 is (5^8 − 1)/4 = 97,656 nodes × 13 B
  ≈ 1.27 MB, under the cap. It encodes, decodes, and `len(out)` equals the predicted `1 + 13 × nodes`. This confirms that
  the guard does not reject encodable input and that every empty node costs exactly 13 bytes.
- **Large-leaf fan-out.** The fan-out-16, depth-8 aliased tree with 64 KiB leaf `Value`s also errors
  within the same time and allocation bounds.

**Aliasing and empties**
- Mutating the input after Decode leaves the decoded nested values unchanged.
- `Batch{}` and `Batch{Records:nil}` round-trip, and the decoded `Records` is non-nil.
- `*Batch` is accepted. A nil element inside a batch is an error.

**Fuzz**
- `FuzzDecode`:
  - It never panics, and every error wraps `ErrCorrupt`.
  - On success with `b[0]==2`, `Encode(recs)` equals `b` (canonical form).
  - On success with `b[0]==1`, `Encode(recs)[1:]` equals `b[1:]`.
  - Seeds: the v1 fixture, the goldens, and the 9-deep stream.
- `FuzzRoundTrip`: random record trees up to depth 8 round-trip.

**Verification** (all run from `worktree/`):
- `gofmt -l .` produces no output.
- `go vet ./...`
- `go build ./...`
- `go test ./...`
- `go test -race ./...`
- `go test -short ./...`
- A short `-fuzz` run of each fuzz target.
- `cmp` of both public test files against `instrument/`.
- `go doc -all` to confirm the only new exported names are `Batch` and `Batch.Kind`.

## 6. Residual v1 review notes (c1_1_adv), resolved as a side effect

- **Encode walks the records twice and drops the second error:** removed by the single pass (§2.3).
- **No capacity hint:** a Batch gets a bounded exact hint (§2.2). The top-level stream has no count, so it
  keeps `append`.
- **uint32 casts rely on earlier checks:** each cast has an adjacent guard and a comment (§2.3).
- **Key no-alias relies on `string([]byte)` copying:** covered by the new mutation test.
