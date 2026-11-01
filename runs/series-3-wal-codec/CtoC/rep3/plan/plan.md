# Plan: walcodec v2: nested `Batch` records (revision 2)

Status: plan only, nothing implemented. This revision responds to `plan_findings.md` (round p2_r1).
The point-by-point response is in `plan_rebuttal.md`. Every finding is accepted. Changes from
revision 1 are marked **[F1]…[F9]**, matching the order of the findings file.

## 0. Starting state (read before anything else)

- `worktree/` holds only `go.mod`, `walcodec_public_test.go` (the v1 suite) and
  `walcodec_v2_public_test.go` (the v2 suite). **There is no `walcodec.go` in the worktree.** The
  converged v1 implementation exists only at `snapshot-v1/walcodec.go`.
- Implementation step 1: copy `snapshot-v1/walcodec.go` to `worktree/walcodec.go` as the base, then
  apply the changes below. `go.mod` (`module walcodec`, `go 1.25`) does not change. Go 1.25 supports
  multiple `%w` verbs in one `fmt.Errorf`, which [F1] relies on.
- Both public test files stay **byte-identical** to `instrument/`. They share package `walcodec`.
- Still open from the last v1 code review (c1_3): one **minor** (`grow` always doubles capacity, so
  output can carry up to ~2x slack) and three **nits** (no multi-record growth test; a >2 GiB output
  on 32-bit panics; `grow` reimplements `slices.Grow`). The encoder rewrite in §3 resolves all four.
  The 32-bit nit is resolved fully; for 64-bit see [F4].

### Reserved identifiers (new files must not declare these) [F8]
The public test files declare the following names in package `walcodec`. An internal test file that
redeclares any of them fails to compile.
- v1 helpers: `beU32`, `mkFrame`, `recEq`, `recsEq`.
- v2 helpers: `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `recEq2`, `recsEq2`, `v1Fixture`,
  `v1FixtureRecords`.
- v1 tests: `TestRoundTripPutDelete`, `TestCRCFlip`, `TestUnknownType`, `TestTruncatedFrame`,
  `TestOversizeLen`, `TestEmptyRecsVersionOnly`, `TestDecodeNilEmpty`,
  `TestDuplicateKeysKeptInOrder`, `TestEmptyVsNilValue`.
- v2 tests: `TestBatchRoundTrip`, `TestNestedBatchRoundTrip`, `TestV1FixtureDecodesUnderV2`,
  `TestMixedTopLevelRoundTrip`, `TestTruncatedBatch`, `TestVersion3Rejected`,
  `TestBatchNestedPastDepth8`, `TestV1BehaviorsViaFixture`.

Naming convention for the internal file: every test function starts with `TestInternal`, the fuzz
target is `FuzzInternalDecode`, and every helper starts with `it`, for example `itNest`, `itStream`
and `itOffset`. That prefix can't collide with anything in either public file. The internal file may
*call* the public helpers (`mkFrame2`, `putPL2`, `batchPL2`, `recsEq2`, `v1Fixture`).

## 1. What stays (frozen)

- Exported surface: `Record`, `Put`, `Delete`, `Encode`, `Decode` and `ErrCorrupt` keep their names,
  signatures, Kind values (1 and 2) and the `ErrCorrupt` text. `Batch` is the only new exported symbol.
- Frame shape `len(4) type(1) payload(len-1) crc(4)`:
  - big-endian, with the CRC32-IEEE taken over type+payload;
  - `len` counts type+payload only;
  - the 16 MiB cap is inclusive and is checked before any allocation;
  - `len == 0` is rejected as a zero-length frame.
- Put and Delete payload layouts and checks stay the same:
  - The payload must be consumed exactly, so trailing payload bytes are corrupt.
  - A nil `Value` and an empty one encode the same, and both decode to a length-0, non-nil `[]byte`.
- Order of checks for each frame, at every depth:
  1. the header is present;
  2. `len != 0`;
  3. `len <= cap`;
  4. there are enough bytes;
  5. the CRC matches;
  6. the type is dispatched.

  The CRC is checked before the tag.
- Every Decode error wraps `ErrCorrupt` through `corrupt(off, ...)`, with an absolute byte offset.
  - On error, Decode returns nil records.
  - A stream with no frames decodes to an empty, non-nil slice.
  - Decoded keys and values are copies and never alias the input.
- Encode returns `nil, err` on failure. Encode rejects these records at **any** depth:
  - nil records;
  - pointer records (`*Put`, `*Delete`, and now `*Batch`);
  - foreign `Record` implementations.
- Encode's v1 errors (`errUnsupportedRecord`, `errRecordTooLarge`) still do **not** wrap
  `ErrCorrupt`. The one new exception is the depth error ([F1], §3).

## 2. What changes: new surface, constants, and doc comments

```go
// Batch groups records into a single frame (Kind 3). Its payload is a nested
// framed sub-stream: n(4) followed by n frames of the ordinary frame shape.
// A Batch may contain Put, Delete or Batch records, up to 8 levels deep.
type Batch struct{ Records []Record }

