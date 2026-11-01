# Plan: walcodec v2 (nested `Batch` records)

Status: phase 2, round 2. This revision answers every item in `plan/plan_findings.md`; the
point-by-point response is in `plan/plan_rebuttal.md`. Each section changed by a finding is tagged
**[F#]**, where # is the finding's position in the findings file (1 = the major finding, 2 to 6 =
the minors in order, 7 to 10 = the nits in order).

## 0. Starting point

- **The worktree has no implementation file.** `worktree/` holds only `go.mod` (`go 1.25`, so the
  built-in `min` is available), `walcodec_public_test.go` (v1) and `walcodec_v2_public_test.go`
  (v2, identical to `instrument/`). The converged v1 code is at `snapshot-v1/walcodec.go`.
  **Implementation step 1:** copy it unchanged to `worktree/walcodec.go`, then edit it there.
  Without that step the package does not compile.
- v1's shape is kept. Encode validates and sizes everything first, then appends frames and
  back-patches `len`. Decode checks bounds before it slices. Decode errors get `ErrCorrupt` from a
  single place. v2 makes frame sizing, appending and parsing **recursive with a depth parameter**,
  and parsing always runs over a **bounded sub-slice**.
- Files: `worktree/walcodec.go` is edited. `worktree/walcodec_v2_internal_test.go` is added (§5).
  `go.mod` and both public test files are not touched.

## 1. What stays the same

- These keep the same names and signatures: `Record`, `Put`, `Delete`, `Encode`, `Decode`,
  `ErrCorrupt`. The `ErrCorrupt` text is unchanged. `Put{}.Kind()==1` and `Delete{}.Kind()==2`.
- Frame shape is `len(4) type(1) payload(len-1) crc(4)`. `len` counts type+payload. The CRC is
  CRC32-IEEE over type+payload. All integers are big-endian.
- `len` has a 16 MiB inclusive cap, checked before anything is sliced or allocated. A zero-length
  frame is rejected. Both rules now apply **at every depth**.
- Put/Delete payload layout and decode rules: trailing bytes are rejected, nil and empty Value are
  equal, a decoded Value is a non-nil copy that does not alias the input, and the key is copied into
  a new string.
- Empty `recs` gives a one-byte stream. `Decode(nil)` and `Decode([]byte{})` return `ErrCorrupt`. A
  version-only stream decodes to an empty, non-nil slice.
- Encode never returns partial output. It rejects pointer records (`*Put`, `*Delete`, and now
  `*Batch`), nil records and foreign `Record` types, with descriptive errors that do **not** wrap
  `ErrCorrupt`. It also rejects oversize frames the same way. The one exception is the depth cap,
  which wraps `ErrCorrupt` (§2.2, **[F1]**).
- These are kept as they are: `corrupt()`, `readField`, `appendField`, the Put/Delete arms of
  `decodePayload` and `appendFrame`, and the `math.MaxInt` overflow guard in `Encode`.

## 2. What changes

### 2.1 New type and constants
```go
const (
    kindBatch uint8 = 3
    version1        = 1  // Put/Delete only; still accepted by Decode
    version2        = 2  // written by Encode; all three kinds
    maxDepth        = 8  // deepest legal sub-stream depth; the top-level stream is depth 0
    batchCountSize  = 4  // n uint32 at the start of a Batch payload
    minFrameSize    = lenSize + 1 + u32Size + crcSize // 13: smallest legal frame (Delete{""} or empty Batch)
)

// Batch groups records; its payload is a nested framed sub-stream. Kind 3.
// Pass Batch by value. A nil and an empty Records are equivalent.
type Batch struct{ Records []Record }

// Kind returns 3, the wire type tag of Batch.
func (Batch) Kind() uint8 { return kindBatch }

// kindAllowed reports whether a frame of type kind may appear in a stream of
// version ver. It is the only place the version-to-kinds mapping lives.   [F9]
func kindAllowed(ver byte, kind uint8) bool {
    switch kind {
    case kindPut, kindDelete:
        return true
    case kindBatch:
        return ver >= version2
    }
    return false
}
```
The doc comments on the package, `Record`, `Encode`, and `Decode` are updated for v2. They describe
the version rule, the Batch payload, the depth convention, which Encode error wraps `ErrCorrupt`
(only the depth cap), and the fact that a decoded `Batch` has a non-nil `Records`.

