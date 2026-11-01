# Plan: `walcodec` v2 — nested `Batch` records

Source of truth for the existing implementation is `snapshot-v1/walcodec.go` (the v1 package). The worktree currently holds `go.mod` and the v1/v2 public tests and does not yet contain `walcodec.go`. Implementation (a later phase) copies that v1 file into the worktree and applies the edits below. This phase writes the plan only.

No `plan/plan_findings.md` is present, so there is no rebuttal.

## What stays

Frozen names and signatures, unchanged:

- `type Record interface { Kind() uint8 }`
- `type Put struct { Key string; Value []byte }` and `func (Put) Kind() uint8`
- `type Delete struct { Key string }` and `func (Delete) Kind() uint8`
- `func Encode(recs []Record) ([]byte, error)`
- `func Decode(b []byte) ([]Record, error)`
- `var ErrCorrupt = errors.New("walcodec: corrupt stream")`

Unchanged wire rules (every frame, every depth):

- Frame shape `[len uint32][type uint8][payload][crc uint32]`, big-endian.
- `len` counts `type`+`payload` only. `crc` is CRC32-IEEE over `type`+`payload` and sits outside `len`.
- `len` cap is `16 << 20` (16 MiB), inclusive. A larger `len` is `ErrCorrupt` before any allocation sized from that `len`. `len == 0` stays `ErrCorrupt` (a frame always includes the type byte).
- `Put` payload `[klen uint32][key][vlen uint32][value]`. `Delete` payload `[klen uint32][key]`.
- `nil` and empty `Value` both encode as `vlen = 0` and both decode to a non-nil length-0 `[]byte`.
- Duplicate keys are kept in order. Empty `recs` encodes as a one-byte version-only stream and decodes to an empty slice. `Decode(nil)` and `Decode([]byte{})` are `ErrCorrupt`.
- On any corruption, `Decode` returns `(nil, ErrCorrupt)` and no partial record list.
- Encode still rejects a nil record (including a typed nil `*Put`, `*Delete`, or `*Batch`), an unknown concrete type, a length that does not fit in `uint32`, and a frame whose `type`+`payload` exceeds the 16 MiB cap. Those errors remain ordinary `error` values, not `ErrCorrupt`.

## What changes

All edits land in `walcodec.go` (worktree copy of the v1 file). No test files are rewritten. `go.mod` stays `module walcodec`.

### New type

```go
type Batch struct { Records []Record } // Kind 3

func (Batch) Kind() uint8 { return 3 }
```

Value receiver, same as `Put` and `Delete`, so `Batch` and `*Batch` both satisfy `Record`.

### Version constants

- `Encode` writes `version = 2` (constant `versionV2 = 2`). The v1 constant remains so `Decode` can still accept it.
- `Decode` accepts version `1` or `2` and rejects every other byte, including `0` and `3`, before reading any frame.

### Shared frame reader, bounded to a window

Replace the top-level cursor loop with one function that decodes a framed sub-stream inside a caller-supplied window `[off, end)`:

```text
decodeFrames(b []byte, off, end, depth int) ([]Record, error)
```

- The window is a slice range of the caller buffer. No copy of a claimed `len` is made before the cap check.
- Each frame is read only from bytes still inside `[off, end)`. An inner `len` whose body or CRC would extend past `end` is `ErrCorrupt` (truncated). The reader never advances past `end`, so it cannot consume a sibling frame or an outer frame's CRC.
- After the version check, the top-level call is `decodeFrames(b, 1, len(b), 0)`.
- `Put` and `Delete` payload parsing stays in `decodePut` / `decodeDelete`. Those already refuse a length prefix that runs past the payload they were given, and they require the prefixes to consume the payload exactly (trailing bytes are `ErrCorrupt`).

### Batch payload

Kind `3`. Payload:

```text
n uint32  frame_1  frame_2  ...  frame_n
```

