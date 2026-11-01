# Plan — `walcodec` v2 (nested `Batch` records), phase 2, round 3

Round 3. The round-2 review in `plan_findings.md` reports **no blocker, major, or minor issue**.
It confirms that the depth convention, version gating, frame bounds, count and trailing-byte
handling, v1-fixture behavior, and the §1a allocation contract all follow from the amendment and
the public tests. The response is in `plan_rebuttal.md`. The design is unchanged in this round.
Before writing this round I checked the plan again against `snapshot-v1/*.go`, both public test
files, and `amendment.md`. That check added two small things, neither of which changes the design:
- §2.3: a note bounding the encoder's size-pass work when the input is a shared or DAG-shaped
  `*Batch` graph.
- §2.4 test 11: the fuzz oracle also checks canonical bytes. For a successfully decoded input
  `b`, `Encode(Decode(b))` must equal `b` with byte 0 set to `2`.

History: round 2 added §1a (one allocation contract), corrected v1's overclaiming doc comment
(§2.1), and made §2.3 use a single `normalize` dispatch for both the size pass and the write pass.

## 0. Baseline and starting state (read this first)

- **The worktree has no v1 source code.** It holds only `go.mod`, `walcodec_public_test.go`, and
  `walcodec_v2_public_test.go`. Both test files match their `instrument/` copies byte for byte
  (checked with `diff`). The converged v1 implementation is in `snapshot-v1/`
  (`walcodec.go`, `encode.go`, `decode.go`). Its v1 internal, fuzz, and race-tag test files are
  missing from both places.
- **Step 0 of implementation:** copy `snapshot-v1/{walcodec,encode,decode}.go` into `worktree/`
  without changes. The package will not compile with the v2 test file present, because that file
  uses `Batch`. To confirm the baseline is green, run the v1 suite in a scratch copy of the
  worktree that excludes `walcodec_v2_public_test.go`. Do not move or edit the real file. Then
  apply the changes below as a diff against that baseline.
- The two public test files are **never edited**. When implementation ends, `diff` against
  `instrument/` must be empty.
- Checked against the public suites: the v1 `TestUnknownType` uses tag `99`, not `3`, so making
  tag 3 meaningful does not break it. The v1 file's header comment says no case asserts the written
  version byte is `1`. A grep confirms the only literal `1` version bytes are in hand-built
  streams (`TestUnknownType`, `TestOversizeLen`), and v2 still accepts those.

## 1. What stays (v1 invariants carried into v2 unchanged)

| v1 property | Status in v2 |
|---|---|
| Frozen exported surface: `Record`, `Put`, `Delete`, `Encode`, `Decode`, `ErrCorrupt` (same names and signatures) | Unchanged |
| Existing exported `KindPut`, `KindDelete` (part of the converged v1) | Unchanged |
| Frame shape `len(4) type(1) payload crc(4)`, big-endian, `len` = type+payload, crc = CRC32-IEEE over type+payload, outside `len` | Unchanged, and now applied at every depth |
| 16 MiB `len` cap, checked right after reading the 4-byte `len` and before anything else is read or allocated | Unchanged, and now applied at every depth |
| `Put` and `Delete` payload layouts, nil and empty `Value` decoding to a non-nil length-0 slice, no aliasing of the input (`copyBytes`, `string(k)`) | Unchanged |
| Decode allocation behavior | Unchanged in substance, now stated precisely. See §1a. This is the single source of truth |
| Records slice grows lazily (no `make` sized by any length or count field). Empty result is `[]Record{}` | Unchanged, and applied to Batch inner slices too |
| On error, Decode returns `nil` records | Unchanged |
| Encode accepts `Put`, `Delete`, non-nil `*Put`, `*Delete`, and rejects anything else including nil | Unchanged. `Batch` and non-nil `*Batch` are added |
| Encode is two-pass: a size/validation pass with overflow-safe `addSize`, then one exact `make` and a write pass. On error it returns no bytes. Encode errors do **not** wrap `ErrCorrupt` | Unchanged structure, made recursive |
| Duplicate keys kept in order. Records emitted and decoded in stream order | Unchanged |
| Empty `recs` gives a one-byte stream. `Decode(nil)` and `Decode([]byte{})` return `ErrCorrupt` | Unchanged (the byte is now `2`) |