### 2.2 Encode (writes version 2)

**Error type, built lazily [F6] and wrapping ErrCorrupt only for depth [F1]:**
```go
// encodeError is returned by Encode. path holds record indices innermost-first;
// each recursion level appends its own index only while unwinding on failure,
// so the success path never allocates or formats a path.
type encodeError struct {
    path     []int
    detail   string
    tooDeep  bool // depth-cap violation; wraps ErrCorrupt
}
func (e *encodeError) Error() string // "walcodec: record 2.0.1: <detail>" (path reversed, dot-joined)
func (e *encodeError) Unwrap() error // ErrCorrupt if tooDeep, else nil
```
- `frameSize(r Record, depth int) (int, *encodeError)` is recursive. `depth` is the depth of the
  stream that contains `r`; top-level records are at depth 0. The return type is the concrete
  pointer, not `error`, which avoids typed-nil interface mistakes. A non-nil `*encodeError` is
  turned into an `error` only at `return nil, e` inside an `if e != nil` block. The Put, Delete,
  nil, and default arms are the v1 checks, with the same detail texts.
- The Batch arm of `frameSize`:
  1. If `depth+1 > maxDepth`, return `&encodeError{detail: "batch nesting exceeds depth 8",
     tooDeep: true}`. This check runs **before** the code looks at `x.Records`. That bounds the
     recursion at 9 levels and also ends a self-referencing input
     (`rs := make([]Record, 1); rs[0] = Batch{rs}`).
  2. Start with `payloadLen := batchCountSize`. For each inner record `j`, call
     `frameSize(inner, depth+1)`. On error, run `e.path = append(e.path, j)` and return `e`.
     Otherwise add `lenSize + innerLen + crcSize` to `payloadLen`. **After each addition**, if
     `1+payloadLen > maxFrameLen`, return the too-large error at once. Every inner frame length is
     at most 16 MiB, so the running sum stays under about 32 MiB and cannot overflow `int`, even on
     32-bit. A huge `Records` slice stops after about 1.3M elements.
  3. Return `1+payloadLen`. Step 2 means `len(x.Records)` is far below 2^32, so `uint32(n)` is
     safe.
- `Encode` loops over the top-level records. On error it runs `e.path = append(e.path, i)` and
  `return nil, e`, and writes no output. Then it writes `version2` and appends the frames. The
  `math.MaxInt` guard is unchanged.
- **Encode doc comment (updated):** "Encode returns nil and an error if an element (at any depth) is
  nil, is a pointer or foreign type, would produce a frame whose len exceeds 16 MiB, or nests
  Batches deeper than 8 levels. Only the depth-cap error wraps ErrCorrupt, because the format
  defines an over-deep nest as corrupt and Encode will not emit a stream Decode would reject. The
  other errors are descriptive only."
- `appendFrame(buf, r)` gets a Batch arm. It writes the tag `kindBatch`, then
  `AppendUint32(uint32(len(x.Records)))`, then calls `appendFrame` on each inner record. The
  existing pattern (placeholder, then back-patch from `buf[start+lenSize:]`) already handles
  nesting: each call patches only its own `start`, and inner frames are complete before the outer
  `len` and CRC are computed. Depth is not checked again here, because `frameSize` has already
  validated the whole tree. The `default: panic` stays as an internal guard.
- **The encoder never emits a stream that it would reject itself.** It uses the same `maxDepth`,
  the same depth convention, and the same cap on every frame.

### 2.3 Decode (accepts version 1 and version 2)

**Error layering rule, written into a code comment [F2]:** `readFrame`, `decodeFrame`,
`decodeStream`, `decodeCounted` and `decodePayload` return **plain detail errors at every depth**.
None of them calls `corrupt()` or refers to `ErrCorrupt`. Each loop adds context to an inner error
with `fmt.Errorf("frame %d at offset %d: %w", i, off, err)`. Offsets are relative to the enclosing
region. **Only `Decode` calls `corrupt()`, exactly once**, so the `walcodec: corrupt stream:`
prefix appears once. An example message:
`walcodec: corrupt stream: frame 0 at offset 0: batch: frame 1 at offset 13: crc mismatch`.