Each inner frame is a full `[len][type][payload][crc]` frame, same cap and CRC rule as the outer stream.

Decode rules for a Batch payload of length `P`:

1. Need 4 bytes for `n`. Short payload → `ErrCorrupt`.
2. Decode exactly `n` frames from the remaining `P-4` bytes by calling `decodeFrames` on that window at `depth+1`.
3. Those `n` frames must consume the window exactly. Too few bytes for `n` frames, an inner `len` past the window, extra trailing bytes after `n` frames, or a count that does not match the frames present → `ErrCorrupt`.
4. Inner type tags: `1` Put, `2` Delete, `3` Batch. Any other tag → `ErrCorrupt`.
5. A version-1 stream that contains type `3` is `ErrCorrupt` (v1 is Put/Delete only). Version 2 accepts all three tags. The allowed-tag set is a parameter of `decodeFrames` (or a version flag), not a second parser.

Encode of `Batch`:

- Encode each inner record to a frame (no version byte) at `depth+1`, then prefix `n = uint32(len(Records))`.
- `n` fits in `uint32` because a legal payload is capped at 16 MiB, so the record count cannot exceed that.
- A nil element inside `Records` is the same error as a nil top-level element.
- `*Batch` is accepted the same way `*Put` and `*Delete` are accepted today, including a typed-nil check.
- Empty `Batch{Records: nil}` and `Batch{Records: []Record{}}` both encode as `n = 0` and decode as `Batch` with an empty `Records` slice.

### Depth cap

Convention used by the grader: the top-level stream is depth 0. A Batch's inner sub-stream is decoded (and encoded) one level deeper than the Batch frame itself.

- Entering `decodeFrames` / the encode walker with `depth > 8` returns `ErrCorrupt` on decode and a non-nil error on encode. The encoder does not emit a stream it would reject.
- Eight nested Batches are legal: outermost Batch frame is in the depth-0 stream; its payload is decoded at depth 1; the eighth Batch's payload is decoded at depth 8. A ninth wrap would ask for a sub-stream at depth 9 and is rejected.
- This matches public test 7, which wraps a Put in 9 Batch frames under version 2 and expects `ErrCorrupt`.
- Depth is a recursion counter, not a function of byte offsets. A wide stream of sibling Batches stays at the same depth.

### Version-1 regression

- `Decode` of a stream whose first byte is `1` uses the v1 tag set (Put, Delete only) and the same CRC, truncation, oversize, empty, and nil-value rules as v1.
- Acceptance of v1 is checked against the fixed fixture in `walcodec_v2_public_test.go` (`v1Fixture`, version byte `0x01`, records `Put{"alpha","one"}` then `Delete{"beta"}`), not by inspecting `Encode`'s first byte.
- `Encode` of Put/Delete-only input writes version `2`. That is the allowed behavior change. The v1 public suite never asserts that the written version byte is `1`.

## How each v2 rule is satisfied

| Rule | How |
| --- | --- |
| `Batch{Records []Record}`, Kind 3 | New struct and `Kind()` returning `3`. |
| Payload is `n` plus `n` inner frames of the same frame shape | `batchPayload` / `decodeBatch` as specified above. |
| Inner frames consume the Batch payload exactly; trailing bytes or a short count is `ErrCorrupt` | `decodeFrames` returns the end offset; `decodeBatch` requires that offset to equal the payload end and the decoded count to equal `n`. |
| Inner `len` past the enclosing payload is truncated `ErrCorrupt`, never a read into the next outer frame | Every read is bounded by the window `end`. The outer CRC is outside that window. |
| 16 MiB `len` cap at every depth | `decodeFrames` applies `maxFrameLen` before allocation at every depth. Encode refuses an oversize frame before writing it, including a Batch whose nested payload exceeds the cap. |
| Depth cap 8; a 9th nesting level is `ErrCorrupt`; encoder enforces the same cap | `depth` argument; reject `depth > 8` on both paths. |
| `Encode` writes version 2 and may include Batches | Version byte constant flipped to 2; Batch is a first-class case in the encode switch. |
| `Decode` accepts version 1 (Put/Delete only) and version 2 (all three); any other version is `ErrCorrupt` before any frame | Version switch at the start of `Decode`, before `decodeFrames`. |
| Frozen v1 symbols | No renames, no signature changes. `ErrCorrupt` text stays `walcodec: corrupt stream`. |
| v1 decode behaviors hold on a fixed v1-bytes fixture | Version-1 path reuses the same frame and payload checks. Public tests 3 and 8 drive the fixture, a CRC flip of its last byte, and a 3-byte truncation. |