// Kind returns the Batch type tag, 3.
func (Batch) Kind() uint8 { return kindBatch }
```

Constants (unexported):
- `version1 = 1`: "legacy version: Put and Delete only. Decode still accepts it; Encode never writes it."
- `version2 = 2`: "current version: Put, Delete and Batch. Encode always writes it."
- `kindBatch = 3`.
- `maxDepth = 8`: "the deepest legal sub-stream depth. The top-level stream is depth 0."
- `countSize = 4`: the size of the Batch `n` field.
- `batchHintCap = 16`: the upper bound on the capacity hint for a decoded Batch's records ([F2]).
- `minFrameSize` from revision 1 is **removed**, because [F2] no longer needs it.

New unexported errors:
- `errNestingTooDeep = errors.New("batch nesting exceeds depth 8")`. The depth error itself is
  built as described in [F1], §3.
- `errStreamTooLarge`: returned when the total stream size would not fit in an `int` ([F4]).
- `errEncoderInvariant = errors.New("walcodec: internal error: encoded size mismatch")`. This
  "should not happen" guard is explained in [F3].

**Doc comments that must be rewritten so no stale text survives [F7]:**
1. **Package doc.**
   - Replace "Encode writes version 1, the only version Decode accepts" with: "Encode writes
     version 2. Decode accepts version 1 (Put and Delete only) and version 2 (Put, Delete and
     Batch). Any other version is ErrCorrupt."
   - Add the `batch payload = n(4) frame{n}` grammar line.
   - Add the depth convention: top level = 0, a Batch's sub-stream = the Batch's depth + 1, and a
     depth above 8 is corrupt.
   - Extend the `# Errors` section with the Encode depth exception.
2. **`Encode` doc.**
   - Replace "writes recs as a version-1 stream" with "writes recs as a version-2 stream, in order".
   - Replace "Only Put and Delete values are accepted" with "Only Put, Delete and Batch values (not
     pointers) are accepted, at any nesting depth".
   - Add: "nesting deeper than 8 levels is rejected with an error that wraps ErrCorrupt (Encode
     refuses to emit a stream Decode would reject as corrupt). Encode's other errors do not wrap
     ErrCorrupt."
3. **`version1` const comment.** The comment "the only version this decoder knows; Encode writes it"
   is replaced as described above.
4. **`decodeFramesV1` / `decodeRecordV1`.** These are renamed to `decodeStream` / `decodeRecord`, so no
   "V1" names remain on code that handles both versions.
5. **`Decode` doc.** Add "accepts version 1 and version 2 streams".
6. **`beginFrame`, `grow`, `finishFrame` comments.** They are rewritten or deleted to match §3, since
   `grow` is deleted.

## 3. Encoder design

Encoding takes two passes. Pass 1 sizes and validates. Pass 2 writes. Pass 2 also enforces the
invariants independently ([F3]).

**Pass 1, sizing and validation: `frameLen(r Record, depth int) (uint64, error)`.** It returns the
frame's `len` value (type+payload) and doesn't allocate.
- `Put` is `1 + 4 + len(Key) + 4 + len(Value)`. `Delete` is `1 + 4 + len(Key)`.
- A `Batch` sits in the stream at depth `depth`, so its sub-stream sits at `depth+1`.
  - If `depth+1 > maxDepth`, it returns the depth error **before** looking at any child. Empty
    Batches at an illegal depth are therefore rejected too.
  - Otherwise the size is `1 + countSize + Σ(lenSize + frameLen(child, depth+1) + crcSize)`.
  - The sum is computed in `uint64`. It **returns `errRecordTooLarge` as soon as the running sum
    exceeds `maxFrameLen`**. After that early exit, `len(Records) < maxFrameLen`, so converting it to
    `uint32` is safe.