## 1a. Decode allocation contract (resolves the round-1 finding)

The implementation, doc comments, and tests all use this one contract. Decode stays
**single-pass build-as-you-go**, exactly as v1. Validate-before-build is not adopted; §6 gives the
reason.

- **A1. Errors allocate nothing.** Every Decode error is a preallocated package-level sentinel
  that wraps `ErrCorrupt`, so building or returning an error never allocates.
- **A2. No allocation is sized by an untrusted field before it is validated.** A frame `len` is
  checked against the 16 MiB cap and against the bytes actually present before any payload byte is
  read. A Batch count `n` is never used as a capacity: there is no `make(..., n)`. Every
  allocation Decode makes is a decoded record or the slices that hold them, and their total is
  bounded by O(len(b)).
- **A3. Errors found before the first record is built allocate zero.** That record may be at any
  depth. The zero-allocation tests (§2.4 test 10) assert exactly this, with
  `testing.AllocsPerRun == 0`.
- **A4. Errors found after earlier records were built** have already allocated those records:
  earlier top-level frames, earlier siblings inside a Batch, or a Put's key and value copy. The
  records are discarded and Decode returns `(nil, err)`. The garbage is bounded by A2. v1 already
  behaves this way across top-level frames, so this is not a regression. It is just stated now.
  No test asserts zero allocations for this case, and §2.4 test 10 says so explicitly.

The v1 comment "Returning them never allocates, so no Decode error path allocates" in
`walcodec.go` claims more than v1 does (see A4). §2.1 replaces it with A1–A4 in brief.

## 2. What changes

### 2.1 `walcodec.go`
- Add the one new exported type:
  ```go
  // Batch groups records into a single frame whose payload is a nested framed sub-stream.
  type Batch struct{ Records []Record }
  func (Batch) Kind() uint8 { return kindBatch }   // == 3
  ```
- `kindBatch uint8 = 3` is **unexported**. The amendment says "the only addition to the surface is
  the new type `Batch`", so no `KindBatch` constant, new exported errors, or exported depth
  constant are added. `Batch{}.Kind()` still returns `3`.
- Constants: replace `version1` with `version1 uint8 = 1` and `version2 uint8 = 2`. Add
  `maxDepth = 8` and `batchCountSize = 4`. `maxFrameLen`, `lenFieldSize`, `crcFieldSize`, and
  `u32Size` are unchanged.
- New preallocated errors, all of the form `fmt.Errorf("%w: ...", ErrCorrupt)` at package init:
  - `errTooDeep`: "batch nesting exceeds depth 8"
  - `errBatchShort`: "batch payload ends before its record count is reached"
  - `errBatchCount`: "batch payload shorter than its 4-byte count"
  - The existing `errTrailing` is reused for bytes left after the n-th inner frame.
- Replace the comment on the error block with the precise contract: "Preallocated decode errors;
  all wrap ErrCorrupt. Returning one never allocates. Decode allocates only decoded records, never
  a buffer sized by an unvalidated length or count. See Decode's doc comment."
- `Decode`'s doc comment gets one added sentence: "An error found after some records were decoded
  discards them. Their allocation is not undone, but is bounded by len(b)."
- Update the package doc comment to describe the Batch payload, the depth rule, and versions 1 and 2.

### 2.2 `decode.go`: recursive and bounded by sub-slices
Structure, a direct extension of the v1 functions:

```go
func Decode(b []byte) ([]Record, error) {
    if len(b) == 0 { return nil, errNoVersion }
    var allowBatch bool
    switch b[0] {
    case version1: allowBatch = false      // Put/Delete only
    case version2: allowBatch = true
    default:       return nil, errBadVersion // 0, 3, ..., 255: before any frame
    }
    // top-level loop exactly as v1, calling decodeFrame(b[off:], 0, allowBatch)
}

// depth = depth of the (sub-)stream that contains this frame; top level is 0.
func decodeFrame(p []byte, depth int, allowBatch bool) (Record, int, error)
    // v1 order kept: short header -> oversize (>16 MiB) -> len==0 -> truncated -> crc
    // dispatch: KindPut, KindDelete as v1;
    //           kindBatch: if !allowBatch -> errUnknownType; else parseBatch(body[1:], depth)
    //           default -> errUnknownType

func parseBatch(p []byte, depth int) (Record, error) {
    inner := depth + 1
    if inner > maxDepth { return nil, errTooDeep }        // before reading anything in p
    if len(p) < batchCountSize { return nil, errBatchCount }
    n := binary.BigEndian.Uint32(p); rest := p[batchCountSize:]
    var recs []Record                                     // lazy; NO make(.., n)  (A2)
    for i := uint32(0); i < n; i++ {
        if len(rest) == 0 { return nil, errBatchShort }
        rec, used, err := decodeFrame(rest, inner, true)  // rest is a sub-slice of THIS payload
        if err != nil { return nil, err }
        recs = append(recs, rec); rest = rest[used:]
    }
    if len(rest) != 0 { return nil, errTrailing }         // more frames or trailing junk
    if recs == nil { recs = []Record{} }
    return Batch{Records: recs}, nil
}
```

Key decisions:
- **No read into a sibling or outer frame, by construction.** `parseBatch` receives only
  `body[1:]`, the outer frame's payload sub-slice ending at `lenFieldSize+len`. It never receives
  the rest of the stream. Every inner `decodeFrame` bounds-checks against `len(rest)`, so an inner
  `len` that runs past the enclosing payload fails the existing `len(p) < need` check with
  `errTruncated`, even when real bytes (the outer crc, the next outer frame) follow in `b`. This
  is a structural guarantee, not a separate check that could be forgotten.
- **No allocation driven by the untrusted `n` (A2).** `n` can be as large as 2^32−1. Preallocating
  with `make([]Record, 0, n)` would be an attacker-sized allocation. It would also break A3 for
  errors in the first inner frame. The loop ends on its own: each successful inner frame consumes
  at least 9 bytes, and an empty `rest` returns `errBatchShort` at once, so a huge `n` costs
  O(payload) work.
- **Depth check before parsing the sub-stream.** It runs after the Batch frame's own len and crc
  checks (v1 order) but before the count or any inner byte is read. A 9th level is rejected no
  matter what it contains, and with zero allocations (A3).
- **Version gating is passed down.** `allowBatch` is false only for version-1 streams. A Batch
  frame can only be reached when it is already true, so passing `true` inside `parseBatch` is
  correct. A version-1 stream containing a type-3 frame is `errUnknownType`, since v1 means
  "Put/Delete only".
- **CRC is verified at every depth.** The outer crc covers the inner bytes, but a hand-built
  stream can carry a valid outer crc over an invalid inner crc, and that must be `ErrCorrupt`.
  `decodeFrame` is reused, so this comes for free.
- Recursion depth is at most 9 calls, so stack use is trivial.
- Decoded Batches are `Batch` values, not pointers, matching how `Put` and `Delete` are returned.
  An empty Batch (`n = 0`, 4-byte payload) is legal. It decodes to `Batch{Records: []Record{}}`.

### 2.3 `encode.go`: recursive size pass and back-filled write pass
- `Encode` writes `version2`. Doc comment: "writes a version-2 stream; may include Batches".
- **One dispatch, shared by both passes.** `normalize` is extended so both passes call it:
  ```go
  // normalize extracts the wire fields of a supported record. For a Batch, kind is
  // kindBatch and children holds its records; key and val are unused.
  func normalize(r Record) (kind uint8, key string, val []byte, children []Record, err error)
      Put/*Put, Delete/*Delete: as v1 (children nil)
      Batch / non-nil *Batch:   kindBatch, children = x.Records
      nil, nil pointer, other:  "unsupported record type %T"
  ```