```go
// readFrame parses one frame from region b at off. b is the ENCLOSING region only
// (the stream after the version byte, or one Batch payload after its count).
func readFrame(b []byte, off int) (kind uint8, payload []byte, next int, err error)
```
`readFrame` holds the v1 checks, moved out of `Decode`, in the same order and in the same form
**[F10]**:
```go
rem := len(b) - off
if rem < lenSize { /* truncated frame header */ }
n := binary.BigEndian.Uint32(b[off:])
if n > maxFrameLen { /* oversize: nothing sliced or allocated */ }
if n == 0 { /* zero-length frame */ }
if uint64(n)+lenSize+crcSize > uint64(rem) { /* truncated frame */ }   // verbatim v1 form
end := off + lenSize + int(n)
// crc check over b[off+lenSize:end] against b[end:]
```

```go
// decodeFrame reads one frame and decodes it. depth is the depth of the stream holding it.
func decodeFrame(b []byte, off int, ver byte, depth int) (Record, int, error)
```
It calls `readFrame`. Then, if `!kindAllowed(ver, kind)`, it returns
`"record type %d not allowed in version %d"`. This covers tag 3 in a v1 stream and also unknown
tags such as 99. Otherwise it calls `decodePayload(kind, payload[1:], ver, depth)`.
`decodePayload` keeps its `default: unknown record type` arm as a defensive fallback.

**Two entry points, no sentinel mode [F7]:**
```go
// decodeStream decodes the top-level stream: frames until b is exhausted. Depth 0.
func decodeStream(b []byte, ver byte) ([]Record, error)

// decodeCounted decodes exactly n frames that must consume b exactly. b is one
// Batch payload after its count; depth is that sub-stream's depth.
func decodeCounted(b []byte, ver byte, depth int, n uint32) ([]Record, error)
```
`decodeStream` is the v1 loop: `out := make([]Record, 0)`, then `decodeFrame(b, off, ver, 0)` until
`off == len(b)`.

`decodeCounted`, written without mixed-type comparisons **[F8]**:
```go
out := make([]Record, 0, int(min(uint64(n), uint64(len(b)/minFrameSize))))
off := 0
for i := uint32(0); i < n; i++ {
    if off == len(b) {
        return nil, fmt.Errorf("batch declares %d records, found %d", n, i)
    }
    rec, next, err := decodeFrame(b, off, ver, depth)
    if err != nil {
        return nil, fmt.Errorf("frame %d at offset %d: %w", i, off, err)
    }
    out = append(out, rec)
    off = next
}
if off != len(b) {
    return nil, fmt.Errorf("%d trailing bytes after %d batch records", len(b)-off, n)
}
return out, nil
```
- The capacity is capped by how many frames could actually fit, so an attacker's
  `n = 0xFFFFFFFF` causes no large allocation. `out` is never nil, so an empty Batch decodes to
  `Records: []Record{}`.
- `b` is the Batch payload **sub-slice**. An inner `len` that points past the enclosing payload
  fails `readFrame`'s truncation check. The reader never sees sibling or outer bytes, and that is
  enforced by slice bounds, not only by arithmetic.
- The trailing-bytes check covers both "more frames than n" and plain garbage.

`decodePayload(kind, p, ver, depth)` gets a Batch arm:
1. If `depth+1 > maxDepth`, return `"batch nesting exceeds depth 8"`. This check runs before the
   count or the inner frames are read.
2. If `len(p) < batchCountSize`, return `"truncated batch count"`.
3. Call `inner, err := decodeCounted(p[batchCountSize:], ver, depth+1, binary.BigEndian.Uint32(p))`.
   On error, return `fmt.Errorf("batch: %w", err)`.
4. Return `Batch{Records: inner}`.

`Decode`:
1. If `len(b) == 0`, return `corrupt("missing version byte")`.
2. If `b[0]` is neither `version1` nor `version2`, return `corrupt("unknown version %d", b[0])`.
   This happens **before any frame** is read.
3. Call `recs, err := decodeStream(b[1:], b[0])`. If `err != nil`, return `nil, corrupt("%v", err)`.

- **Cost bound.** Recursion is at most 9 levels deep, and each byte is covered by at most 9 CRCs.
  Decode time is O(9·len(b)), and stack use is bounded.

## 3. How each v2 rule is satisfied