- Any result `> maxFrameLen` returns `errRecordTooLarge`, at every level.
- Any other type (nil, a pointer, a foreign implementation) returns `errUnsupportedRecord`.
- Errors carry an index path, wrapped with `%w` at each level:
  `walcodec: encode record 3: batch record 1: batch record 0: <cause>`.

**[F1] Depth error wraps ErrCorrupt.**
- The leaf is built as `fmt.Errorf("%w: %w", ErrCorrupt, errNestingTooDeep)`. Its text is
  `walcodec: corrupt stream: batch nesting exceeds depth 8`. The index path is wrapped around it
  with `%w`.
- So `errors.Is(err, ErrCorrupt)` and `errors.Is(err, errNestingTooDeep)` both hold on Encode's
  return.
- Rationale, to be stated in the Encode doc: the amendment says "a deeper nest is ErrCorrupt" and
  "the encoder enforces the same cap" without scoping that to Decode. Size and unsupported-type
  errors keep the v1 policy of not wrapping ErrCorrupt, because the v1 spec ties ErrCorrupt only to
  Decode and that behaviour converged in v1.

**[F4] Total size, checked while summing.** `total` starts at 1 (the version byte). For each
top-level record, `total += lenSize + frameLen + crcSize` in `uint64`. **Encode returns
`errStreamTooLarge` immediately** if `total > math.MaxInt`, before it sizes any further records.
- This fixes the 32-bit `make` panic: totals above 2 GiB return an error.
- **Stated plainly:** on 64-bit, a total between the runtime's maximum allocation size (~2^48 on
  amd64) and `math.MaxInt` still makes `make` panic. Reaching that needs about 2^24 top-level
  records that alias one 16 MiB buffer. The package cannot know the runtime's limit, and it doesn't
  invent a stream-size cap the spec doesn't have. Any Go program trying to allocate that much hits
  the same failure. This residual is documented in a code comment beside the check.

**Pass 2, writing: `out := make([]byte, 0, total)` (one allocation, no slack), then
`appendFrame(dst, r)`.**
- `beginFrame(dst, tag)` appends a 4-byte placeholder `len` plus the tag, and returns `start`.
- Put and Delete append their fields with `appendField`, which is unchanged.
- Batch appends `uint32(len(Records))`, then calls `appendFrame` on each child recursively.
- **[F3]** `finishFrame(dst, start) ([]byte, error)` works as follows:
  - It computes `n := len(dst) - start - lenSize`.
  - If `n > maxFrameLen`, it **returns `errRecordTooLarge` and does not backpatch**.
  - Otherwise it backpatches `len = n` and appends the CRC over `dst[start+lenSize:]`. The CRC is
    computed **after** all inner frames are fully written.
  - Pass 2 therefore applies the cap check to the bytes actually written, not just to pass 1's
    arithmetic. Even if `frameLen` and `appendFrame` drift apart, no frame over the cap is ever
    returned.
- **[F3]** After pass 2, Encode checks `len(out) == int(total)`.
  - On a mismatch it returns `nil, errEncoderInvariant` and emits no bytes.
  - This catches any drift between the two layout definitions that stays under the cap.
  - Depth and type validity can't drift, because pass 2 only runs after pass 1 has accepted the same
    tree, and the type switch only has Put/Delete/Batch arms.
- `appendFrame`'s `default` arm returns `errUnsupportedRecord`. An internal test covers it directly
  ([F9], §6).
- `grow` is deleted, which fixes the carried minor and the `slices.Grow` nit.

`Encode(nil)` and `Encode([]Record{})` still produce a 1-byte stream, `[]byte{2}`.
`Batch{Records: nil}` and `Batch{Records: []Record{}}` produce identical bytes (`n = 0`).

## 4. Decoder design

```
Decode(b):
  len(b)==0           → corrupt(0, "missing version byte")
  b[0]==1             → d{batches:false}.decodeStream(b[1:], base=1)
  b[0]==2             → d{batches:true }.decodeStream(b[1:], base=1)
  otherwise (0,3,...) → corrupt(0, "unknown version %d")   // before any frame is examined
```

A decoder struct `dec{ batches bool }` holds the flag for each version.

**`readFrame(region []byte, pos, base int) (tag byte, payload []byte, next int, err error)`.** This is
the v1 frame-loop body, extracted without changing its checks. Every error it reports uses offset
`base + pos`.
- `region` is always the **enclosing region only**: either the top-level frame area or exactly one
  Batch's frame area.