- The size/validation pass becomes recursive:
  ```go
  // frameBodyLen validates r as a record in a sub-stream at `depth` and returns its
  // len field (type+payload). Errors do not wrap ErrCorrupt.
  func frameBodyLen(r Record, depth int) (uint64, error)
      kind, key, val, children, err := normalize(r)
      Put/Delete: v1 arithmetic
      Batch:
          if depth+1 > maxDepth -> error "batch nesting exceeds depth 8"
          if uint64(len(children)) > math.MaxUint32 -> error (count field overflow)
          n := uint64(1) + batchCountSize
          for i, c := range children {
              cl, err := frameBodyLen(c, depth+1)    // wrap as "batch record %d: %w"
              if cl > maxFrameLen -> error (inner 16 MiB cap)
              n, ok = addSize(n, lenFieldSize+cl+crcFieldSize, uint64(maxFrameLen)); !ok -> cap error
          }
  ```
  The count guard can never fire in practice: each inner frame is at least 13 bytes, and the
  batch is capped at 16 MiB. It is kept for clarity. The top-level loop in `Encode` stays as in
  v1: call `frameBodyLen(r, 0)`, check it against `maxFrameLen`, and accumulate `total` with
  `addSize(..., maxInt)`. `addSize` is capped at `maxFrameLen` inside batches, so the size pass
  stops as soon as a batch is too big. It never sums past the cap and cannot overflow.
- **The encoder's depth rule matches the decoder's**: a Batch whose sub-stream would sit at depth
  > 8 is an error, so Encode never emits a stream Decode would reject. Nesting 8 deep works.
  Nesting 9 deep fails. This also prevents unbounded recursion on a self-referential `*Batch`
  graph (`b.Records = []Record{b}`), which fails at depth 9 instead of overflowing the stack.
- **Size-pass work is bounded even for shared subtrees.** A `*Batch` that appears many times in
  a graph (a DAG) is measured again each time it appears, so there is no memoization. This is
  still bounded. Every visited frame adds at least 13 bytes, and the batch sum stops at 16 MiB.
  So one top-level record costs at most about 16 MiB / 13 frame visits before it is accepted or
  rejected. Total work is O(output size), and the output must be produced anyway. No cycle
  detection is needed beyond the depth cap.
- Write pass: `appendRecord(out, r)` calls `normalize`, which cannot fail here because the size
  pass already accepted every record. It appends a len placeholder, the type byte, and the
  payload, then back-fills `len = len(out[start+4:])` and appends `crc32(body)`, as v1's
  `appendFrame` does. For a Batch the payload is `uint32(len(children))` followed by
  `appendRecord` for each child. The child frames sit fully inside the parent body before the
  parent's len and crc are computed, so back-filling works at every level with no second size
  computation. The final `make` capacity is exact. An internal test asserts
  `len(out) == total` (test 8).
- Encode errors stay plain `fmt.Errorf` errors that do not wrap `ErrCorrupt` (v1 contract), with
  the path included, e.g. `walcodec: record 2: batch record 0: batch nesting exceeds depth 8`.

### 2.4 Tests added (internal, package `walcodec`; public files untouched)
`walcodec_v2_internal_test.go`, plus `race_on_test.go`/`race_off_test.go` (a `raceEnabled`
build-tag constant) recreated because the snapshot did not keep them:

1. **Depth boundary (decode):** 8 hand-built nested Batches around a Put decode. 9 nested fail
   with `ErrCorrupt` (and specifically `errTooDeep` via `errors.Is`).
2. **Depth boundary (encode):** 8 nested `Batch` values encode and round-trip. 9 nested return a
   non-nil error, not `ErrCorrupt`, and nil bytes. A self-referential `*Batch` returns an error
   without overflowing the stack.
3. **Versions:** bytes 0, 3, and 255 on an otherwise valid stream give `errBadVersion`.
   A version-1 stream with a valid type-3 frame gives `errUnknownType`. The same bytes with
   version 2 decode.
4. **No read-through:** a hand-built outer Batch whose single inner frame claims `len` 1 byte past
   the Batch payload. The outer frame has a correct crc and is followed by a valid second
   top-level frame, so the "past" bytes really exist in `b`. Expect `errTruncated`.
