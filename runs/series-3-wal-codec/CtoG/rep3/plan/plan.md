# Plan — `walcodec` v2 amendment (nested `Batch` records)

Phase 2, revision 1. No `plan/plan_findings.md` exists for this phase, so there is no rebuttal.
Sources read: `amendment.md`, `spec-v1.md`, `instrument/walcodec_v2_public_test.go`,
`instrument/walcodec_public_test.go`, the v1 implementation in `snapshot-v1/`
(`walcodec.go`, `encode.go`, `decode.go`), and the phase-1 adversary turns (for context on v1 decisions).

## 0. Baseline / worktree state

The worktree currently holds only `go.mod`, `walcodec_public_test.go` and
`walcodec_v2_public_test.go`. The converged v1 source lives in `snapshot-v1/`. **Step 0 of
implementation:** copy `snapshot-v1/{walcodec.go,encode.go,decode.go}` into the worktree unchanged,
then confirm that `go build ./...` passes and that the v1 public suite alone passes. (The v2 suite will
not compile yet because `Batch` is undefined.) After that, apply the changes below. The v1 adversary
review mentions v1 internal tests and a `FuzzRoundTrip`, but they are not in the snapshot. New
internal tests (section 6) cover both v1 and v2 behavior.

Both public test files stay **byte-for-byte unchanged**. Helper names they own (`beU32`, `mkFrame`,
`recEq`, `recsEq`, `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `recEq2`, `recsEq2`, `v1Fixture`,
`v1FixtureRecords`) must not be redeclared in any new file.

## 1. What stays (frozen)

- Exported names and signatures stay as they are: `Record`, `Put`, `Delete`, `Encode(recs []Record) ([]byte, error)`,
  `Decode(b []byte) ([]Record, error)`, `ErrCorrupt` (same `errors.New` value and text). `Put.Kind()==1`,
  `Delete.Kind()==2`.
- The frame shape `len(4) type(1) payload(len-1) crc(4)` is unchanged: big-endian, `len` counts type+payload,
  CRC32-IEEE over type+payload, `1 <= len <= 16 MiB` (len 0 stays rejected, as in v1).
- Put/Delete payload layouts are unchanged, and parsing is still exact (trailing bytes inside a payload mean
  `ErrCorrupt`). nil and empty `Value` both decode to a non-nil, length-0 slice. Decoded keys and values never alias the input.
- Decode stays a two-pass design per (sub-)stream: a structural header scan first (no allocation sized
  by untrusted counts), then CRC check and parse. Any failure returns `nil, ErrCorrupt`, never a partial slice.
- Encode error contract: on failure it returns a `nil` slice and an error that is **never** `ErrCorrupt`
  (`errUnsupportedRecord`, `errRecordTooLarge`, `errOutputTooLarge`, plus one new sentinel below).
- The non-nil `*Put`/`*Delete` convenience on Encode stays, because it is existing v1 behavior.
- Top-level total-size guard (`addFrameSize` against `math.MaxInt`) stays.

## 2. What changes — surface and constants (`walcodec.go`)

```go
// Batch groups records into one frame (Kind 3). Its payload is a nested framed
// sub-stream: n(4) followed by n frames of the same [len][type][payload][crc] shape.
// Batches may nest up to 8 levels deep.
type Batch struct{ Records []Record }

func (Batch) Kind() uint8 { return kindBatch } // 3
```

New and changed constants:
- `version1 = 1` (still accepted by Decode), `version2 = 2` (written by Encode).
- `kindBatch = 3`.
- `maxDepth = 8`: the deepest legal sub-stream depth, where the top-level stream is depth 0.
- `countSize = 4`: the Batch `n` field.
- `minBatchBody = 1 + countSize`: an empty Batch body.

New encode-side sentinel (unexported, never `ErrCorrupt`):
`errTooDeep = errors.New("walcodec: batch nesting exceeds depth 8")`.

Doc updates:
- Package doc: describe v2 (the Batch payload grammar, the depth convention, "Encode writes version 2;
  Decode accepts 1 and 2; version 1 streams may contain only Put/Delete").
- `ErrCorrupt` doc: add the bad Batch count, trailing bytes in a Batch, excess depth, and a Batch tag in a v1 stream.
- `Encode` and `Decode` docs: version 2, Batch, and `*Batch` accepted.

## 3. Depth convention (one definition, used by both sides)

Every frame is read or written *in a stream at depth `d`*. The top-level stream is `d = 0`. A Batch
frame that appears in a stream at depth `d` owns a sub-stream at depth `d+1`. The rule is:
**if `d+1 > maxDepth`, reject**: `ErrCorrupt` on decode, `errTooDeep` on encode. The check runs when the Batch
frame is reached, **before** reading `n` or descending, so it applies even to an **empty** Batch, whose
empty sub-stream would still sit at depth 9.

Worked check against the public test 7: nine wrapping Batch frames give sub-streams at depths 1..9, so the
ninth Batch (in the depth-8 stream) is rejected. Eight wrappers give sub-streams at depths 1..8, which is legal.
Encoder and decoder share `maxDepth` and the same `d+1 > maxDepth` predicate, so the encoder cannot emit a
stream that its own decoder would reject. A property test asserts this (section 6).

## 4. Encode (`encode.go`)

Encode keeps two passes: validate and size everything first, then write. On any validation error, nothing is
written and the result is `nil, err`.

### 4.1 Normalization
`asConcrete` gains `Batch` (value) and non-nil `*Batch` (dereferenced). nil, typed-nil pointers, and other
types still produce `errUnsupportedRecord`. This also applies to elements inside `Batch.Records` at any depth.

### 4.2 Sizing pass: `sizeBody(r Record, depth int) (uint64, error)`
`depth` is the depth of the stream that `r` sits in. The function returns `r`'s frame `len` (type+payload).
- Put/Delete: same as v1 (`concreteBodyLen`, each length cast to `uint64` separately), then `> maxFrameLen`
  produces `errRecordTooLarge`.
- Batch:
  1. If `depth+1 > maxDepth`, return `errTooDeep`.
  2. Set `n := uint64(minBatchBody)`. For each child `j`: `cb, err := sizeBody(child, depth+1)`, wrapping the
     error with `fmt.Errorf("batch item %d: %w", j, err)`. Then `n += lenSize + cb + crcSize`, and **immediately**
     return `errRecordTooLarge` if `n > maxFrameLen`.
  3. No overflow is possible. Before each add, `n <= 16 MiB` and `cb <= 16 MiB`, so the sum is at most about 32 MiB.
     The early exit also bounds the work spent on an oversized Batch.
  4. `uint32(len(Records))` cannot truncate. Every child frame is at least 9 bytes (`minFrameSize`) and the body
     is capped at 16 MiB, so the count is far below 2^32.
- Self-referential input, such as `b := &Batch{}; b.Records = []Record{b}`, is stopped by the depth check.
  Encode returns `errTooDeep` instead of recursing until the stack overflows.
- Top level: `Encode` calls `sizeBody(r, 0)` for each record, wraps errors with the record index as v1 did, and
  accumulates `addFrameSize` as before.

This enforces the 16 MiB cap at every depth. An inner frame is strictly smaller than its enclosing frame,
which is itself checked.

### 4.3 Write pass: `appendFrame(out []byte, c Record) []byte` (len backpatching)
- Reserve 4 bytes for `len`, remember `start`, then write the type byte and payload:
  - Put/Delete: same as v1.
  - Batch: `kindBatch`, then `n = uint32(len(Records))`, then `appendFrame(out, asConcrete(child))` for each child in order.
- Backpatch `len = len(out) - start`, then append `crc32.ChecksumIEEE(out[start:])`.
- With backpatching, the write pass never recomputes sizes. That removes the v1 nit about size drift between the
  sizing and writing code, and it avoids re-sizing every nested Batch at each enclosing level, which would cost
  O(depth × size). The `total` from the sizing pass is still used only as the exact `make` capacity.
- Encode writes `version2` as the first byte. Encode of an empty or nil slice returns `[]byte{2}`, so the v1 public
  test 6 (length 1, decodes to empty) still holds.
- The write pass does not check depth, because the sizing pass already did. Recursion depth is at most 9.
  Concurrent mutation of `recs` during `Encode` is a caller data race and is documented as unsupported, as in v1.

## 5. Decode (`decode.go`)

### 5.1 Version
```go
if len(b) < 1 { return nil, ErrCorrupt }
switch b[0] {
case version1: allowBatch = false
case version2: allowBatch = true
default:       return nil, ErrCorrupt   // 0, 3, 255, ... rejected before any frame
}
recs, ok := decodeStream(b[1:], anyCount, 0, allowBatch)
```
A **version-1 stream may contain only Put and Delete**. A type-3 frame in a v1 stream is `ErrCorrupt`, which is
exactly what a v1 decoder does with it (unknown tag). On version-1 input, v2 `Decode` therefore accepts and rejects
exactly the same byte strings as v1 `Decode` and returns the same records. Section 6 checks this with a
differential test against a frozen copy of the v1 decoder.

### 5.2 One recursive sub-stream decoder
```go
// decodeStream decodes a framed sub-stream p that sits at depth `depth`.
// want < 0: any number of frames (the top level); otherwise exactly `want` frames,
// which must consume p exactly.
func decodeStream(p []byte, want int64, depth int, allowBatch bool) ([]Record, bool)
```
- **Pass 1 (structural):** count frames with `nextFrame` until `p` is empty. Any malformed header fails: short header,
  `len` 0 or above 16 MiB, or `len+crc` running past the end of `p`. If `want >= 0 && count != want`, fail. This covers
  fewer than `n` frames, more than `n` frames, and trailing bytes: trailing bytes either form an extra frame
  (count mismatch) or a malformed header (failure). The result slice is then allocated with
  `make([]Record, 0, count)`, a capacity bounded by the bytes actually present, never by the untrusted `n`.
- **Pass 2:** for each frame, verify the CRC, then `decodeBody(typ, payload, depth, allowBatch)`.
- The top level is `decodeStream(b[1:], -1, 0, allowBatch)`. Its v1 behavior is unchanged.

### 5.3 `decodeBody` gains a Batch case
```go
case kindBatch:
    if !allowBatch            { fail }         // Batch inside a version-1 stream
    if depth+1 > maxDepth     { fail }         // sub-stream would exceed depth 8
    if len(p) < countSize     { fail }
    n := binary.BigEndian.Uint32(p)
    sub := p[countSize:len(p):len(p)]
    if uint64(n) > uint64(len(sub))/minFrameSize { fail }   // cheap early reject
    inner, ok := decodeStream(sub, int64(n), depth+1, true)
    return Batch{Records: inner}, ok
```
`allowBatch` stays `true` inside Batches, which can only exist in v2 streams anyway. An empty Batch (`n = 0` with an
empty `sub`) decodes to `Batch{Records: []Record{}}`.

### 5.4 Containment: "never a read into the next outer frame"
- Each Batch sub-stream is decoded from a slice of exactly the enclosing payload, and `nextFrame` checks
  `uint64(len(avail)) < uint64(n)+crcSize` against **`len`**. An inner `len` that reaches past the Batch payload is
  therefore `ErrCorrupt` (truncated), even if the following outer bytes would make a valid frame.
- Go hazard addressed: `s[:k]` is legal for `k <= cap(s)` even when `k > len(s)`, so a sloppy bounds check on a
  subslice can silently read sibling bytes. To make that impossible rather than merely unlikely, `nextFrame` returns
  the body as `avail[:n:n]` and the Batch sub-stream is `p[countSize:len(p):len(p)]` (both full slice expressions).
  Any later out-of-range reslice then panics in tests instead of reading a neighbor. A dedicated test (section 6,
  "sibling-overrun") builds exactly that attack.
- The 16 MiB cap is enforced by `nextFrame` at **every** depth, because inner frames go through the same function.
  An inner `len` above 16 MiB also cannot fit in an enclosing payload of at most 16 MiB, so the check fails either way.

### 5.5 Cost bounds
CRC and the structural scan run once per nesting level over the bytes at that level. Total work is at most
`(maxDepth+1) × len(b)`, which is linear. Recursion depth is at most 9 frames, so there is no stack risk from
hostile input. The number of allocated records is at most `len(b)/9`.

## 6. Tests

The public files stay unchanged. New white-box tests go in `walcodec_internal_test.go` (package `walcodec`), and fuzz
targets go in `walcodec_fuzz_test.go`. No helper names collide with the public files. New helpers use a `t`
prefix, for example `tFrame` and `tBatchPL`.

**v2 rule → test mapping** (public test number in parentheses):

| Rule | Tests |
|---|---|
| Batch Kind 3 | `Batch{}.Kind()==3`; `*Batch` encodes the same as `Batch` |
| Batch round-trips (1), nested (2), mixed (4) | public, plus empty Batch, nil `Records`, Batch of empty Batches, duplicates inside a Batch |
| Golden wire layout | exact bytes for `Encode([]Record{Batch{[Put{"a","1"}, Delete{"b"}]}})` built by hand: version 2, outer len/type 3/n=2/inner frames/crc |
| Encode writes version 2 | `Encode(nil) == []byte{2}`; `b[0]==2` for Put-only input |
| Version gate (6) | versions 0, 3, 255 → `ErrCorrupt` on an otherwise valid stream; versions 1 and 2 with no frames → empty, no error |
| v1 stream: Put/Delete only | hand-built v1 stream with a valid type-3 frame → `ErrCorrupt` |
| v1 regression (3, 8) | public fixture tests, plus a **differential fuzz** (`FuzzV1Equivalence`): for inputs with `b[0]==1`, v2 `Decode` and a frozen copy of the snapshot-v1 decoder (test-only `decodeV1Ref`) agree on accept/reject and on records |
| Exact n | `n` = frames+1, `n` = frames−1, `n=0` with one frame, trailing 1..8 garbage bytes after n frames, payload shorter than 4 bytes → all `ErrCorrupt` |
| Huge n | `n=0xFFFFFFFF` with no frames → `ErrCorrupt`, and `testing.AllocsPerRun` shows no large allocation |
| Truncated Batch (5) | public, plus truncation at every byte offset of a nested encoding → `ErrCorrupt` (no panic) |
| Inner len past enclosing payload | **sibling-overrun**: the outer Batch's inner frame `len` claims bytes that belong to the next top-level frame, the inner CRC is computed over those combined bytes so an overrunning decoder would accept it, and the outer CRC is valid → `ErrCorrupt` |
| Inner CRC checked at every depth | flip a CRC byte of an inner frame at depth 2 and recompute all enclosing CRCs so only the inner one is wrong → `ErrCorrupt`; same for an inner unknown tag and an inner zero `len` |
| Cap at every depth | an inner frame header with `len = 16 MiB + 1` inside a Batch → `ErrCorrupt` |
| Depth cap (7) | hand-built 8 wrappers → decodes; 9 wrappers → `ErrCorrupt` (public); an **empty** Batch as the 9th level → `ErrCorrupt`; an empty Batch as the 8th level → decodes |
| Encoder depth cap | 8-deep nested `Batch` values → round-trip; 9-deep → error, `errors.Is(err, errTooDeep)`, `!errors.Is(err, ErrCorrupt)`, nil output; self-referential `*Batch` → `errTooDeep` (terminates) |
| Encoder cap/validation inside Batch | a Batch whose total body exceeds 16 MiB (two ~9 MiB Puts) → `errRecordTooLarge`; a nil or unsupported record nested 3 deep → `errUnsupportedRecord`; never `ErrCorrupt` |
| Encoder never emits a stream it rejects | `FuzzRoundTrip`: builds random nested trees (depth 0..10, sizes kept under the cap). Either `Encode` errors and `Decode` is never needed, or `Decode(Encode(x))` succeeds and equals `x` |
| Decoder robustness | `FuzzDecode`: no panics; every error is `ErrCorrupt`. On success, re-encoding (version 2) and decoding again gives equal records (records are compared, not bytes, because v1 input re-encodes as v2). Seeds: v1 fixture, nested encodings, the depth-8 and depth-9 hand-built streams |

## 7. Verification steps (implementation phase)

`gofmt -l .` (empty), `go vet ./...`, `go build ./...`, `go test ./...` (both public suites plus internal),
`go test -race ./...`, `GOARCH=386 go test ./...` (32-bit `int`), and short runs of each fuzz target
(`-fuzztime=20s`).

## 8. Rule-by-rule satisfaction summary

| Amendment rule | Where satisfied |
|---|---|
| New type `Batch{Records []Record}`, Kind 3; no other surface change | §2 |
| Payload = `n(4)` + n full frames, consumed exactly | §5.2 pass 1 count==n and empty remainder; §4.3 writer |
| Batch may contain Put/Delete/Batch (recursion) | §4.2/§4.3 recursive sizing and writing; §5.3 recursive decoding |
| Same 16 MiB `len` cap at every depth | §4.2 (encoder, each level); §5.4 (`nextFrame` at each level) |
| Inner `len` past enclosing payload → `ErrCorrupt`, never reads outer | §5.4 length check + full-slice-expression cap clipping; sibling-overrun test |
| Depth cap 8 (top-level = 0; sub-stream > 8 rejected) on decode and encode | §3, §4.2 step 1, §5.3; boundary tests at 8 and 9 including the empty-Batch edge |
| Encode writes version 2 | §4.3 |
| Decode accepts v1 (Put/Delete only) and v2 (all types) | §5.1, §5.3 `allowBatch` |
| Other versions (0, 3, …) → `ErrCorrupt` before any frame | §5.1 |
| v1 regression via a fixed v1 fixture | public tests 3/8 kept; differential fuzz vs. a frozen v1 decoder |
| v1 exported symbols unchanged | §1 |