- An inner `len` that runs past its Batch therefore fails the "enough bytes" check and is reported
  as "truncated frame". Go slice bounds make it impossible to read a sibling or an outer frame.

**Offset arithmetic, pinned [F5].** All offsets are absolute in `b` (the full stream, version byte
included).
- Top-level region: `b[1:]`, `base = 1`. The frame at region position `pos` has absolute offset
  `absOff = 1 + pos`. This matches v1's numbering exactly.
- A Batch frame at `absOff` (the offset of its `len` field):
  - its payload starts at `absOff + lenSize + 1`;
  - its region (the frame area after `n`) starts at
    **`regionBase = absOff + lenSize + 1 + countSize`** (i.e. `absOff + 9`);
  - the inner frame at region position `pos` has `absOff = regionBase + pos`, which is passed to
    `readFrame` as `base = regionBase`.
- Batch-level errors report these offsets:
  - "batch nesting exceeds depth 8" and "truncated batch count" → the Batch's `absOff`;
  - "batch has fewer frames than count" and "trailing bytes after batch frames" →
    `regionBase + pos`, the point where the region ran out or where the stray bytes begin.

**Top-level `decodeStream`:** loops `readFrame` → `decodeRecord` until the region is exhausted, as in v1.

**`decodeRecord(tag, payload, depth, absOff)`:** Put and Delete are unchanged from v1. Kind 3 runs these steps:
1. If `!d.batches` (a version-1 stream), report corrupt "unknown record type 3". v2 must "return the
   same records a v1 decoder would", and a v1 decoder rejects tag 3.
2. Compute `inner := depth + 1`. If `inner > maxDepth`, report corrupt "batch nesting exceeds depth 8".
   This happens **before** `n` or any child is read.
3. If `len(payload) < countSize`, report corrupt "truncated batch count". Otherwise
   `n := BigEndian.Uint32(payload)` and `region := payload[countSize:]`.
4. **[F2] Capacity hint: `make([]Record, 0, min(n, batchHintCap))`**, and let `append` grow it from
   there.
   - The slice is always non-nil, even when `n == 0`.
   - Records are appended only after a child has decoded successfully, and every decoded record
     corresponds to a distinct frame header in the input. So the total record-slice memory across
     all levels is O(number of real frames) ≤ O(input/13), whatever `n` claims.
   - This removes the overlapping-region amplification from revision 1, where each level's hint
     was `len(region)/13` and was allocated before any child failed.
   - Option (a), pre-scanning headers, was rejected. It would be a second header walk with its own
     set of checks, which means another source of truth to keep in sync with `readFrame`. Option
     (b) keeps a single walk.
5. Loop `for i := uint32(0); i < n; i++`:
   - If `pos == len(region)`, report corrupt "batch has fewer frames than count".
   - Otherwise call `readFrame(region, pos, regionBase)`, then
     `decodeRecord(tag, payload, inner, regionBase+pos)`, append the result, and advance `pos`.
   - Child errors are returned unchanged. They already carry an absolute offset.
6. After `n` frames, if `pos != len(region)`, report corrupt "trailing bytes after batch frames".
   This covers both "more than n frames" and stray bytes.
7. Return `Batch{Records: recs}`.

Bounds:
- Recursion is at most 9 Go frames deep.
- CRCs are checked at every depth, so a byte is hashed at most 9 times. Every frame "of the same
  shape" has to carry a valid CRC, so this bounded factor can't be avoided.

## 5. Rule-by-rule traceability