5. **Count mismatches:** `n = 2` with one inner frame gives `errBatchShort`. `n = 1` with two
   frames gives `errTrailing`. `n = 1` plus 3 trailing junk bytes gives `errTrailing`. Payload
   shorter than 4 bytes gives `errBatchCount`. `n = 0xFFFFFFFF` over a tiny payload fails fast.
6. **Inner frame faults with a valid outer crc:** bad inner crc gives `errCRC`. Inner
   `len > 16 MiB` gives `errOversize`. Inner `len = 0` gives `errEmptyFrame`. Inner unknown type
   gives `errUnknownType`.
7. **Empty Batch:** `Batch{}` and `Batch{Records: []Record{}}` both round-trip to a 0-length
   Records slice. `Encode` of a Put/Delete-only list writes byte 0 = `2`.
8. **Encode cap:** a Batch whose inner frames are each under 16 MiB but whose total exceeds it is
   rejected. So is a Batch containing an oversize Put. `len(Encode(x)) == total` is checked over
   a varied corpus.
9. **No aliasing:** mutating the input after Decode does not change decoded Put values inside
   Batches.
10. **Allocation contract (§1a)**, skipped when `raceEnabled`.
    - *A3, zero allocations asserted* with `testing.AllocsPerRun(...) == 0`. Each stream is
      hand-built so the fault comes before any record exists at any depth: bad version, missing
      version, inner oversize `len` as the first inner frame, depth 9, `errBatchCount`,
      `errBatchShort` with `n = 1` and an empty sub-stream, and a truncated first inner frame. The
      v1 top-level cases are included too (oversize, truncated, crc, unknown type in the first
      frame).
    - *A4, documented and deliberately not asserted:* a comment in the test file states that a
      fault after a valid earlier record (top-level or sibling) may allocate that record, per §1a.
      No AllocsPerRun assertion is made for those streams. The only check is that they return
      `(nil, ErrCorrupt)`.
    - *A1* follows from the sentinels. Test 10 also checks that each new error value satisfies
      `errors.Is(err, ErrCorrupt)` and is a package-level variable (identity compare).
11. **Fuzz** `FuzzDecode`, seeded with the v1 fixture, the v2 public streams, and the deep-nest
    stream. Decode must never panic. When Decode succeeds, `Encode(recs)` must succeed and
    `Decode` of that output must equal `recs` (a deep compare including Batch). The encoding must
    also be canonical: `Encode(recs)` must equal the input `b` with byte 0 replaced by `2`. This
    holds for both v1 and v2 inputs. Decode rejects trailing bytes at every level and every field
    has exactly one encoding, so any accepted stream re-encodes to the same frames. On error, the
    returned records must be `nil`.

## 3. How each v2 rule is satisfied