| Amendment rule | Mechanism |
|---|---|
| `Batch{Records []Record}`, Kind 3, the only surface addition | §2.1. `encodeError`, `kindAllowed` and the helpers are unexported |
| Payload = `n(4)` + n full frames | `appendFrame` Batch arm; `decodePayload` Batch arm + `decodeCounted` |
| Recursion (Put/Delete/Batch inside Batch) | `frameSize`, `appendFrame` and `decodeFrame`→`decodePayload` all recurse through one switch |
| Same 16 MiB cap at every depth | `readFrame` is shared by all depths; the encoder checks every frame's len and the running sum |
| Inner len past enclosing payload → ErrCorrupt, never reads outer | `readFrame` runs on the payload **sub-slice**; the truncation check fails |
| n frames consume the payload exactly | `decodeCounted`: region exhausted before n → error; leftover after n → error; `len(p)<4` → error |
| Depth: top = 0, sub-stream = depth+1, >8 → ErrCorrupt | `depth+1 > maxDepth` in the decoder's Batch arm; 8 wrappers accepted, 9 rejected |
| The encoder enforces the same cap | same check and constant in `frameSize`; the error wraps `ErrCorrupt` [F1]; no output |
| Encode writes version 2 | `buf = append(buf, version2)` |
| Decode accepts 1 and 2; other versions → ErrCorrupt before any frame | version check in `Decode` before `decodeStream` |
| Version 1 = Put/Delete only | `kindAllowed(version1, kindBatch) == false` [F9] |
| v1 regression via a fixed v1 fixture | the v1 path is v1's frame logic unchanged; the fixture decodes to `Put{alpha,one}`, `Delete{beta}` (checked against the snapshot-v1 decoder) |
| Every Decode error is ErrCorrupt | a single `corrupt()` call in `Decode` [F2] |
| v1 surface frozen | no signature changes; the v1 public test file passes unchanged |

## 4. Public test walk-through (v1 and v2 files run together)

- v1 1, 8, 9: round-trips do not depend on the version byte. Encode now writes 2, which the
  amendment allows.
- v1 2, 4: a CRC flip or truncation of a Batch-free v2 stream hits the same `readFrame` checks as
  before.
- v1 3: a hand-built version-1 stream with tag 99. `kindAllowed` returns false, so the result is
  `ErrCorrupt`.
- v1 5: `len` is greater than 16 MiB, so the frame is rejected before any slicing.
- v1 6, 7: `Encode(nil)` returns `[2]`, which is one byte, and it decodes to an empty slice. Nil
  or empty input returns `ErrCorrupt`.
- v2 1, 2, 4: these round-trip through the recursive encoder and decoder.
- v2 3, 8: the fixture is version 1 and decodes to `alpha/one`, `beta`. A flip of the last CRC
  byte fails the CRC check. Cutting 3 bytes leaves the last frame truncated.
- v2 5: cutting 2 bytes makes the outer Batch frame fail the top-level truncation check.
- v2 6: `b[0]=3` is rejected by the version check.
- v2 7: with 9 wrappers, the 9th Batch sits in the depth-8 sub-stream, so its own sub-stream
  would be at depth 9. That is rejected. Every CRC is valid, so only the depth cap fails.

## 5. Extra tests (`worktree/walcodec_v2_internal_test.go`, package `walcodec`)

The tests reuse the public v2 helpers (`be2`, `mkFrame2`, `putPL2`, `batchPL2`, `recsEq2`) from the
same package. Any new helper gets a unique `x`-suffixed name so it cannot collide with them.

1. **Depth boundary.** 8 hand-built wrappers decode, and 9 return `ErrCorrupt`. `Encode` of 8
   nested Batches round-trips. `Encode` of 9 returns nil bytes and an error for which
   `errors.Is(err, ErrCorrupt)` is **true** **[F1]**, and whose message names the path
   (`record 0.0.0.0.0.0.0.0.0`: nine indices, since the 9th Batch sits under eight others). An empty Batch as the 9th level is also rejected by both sides.
2. **Contract both ways [F1].** The oversize, nil-element, foreign-type and `*Batch` errors from
   Encode do **not** satisfy `errors.Is(err, ErrCorrupt)`.
3. **Self-referencing Records.** `Encode` returns the depth error and does not hang or overflow the
   stack.