| v2 rule | Satisfied by |
|---|---|
| `Batch` type, Kind 3 | §2 `Batch` and `Kind()` returns `kindBatch` |
| Payload = `n(4)` + n frames of the same shape | §3 pass 2 Batch branch; §4 steps 3–5 (reuses `readFrame`) |
| Put/Delete/Batch inside, recursion | §4 step 5 recursive `decodeRecord`; §3 recursive `appendFrame` |
| 16 MiB cap at every depth | `readFrame` checks the cap on every frame (cap checked before overrun); the encoder checks it in pass 1 and again in pass 2 `finishFrame` [F3] |
| An inner len past its enclosing payload is ErrCorrupt and never reads outer bytes | `readFrame` sees only the Batch's own `region` (§4) |
| Exactly n frames; fewer, more or trailing bytes is ErrCorrupt | §4 steps 5–6 |
| Depth: top level = 0, sub-stream = Batch depth + 1, a depth above 8 is ErrCorrupt | §4 step 2; same arithmetic in §3 `frameLen` |
| The encoder enforces the same cap and won't emit what it rejects | Pass 1 checks depth, cap and types with the decoder's rules. Pass 2 re-checks the cap on written bytes and checks the total [F3]. The depth error wraps ErrCorrupt [F1] |
| Encode writes version 2 | §3, `out = append(out, version2)` |
| Decode accepts 1 (Put/Delete only) and 2 (all) | §4 dispatch plus the `d.batches` gate (step 1) |
| Version 0, 3 or any other is ErrCorrupt before any frame | §4 dispatch, in the same order as v1 |
| v1 surface frozen | §1 |
| v1 regression checked against fixed v1 bytes | The v1 path is the same `readFrame`/`decodeRecord` code with `batches=false`. v2 tests 3 and 8 use the literal fixture |

### Public test walk-through
- **v2 #1, #2, #4** (Batch, nested Batch, mixed round-trip): the encoder and decoder are symmetric,
  and the nesting reaches depth 2. `Put{Value: nil}` decodes to an empty value, and `recEq2`
  compares values with `string(...)`, so the test passes.
- **v2 #3, #8** (v1 fixture): version 1 goes down the Put/Delete path.
  - Flipping the last byte breaks the Delete frame's CRC.
  - Truncating 3 bytes fails the "enough bytes" check at absolute offset 26.
- **v2 #5** (Batch truncated by 2 bytes): the outer frame fails the "enough bytes" check.
- **v2 #6** (version 3): rejected at dispatch.
- **v2 #7** (9 Batch wrappers): the k-th wrapper from the outside opens a sub-stream at depth k. The
  9th is at depth 9 > 8, so it is ErrCorrupt.
- **v1 suite, unchanged:**
  - The CRC-flip, truncation, duplicate and nil-vs-empty tests don't depend on the version byte.
  - `TestEmptyRecsVersionOnly` gets `[2]`, which has length 1 and decodes to empty.
  - `TestUnknownType` and `TestOversizeLen` build version-1 streams by hand. Those streams are still
    accepted at dispatch and still rejected for the same reasons.

## 6. Internal tests (new file `walcodec_internal_test.go`; naming per §0 [F8])

1. **`TestInternalDepthBoundary`.**
   - Decode: 8 nested wrappers decode OK; 9 are corrupt; an **empty** Batch at the 9th level is
     corrupt.
   - Encode: 8-deep nesting succeeds. 9-deep nesting, including an empty innermost Batch, returns
     `nil` plus an error where **`errors.Is(err, ErrCorrupt)` and
     `errors.Is(err, errNestingTooDeep)` are both true**.
   - The error text names the index path (`encode record 0: batch record 0: …`). [F1]
2. **`TestInternalBatchCountMismatch`**, table-driven. All of these must be corrupt:
   - `n=2` with 1 frame (fewer);
   - `n=1` with 2 frames (more);
   - `n=1` with 1 frame and 3 stray bytes (trailing);
   - a payload of 0–3 bytes (missing count).
3. **`TestInternalInnerOverrunNoReadThrough`:** an inner frame's `len` points past the end of its
   Batch payload, while a valid sibling top-level frame follows. The result must be corrupt, with
   the text "truncated frame" and the inner frame's absolute offset.
4. **`TestInternalInnerLenOverCap` [F6]:** an inner `len = 16 MiB + 1` inside a small valid Batch.
   The test asserts **`errors.Is(err, ErrCorrupt)` and that the error text contains "exceeds cap"**.
   - `readFrame` checks the cap before the overrun, so this text appears only if the cap check
     ran. If the cap check is deleted, the error becomes "truncated frame" and the test fails.
   - The comment on the test explains that ordering dependency.
5. **`TestInternalNestedErrorOffsets` [F5]:** a version-2 stream laid out as
   `[2] PutFrame Batch{ PutFrame Batch{ DeleteFrame(with a flipped CRC) } }`, where both outer CRCs
   are recomputed so they're valid.
   - The expected offset is computed from the §4 formula:
     `1 + len(PutFrame) + 9 + len(innerPutFrame) + 9`.
   - The test asserts that the error contains `(offset N)` with that value.
   - A second case checks that "batch has fewer frames than count" reports `regionBase + pos`.