| Amendment rule | Mechanism |
|---|---|
| `Batch{Records []Record}`, Kind 3, only new surface | §2.1: exported `Batch` with `Kind()==3`. Constant `kindBatch` unexported. Nothing else exported |
| Payload = `n uint32` + n full inner frames of the same shape | `parseBatch` reads `n`, then calls the same `decodeFrame` n times. Encoder writes `uint32(len)` and then the child frames through the same `appendRecord` |
| Batch may contain Put, Delete, or nested Batch | Recursive `decodeFrame`/`parseBatch` and recursive `frameBodyLen`/`appendRecord` |
| Same 16 MiB cap at every depth | Inner frames go through the same `decodeFrame` (oversize check right after the header). The encoder checks each inner `cl` and caps batch sums at `maxFrameLen` |
| Inner `len` past the enclosing payload → ErrCorrupt (truncated), never reading the next outer frame | Sub-slice bounding (§2.2). Inner decode only ever sees the enclosing payload slice. Test 4 |
| n frames consume the payload exactly. Fewer, more, or trailing bytes → ErrCorrupt | `errBatchShort` when the payload runs out before n, `errTrailing` when bytes remain after n. Test 5 |
| Depth cap 8. Top level is depth 0. Inner sub-stream = Batch's depth + 1. Depth > 8 → ErrCorrupt | `parseBatch` computes `inner := depth+1` and rejects when `inner > 8`. Top-level frames are decoded with `depth = 0`. Public test 7 plus internal test 1 (8 accepted, 9 rejected) |
| Encoder enforces the same cap | `frameBodyLen` uses the identical `depth+1 > maxDepth` rule. Test 2 |
| Encode writes version 2 | `out = append(out, version2)`. Test 7 |
| Decode accepts version 1 (Put/Delete only) and version 2 (all types) | `allowBatch` switch in `Decode`. A Batch in a v1 stream is `errUnknownType`. Test 3 |
| Any other version, including 0 and 3 → ErrCorrupt before any frame | `default: errBadVersion` before the frame loop. Public test 6, test 3 |
| v1 regression via a fixed v1-bytes fixture | The v1 path in `decodeFrame` for Put and Delete is unchanged. Public tests 3 and 8 decode the literal fixture and check crc flip and truncation. The v1 public suite also runs unchanged. None of its cases asserts the version byte is `1` |
| v1 exported symbols keep names and signatures | No signature edits. A compile of both public suites proves it |

## 4. Public v2 test walk-through (expected path)
1. `TestBatchRoundTrip`: Encode gives `[2][Batch frame: n=2, Put frame, Delete frame]`. Decode
   gives an equal `Batch`.
2. `TestNestedBatchRoundTrip`: inner sub-streams sit at depths 1 and 2 (≤ 8) and round-trip.
3. `TestV1FixtureDecodesUnderV2`: version 1 is accepted with `allowBatch=false`, and the Put and
   Delete frames decode as in v1.
4. `TestMixedTopLevelRoundTrip`: `Put{last, nil}` decodes to a 0-length value, and `recEq2`
   compares through `string()`, so they are equal.
5. `TestTruncatedBatch`: cutting 2 bytes clips the outer crc, so the outer `decodeFrame` returns
   `errTruncated`.
6. `TestVersion3Rejected`: `errBadVersion`.
7. `TestBatchNestedPastDepth8`: the outermost Batch is at depth 0. The 9th Batch is at depth 8,
   and its sub-stream would be at depth 9, so `errTooDeep`.
8. `TestV1BehaviorsViaFixture`: the last-byte flip gives `errCRC` on the Delete frame. Cutting 3
   bytes gives `errTruncated`.

## 5. Acceptance checklist
`gofmt -l` is empty. `go build ./...` and `go vet ./...` are clean. `go test -count=1 ./...` passes
(v1 public + v2 public + internal). `go test -race ./...` passes. A short `go test -fuzz=FuzzDecode`
run finds nothing. `diff` of both public test files against `instrument/` is empty. No exported
identifiers besides `Batch` (and its `Kind` method) are added, checked with
`go doc -all . | diff` against the v1 surface. Every doc comment that mentions allocation matches
§1a. No comment may claim that "no Decode error path allocates".

## 6. Risks and trade-offs noted
- `kindBatch` is unexported while `KindPut`/`KindDelete` stay exported. This asymmetry is chosen
  deliberately to obey "the only addition to the surface is `Batch`".
- Inner CRCs are redundant with the outer CRC for honest streams. They are verified anyway because
  the amendment requires the same frame shape and a hand-crafted stream can be wrong only inside.
- **Why validate-before-build was not adopted (§1a).** A two-pass decoder would make every error
  path allocate zero. The cost is a second traversal of every frame, or a `build bool` threaded
  through every parse function, for a property that neither the v1 nor the v2 spec requires. The
  properties that matter for safety hold either way: allocation bounded by input size, nothing
  sized by an unvalidated field, and error values that allocate nothing (A1, A2). The current
  approach is v1's existing behavior, now stated honestly instead of overstated. If a reviewer
  needs the stronger property, the change is local to `decode.go`: a validation pass that reuses
  `decodeFrame` with building disabled, then a build pass. It would not touch the public surface.