4. A Batch frame (tag 3) in a **version-1** stream returns `ErrCorrupt`, even though its CRC and
   structure are valid.
5. Versions 0, 3 and 255 are each rejected, with a valid frame following.
6. **Inner len past the payload.** (a) An in-bounds case: the inner `len` exceeds the payload, and
   the outer CRC is recomputed, so only the inner bound is wrong. (b) **Read-through case [F3]:**
   the stream is built so that the over-long inner frame is *fully valid* if read past the Batch
   payload. The stream is `0x02 | be(18) | Bbody | outerCRC | filler(5) | innerCRC`, where
   `Bbody = 0x03 | n=1 | innerLen=9+V | 0x01 | klen=0 | vlen=V` and `V = 4+5`. Read through, the
   inner Put's value is `outerCRC|filler`, and `innerCRC` is the correct CRC32 over
   `0x01|klen|vlen|outerCRC|filler`. So a decoder that reads into the outer bytes would get a
   well-formed Put. The test asserts `errors.Is(err, ErrCorrupt)` **and**
   `strings.Contains(err.Error(), "truncated")`, so only the sub-slice bound can be what rejects
   the stream.
7. **Count/consumption.** Each of these returns `ErrCorrupt`, and every case has a valid outer CRC:
   `n` greater than the frames present, `n` less than the frames present, a correct `n` with
   trailing garbage, and a payload shorter than 4 bytes.
8. **Inner frame faults.** Each of these returns `ErrCorrupt`, with the outer CRC recomputed: an
   inner CRC flip, an inner zero-length frame, an inner unknown tag, and an inner
   `len > 16 MiB`.
9. **Allocation bound, measured in bytes [F4].** `testing.Benchmark(func(b *testing.B){ for range
   b.N { Decode(x) } }).AllocedBytesPerOp()` must be under 4 KiB in two cases. The first has
   `n = 0xFFFFFFFF` in a 4-byte payload. The second has `n = 1<<24` followed by one valid inner
   frame. Each `Decode` must also return `ErrCorrupt`. A `make([]Record, 0, n)` regression would
   allocate hundreds of MiB to tens of GiB and fail this test.
10. `Batch{}`, `Batch{Records: []Record{}}`, and a Batch holding an empty Batch all round-trip.
    Decoded `Records` is non-nil.
11. Encode rejects `*Batch`, a nil element inside a Batch, and a foreign type inside a Batch. The
    message carries the nested path, for example `record 1.0.2`.
12. **Frame size.** Encode rejects a Batch whose total frame exceeds 16 MiB even though every inner
    record is under the cap (two 9 MiB Puts). It also rejects the same thing one level deeper. A
    Batch frame of exactly 16 MiB is accepted.
13. Decoded inner Put values do not alias the input. The test mutates the input after decoding and
    checks the decoded values are unchanged.
14. **Wire pin.** The bytes `Encode` writes for a small nested input equal a byte literal built with
    `mkFrame2`/`batchPL2`.
15. **`FuzzDecode` [F5].** Seeds: `v1Fixture()`, the Encode outputs of the public cases, the 8-deep
    and 9-deep streams, and the corpora from items 4 to 9 above. Properties checked for each input
    `x`:
    - `Decode` never panics.
    - On error, the returned records are nil and `errors.Is(err, ErrCorrupt)` holds.
    - On success, `out, err := Encode(recs)` succeeds, because the decoder's caps match the
      encoder's.
    - If `x[0]==2`, `bytes.Equal(out, x)` holds. The format is canonical: `len`, the CRC, `n`, and
      the field lengths are all determined by content, and trailing bytes are rejected at every
      level.
    - If `x[0]==1`, `out[0]==2` and `bytes.Equal(out[1:], x[1:])`.
    - `Decode(out)` succeeds and `recsEq2` matches `recs`.

    Under plain `go test`, only the seeds run. `go test -fuzz=FuzzDecode` is for longer runs.

## 6. Done criteria

All of these must hold:
- `gofmt -l .` prints nothing.
- `go vet ./...` is clean.
- `go build ./...` succeeds.
- `go test ./...` passes the v1 public suite unchanged, the v2 public suite, and the internal tests
  including the fuzz seeds.
- `go test -race ./...` is clean.
- A short `go test -fuzz=FuzzDecode -fuzztime=30s` finds nothing.