6. **`TestInternalHostileCountAllocation` [F2]:**
   - Build a chain of 8 nested Batches, each claiming `n = 0xFFFFFFFF`, around one `Put` with a
     1 MiB value. The result must be corrupt ("fewer frames").
   - Measure `runtime.MemStats.TotalAlloc` across one Decode. Assert the delta is
     `< 3 * len(stream)`.
   - Revision 1's hint would allocate about 8 × 1.26 MiB, so this test would have failed against it.
   - A single-level case with an empty region and `n = 0xFFFFFFFF` is kept as well.
   - The budget is generous enough to hold under `-race` and `-N -l`.
7. **`TestInternalTag3InV1Stream`:** a Batch frame under version 1 is corrupt. The same bytes under
   version 2 decode.
8. **`TestInternalVersions`:** version bytes 0, 3 and 255 are corrupt. `[1]` and `[2]` each decode to
   an empty, non-nil slice.
9. **`TestInternalEmptyBatch`:** `Records: nil` and `Records: []Record{}` encode to identical bytes and
   decode to a non-nil, empty `Records`.
10. **`TestInternalEncodeRejects`:**
    - `*Batch`, nil and foreign records inside a Batch at depth ≥ 1 are rejected, with an error path
      that names the index. These errors are **not** ErrCorrupt (v1 policy).
    - An oversize child inside a Batch is rejected with `errRecordTooLarge`.
    - A Batch whose total exceeds 16 MiB while every child fits (an aliased 1 MiB `Value` repeated
      17 times) is rejected with `errRecordTooLarge` without allocating the output. The TotalAlloc
      delta must be < 1 MiB.
11. **`TestInternalFinishFrameCap` [F3]:** calls `finishFrame` directly on a `dst` whose frame body is
    `maxFrameLen + 1` bytes and expects `errRecordTooLarge` with `dst` unmodified. A
    `maxFrameLen`-byte body must succeed and backpatch the correct `len`.
12. **`TestInternalAppendFrameDefault` [F9]:** calls `appendFrame` directly with a foreign `Record`
    and with `*Put`. Both return `errUnsupportedRecord`. This covers the defensive `default` arm.
13. **`TestInternalCanonicalReEncode`:** for every v2 stream that decodes successfully,
    `Encode(Decode(b)) == b` byte for byte. For a v1 stream, the output equals `b` with byte 0
    changed to 2.
14. **`TestInternalExactPresize`:** a multi-record, multi-level input yields `cap(out) == len(out)`.
15. **`TestInternalNoAliasing`:** mutating the input after Decode does not change nested decoded values.
16. **`FuzzInternalDecode`:**
    - Seeds: the v1 fixture, the v2 public streams and the 9-deep stream.
    - Property: Decode never panics, and when it succeeds, property #13 holds.

## 7. Verification before declaring done

- `gofmt -l` is empty, `go vet ./...` is clean, and `go test ./...` passes (v1 and v2 public tests
  plus the internal tests).
- `go test -race`, `GOARCH=386 go test`, `go test -gcflags=all='-N -l'`, and a short
  `go test -fuzz=FuzzInternalDecode` run all pass.
- **Coverage [F9]:** the target is 100% of `walcodec.go` **except one documented statement**, the
  `return nil, errEncoderInvariant` after the `len(out) != int(total)` check in Encode.
  - That branch is unreachable while pass 1 and pass 2 agree. Making it reachable would need a
    test-only hook, which I chose not to add to production code.
  - A comment on the line says it is unreachable by construction.
  - Every other defensive branch is covered by direct internal calls (#11, #12).
- `cmp` confirms both public test files are identical to their `instrument/` copies.

## 8. Explicit decisions and non-goals

- Version-1 streams that contain tag 3 are rejected, because a v1 decoder rejects them.
- Depth errors match ErrCorrupt from both Decode and Encode [F1]. Encode's size and type errors
  stay outside ErrCorrupt, as in v1.
- There is no stream-size cap on 64-bit. The residual `make` panic above the runtime's allocation
  limit is documented [F4].
- There is no partial-result return and no streaming API. The surface is frozen except for `Batch`.
- Pointer records are not accepted. `*Batch` satisfies `Record` through the value-receiver method set,
  but Encode rejects it, consistent with how v1 treats `*Put` and `*Delete`.