## Public tests, mapped to the implementation

1. `TestBatchRoundTrip` — encode/decode one Batch of Put+Delete.
2. `TestNestedBatchRoundTrip` — Batch containing a Batch and a Delete.
3. `TestV1FixtureDecodesUnderV2` — `Decode(v1Fixture())` equals `Put{"alpha","one"}`, `Delete{"beta"}`.
4. `TestMixedTopLevelRoundTrip` — Put, Delete, Batch, Put with a nil Value, in that order. Nil Value decodes as length 0.
5. `TestTruncatedBatch` — drop the last 2 bytes of an encoded Batch stream → `ErrCorrupt`.
6. `TestVersion3Rejected` — encoded stream with byte 0 set to 3 → `ErrCorrupt` before frames.
7. `TestBatchNestedPastDepth8` — hand-built version-2 stream, Put wrapped in 9 Batch frames → `ErrCorrupt`. Eight wraps must still succeed (encoder and decoder); the plan's depth check is `depth > 8`, so depth 8 is accepted.
8. `TestV1BehaviorsViaFixture` — fixture round-trip, last-byte CRC flip, and truncation of the fixture → `ErrCorrupt`.

The v1 public suite stays green because it never requires the written version to be 1, and `Decode` still accepts version 1 (used by `TestUnknownType` and `TestOversizeLen`, which build streams by hand with version byte `1`).

## Implementation steps (later phase; not done here)

1. Copy `snapshot-v1/walcodec.go` into the worktree package.
2. Add `Batch`, kind constant `3`, and version constant `2`. Point `Encode`'s version byte at `2`.
3. Extract `decodeFrames(b, off, end, depth, allowBatch)` from the current `Decode` loop. Top-level `Decode` checks the version byte, chooses `allowBatch`, and calls it at depth 0.
4. Add `decodeBatch`: read `n`, recurse at `depth+1` on the payload window, require an exact consume of `n` frames.
5. Thread `depth` through encode. Encode a Batch as `n` plus inner frames. Refuse `depth > 8` and an oversize Batch payload with the same cap check `appendFrame` already uses.
6. Accept `Batch` and non-nil `*Batch` in the encode type switch, matching the existing pointer handling for Put and Delete.
7. Run `go test ./...` and `go vet ./...` in the worktree. Both public suites must pass. Do not edit the shipped tests.

## Edge cases the implementation will honor

- Batch payload shorter than 4 bytes, or `n` larger than the frames that fit: `ErrCorrupt`.
- `n` smaller than the number of well-formed frames that fit, leaving trailing bytes: `ErrCorrupt`.
- Inner CRC mismatch, unknown inner type, or inner oversize `len`: `ErrCorrupt`, same as top-level.
- Version 1 stream containing a type-3 frame with a valid CRC: `ErrCorrupt`.
- Nested Batch whose inner `len` is sized to spill into the outer frame's CRC: `ErrCorrupt`, and the outer CRC bytes are not interpreted as an inner frame.
- Encode of 9 nested Batches: error, no bytes returned. Encode of 8 nested Batches: success; `Decode` accepts that output.
- A Batch frame that itself exceeds 16 MiB because its children are large: encode error; a hand-built frame with `len > 16<<20` is `ErrCorrupt` with no allocation of that size.
