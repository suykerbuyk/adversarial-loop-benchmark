# Plan: `walcodec` v2 — nested `Batch` records

Amendment: `amendment.md`. Public acceptance: `instrument/walcodec_v2_public_test.go`, run beside the unchanged v1 suite `instrument/walcodec_public_test.go`. Existing code: `snapshot-v1/walcodec.go` (the v1 codec that must be extended). The worktree currently holds only the public tests and `go.mod`; implementation starts from that snapshot.

`plan/plan_findings.md` reports no remaining blocker or major issue. The two earlier defects stay corrected in this plan: a version-1 stream rejects Kind 3, and Batch encode applies the 16 MiB cap before copying a child that would exceed it. This revision also tightens the encode budget so a child that does not fit is not copied at all.

## What stays

Frozen names and signatures:

```go
type Record interface { Kind() uint8 }
type Put struct { Key string; Value []byte } // Kind 1
type Delete struct { Key string }            // Kind 2
func Encode(recs []Record) ([]byte, error)
func Decode(b []byte) ([]Record, error)
var ErrCorrupt = errors.New("walcodec: corrupt stream")
```

Unchanged wire rules and v1 behaviors:

- Frame shape `[len uint32][type uint8][payload][crc uint32]`, big-endian. `len` counts `type`+`payload` only. CRC32-IEEE is over `type`+`payload` and sits outside `len`.
- `len` cap is `16 << 20` at every depth. An oversize `len` is `ErrCorrupt` before any body allocation.
- Put payload `[klen][key][vlen][value]`. Delete payload `[klen][key]`. Trailing payload bytes are `ErrCorrupt`.
- `nil` and empty `Value` both encode as `vlen = 0` and both decode to a non-nil length-0 `[]byte`.
- Duplicate keys are kept, in stream order. The codec is a log, not a map.
- `Encode(nil)` and `Encode([]Record{})` write a one-byte version-only stream. Decoding that stream returns a non-nil empty slice and a nil error.
- `Decode(nil)` and `Decode([]byte{})` are `ErrCorrupt` (missing version byte).
- Truncated frame, CRC mismatch, unknown type tag, and a `len` that would read past the enclosing buffer are `ErrCorrupt`.
- Unknown concrete types and a nil `Record` on encode are `ErrCorrupt`. Encode checks the 16 MiB cap before copying key or value bytes.
- `readCounted` still copies out of the input so decoded keys and values do not alias the caller's buffer.
- Error value stays the single sentinel `ErrCorrupt`. No new exported errors.
- v1 public tests stay in the package and must still pass. They do not assert that `Encode` writes version 1.

## What changes

One exported addition:

```go
type Batch struct { Records []Record } // Kind 3
func (Batch) Kind() uint8              // returns 3
```

`Encode` writes `version = 2` for every successful stream, including Put/Delete-only and empty input. That is the allowed behavior change.

`Decode` accepts version 1 and version 2. Any other version byte, including 0 and 3, is `ErrCorrupt`, checked before any frame is read. The version byte selects the allowed type set for every frame in that stream, including frames nested inside a Batch:

- Version 1: Put and Delete only. Kind 3, and any other tag, is `ErrCorrupt`. The amendment defines a version-1 stream as Put/Delete-only and says `Decode` accepts version 1 (Put/Delete only). That is a decoder rule, not only a producer rule.
- Version 2: Put, Delete, and Batch. Any other tag is `ErrCorrupt`.

Internal additions, not exported:

- `versionV2 = 2`, `kindBatch uint8 = 3`.
- `maxNestDepth = 8`. Depth is the depth of the sub-stream being decoded or encoded. The top-level stream is depth 0. A Batch's inner sub-stream is decoded and encoded at `depth+1`. A sub-stream at depth greater than 8 is `ErrCorrupt`.
- `appendFrame` gains a depth argument and a `Batch` case. It also receives the remaining budget of the enclosing frame so a Batch never copies a child that would make the enclosing payload exceed `maxFrameLen`.
- `decodeStream(b []byte, depth int, allowBatch bool)` replaces the frame loop inside `Decode`. `Decode` validates the version byte, then calls `decodeStream(b[1:], 0, version == 2)`.

## Batch wire format

```
Batch payload = n(uint32) frame_1 frame_2 ... frame_n
frame         = len(4) type(1) payload(len-1) crc(4)
```

- `n` is the count of inner records. Each inner frame is a full frame, same shape as a top-level frame.
- The `n` frames must consume the Batch payload exactly. Too few frames, an inner frame that does not fit, or any trailing byte after `n` frames is `ErrCorrupt`.
- An inner `len` that would extend past the enclosing payload is `ErrCorrupt` (truncated). The decoder uses the payload length as the end bound. It never reads a sibling frame or an outer frame to satisfy an inner `len`.
- A Batch may contain Put, Delete, or nested Batch, but only in a version-2 stream. Recursion uses the same frame codec. A version-1 stream that contains Kind 3 at any depth is `ErrCorrupt`.
- Empty `Batch{Records: nil}` and `Batch{Records: []Record{}}` both encode as `n = 0` with no inner frames, and both decode to a non-nil empty `[]Record`. They compare equal the same way nil and empty Put values do.
- A nil element inside `Batch.Records` is `ErrCorrupt` on encode, same as a nil top-level record.
- An unknown type inside a Batch is `ErrCorrupt`. Under version 1, Kind 3 is unknown for that version and is `ErrCorrupt` even when the CRC and length are valid.
- Inner `n` that does not match the frames present is `ErrCorrupt`, including `n` larger than the bytes can hold and `n` smaller than the frames that fit (trailing bytes).

## Depth cap

Convention used by the grader: top-level stream is depth 0. Each Batch opens a sub-stream one deeper than the Batch's own stream.

| Nesting | Sub-stream depth | Result |
|---|---|---|
| no Batch | 0 | legal |
| 1 Batch | 1 | legal |
| 8 nested Batches | 8 | legal |
| 9 nested Batches | 9 | `ErrCorrupt` |

`TestBatchNestedPastDepth8` wraps a Put in 9 Batch frames under version 2. The Put is decoded only if the 9th sub-stream (depth 9) is entered, so the check runs at the start of `decodeStream` before reading `n` or any inner frame: if `depth > 8`, return `ErrCorrupt`.

The encoder uses the same numbers. `Encode` calls `appendFrame` at depth 0. A Batch encodes its children at `depth+1`. If that depth is greater than 8, encode returns `ErrCorrupt` and does not emit a stream it would reject. Public test 7 builds the illegal stream by hand because the encoder refuses to emit it.

A Batch record itself is legal at depth 8. Its contents are the illegal part, because they would sit at depth 9. The check belongs on the sub-stream, not on the Batch frame's own depth.

## Encode

`Encode` still allocates a small header buffer, writes one version byte (`2`), and appends frames in order. On any error it returns `nil, ErrCorrupt`. No partial stream is returned.

`appendFrame(dst, rec, depth, budget)`:

- `budget` is the maximum number of bytes this call may append for one complete frame (`len` prefix + body + `crc`). Top-level calls pass a budget of at least `maxFrameLen + 8`, which is always enough for one legal frame. A Batch passes each child the remaining room inside its own payload (see below).
- nil record or unknown concrete type → `ErrCorrupt`.
- `Put` and `Delete` keep the current length-before-copy path. `frameLen` rejects overflow and `len > maxFrameLen` without copying key or value bytes. If the resulting frame (`4 + body + 4`) would exceed `budget`, return `ErrCorrupt` before copying. A Put that is legal alone is still rejected, without copying, when it cannot fit in the enclosing Batch.
- `Batch`:
  1. If `depth+1 > 8`, return `ErrCorrupt` before walking children and before any child allocation.
  2. Compute the outer frame overhead before touching children: type byte (1) + count prefix (4) + `len` prefix (4) + CRC (4) = 13 bytes that are not child frames. If `budget < 13`, or if `1+4` already exceeds `maxFrameLen`, return `ErrCorrupt`.
  3. Child payload room is `min(budget-13, maxFrameLen-5)`. `maxFrameLen-5` is the largest payload that still leaves room for the type byte inside the 16 MiB body cap. This room is the running `remain` counter. It starts as a pure integer. No child scratch is allocated yet.
  4. Walk children one at a time. For each child, first ask the child how many frame bytes it needs (`frameSize(child, depth+1)`). That size is computed with the same `frameLen` arithmetic Put and Delete already use, and with the same recursive room arithmetic for a nested Batch. If the child size is unknown, overflows, or `childSize > remain`, return `ErrCorrupt` before allocating a scratch and before copying that child's key, value, or nested frames. Only a child that fits is then appended into a scratch whose capacity was reserved as `remain` (at most `maxFrameLen`). After a successful append, subtract the appended length from `remain`.
  5. Payload length is `4 + bytes written into scratch`. Body length is `1 + payload length`. Both are already within `maxFrameLen` because of the room check. Append `len`, type `3`, `n` as uint32, the scratch frames, and CRC32-IEEE over `type||payload`, only after every child succeeds.
  6. If `len(records) > math.MaxUint32`, return `ErrCorrupt` before writing `n`. A legal body cannot hold that many frames; the check keeps the count prefix from truncating if the type system ever allows it.

A Batch whose children would sum past 16 MiB returns `ErrCorrupt` without copying the child that crosses the cap, and without assembling an unbounded scratch of many 16 MiB frames. Nested Batches use the same room counter, so a rejected deep tree does not retain more than one 16 MiB cap per nesting level that was actually entered, and it does not enter a level past depth 8. That preserves the v1 encoder's check-before-copy behavior for Put and Delete, and it extends the same bound to Batch assembly.

No change to Put/Delete payload layout. A Put/Delete-only `Encode` differs from a v1 encoder only in the leading version byte.

## Decode

```text
Decode(b):
  if len(b) < 1 → ErrCorrupt
  if b[0] is not 1 and not 2 → ErrCorrupt   // before any frame
  return decodeStream(b[1:], 0, b[0] == 2)

decodeStream(b, depth, allowBatch):
  if depth > 8 → ErrCorrupt
  recs := empty non-nil slice
  while bytes remain:
    read one frame with end bound = len(b)
    if type is Batch and allowBatch is false → ErrCorrupt
    append decoded record
  return recs
```

`allowBatch` is false for the whole walk of a version-1 stream, including any payload that would have been a nested sub-stream. A Kind 3 frame is rejected at the type switch before its payload is interpreted as a sub-stream. Version 2 passes `allowBatch = true` at every depth.

Frame read, shared by the top-level stream and every Batch payload:

1. Need 4 bytes for `len`. Otherwise `ErrCorrupt`.
2. If `len < 1` or `len > 16<<20`, return `ErrCorrupt` before allocating the body.
3. Compute `bodyEnd` and `crcEnd` in a width that cannot wrap (`int` checks against the enclosing length, reject if `crcEnd` would pass the enclosing end or overflow). Passing the enclosing end is truncation, including an inner `len` that extends past its Batch payload.
4. CRC the body. Mismatch → `ErrCorrupt`.
5. Dispatch on type. Put and Delete payloads must be consumed exactly, as in v1. Batch is legal only when `allowBatch` is true; otherwise `ErrCorrupt`. A legal Batch payload goes to the Batch decoder, which calls `decodeStream` on the inner bytes with the same `allowBatch` flag and `depth+1`.
6. Advance to `crcEnd`. Bytes left are the next sibling, or trailing junk if this read was inside a counted Batch.

Batch payload:

1. Need 4 bytes for `n`. Otherwise `ErrCorrupt`. This path is reached only for a version-2 Kind 3 frame that already passed CRC and the length cap.
2. `decodeStream` on the bytes after `n`, at `depth+1`, with the same `allowBatch` value. The sub-stream function rejects depth greater than 8 immediately.
3. The sub-stream must return exactly `n` records and consume every remaining payload byte. Implement that by decoding until the payload is exhausted, then comparing `len(records)` to `n`. A short payload fails inside the frame reader (truncated). Extra frames that still fit are decoded and then rejected because the count does not match. Trailing bytes that are not a full frame fail as truncation. Both are `ErrCorrupt`.
4. Do not allocate `n` slots up front. A hostile `n` (for example `0xFFFFFFFF`) with a short payload must fail on the frame bound, not by allocating `n` records. Depth is rejected before that allocation as well.

Version 1 and version 2 share the frame reader and the Put/Delete payload parsers. They do not share the type set. The v1 fixture begins with `0x01` and contains Put `alpha`/`one` then Delete `beta`. `Decode` must return those two records. Flipping the fixture's last byte and truncating its last three bytes must both return `ErrCorrupt`. Those checks are public tests 3 and 8. Do not implement the regression by encoding with this package and rewriting byte 0 to 1.

A hand-built version-1 stream whose only difference from a legal v2 stream is a Kind 3 frame must return `ErrCorrupt`. That includes a Batch at the top level and a Batch nested inside another Batch if a version-1 decoder were ever to open one. Rejection happens at the type tag, after CRC, so a corrupt CRC still fails as a CRC error and an oversize `len` still fails before allocation.

`TestTruncatedBatch` encodes a Batch and drops the last two bytes. That cuts the outer CRC, so the existing truncated-frame check already returns `ErrCorrupt`. The inner-bound rule still has to be implemented: an inner `len` that is in range of the outer buffer but past the Batch payload must stop at the payload end.

`TestVersion3Rejected` rewrites byte 0 of a successful encode to 3. Rejection is the version check alone.

## v1 regression

Keep the v1 helpers and their allocation boundaries:

- `frameLen` rejects overflow and `len > maxFrameLen` without allocating.
- `readCounted` still returns a fresh slice, including a non-nil empty slice for a zero length.
- CRC, unknown type, truncation, oversize `len`, empty input, nil input, duplicate keys, and nil-versus-empty Value stay as specified in `spec-v1.md`.
- v1 tests call `Encode` and then `Decode` on that output. They tolerate version 2. `TestUnknownType` builds its own version-1 stream with type 99; the version check must still accept 1, and the type switch must still reject 99. Kind 3 on that same version-1 path is rejected the same way.
- `TestOversizeLen` builds a version-1 stream whose first frame `len` is `16<<20+1`. Reject that before allocating a body of that size. Same check on an inner frame.

## How each v2 rule is satisfied

| Rule | Mechanism |
|---|---|
| `Batch{Records []Record}`, Kind 3 | Exported struct. `Kind()` returns 3. |
| Payload is `n` plus `n` full frames | Encoder writes count then child frames. Decoder reads count, then frames, and requires an exact match. |
| Batch may contain Put, Delete, or Batch | Version-2 `appendFrame` / `decodeRecord` switch includes all three. Nested calls pass `depth+1`. Version 1 rejects Kind 3. |
| 16 MiB cap at every depth | Same `maxFrameLen` check in the shared frame reader and in the encoder, before allocation or payload copy. Batch child copies are charged against a running room counter capped at `maxFrameLen`. A child that would exceed the remaining room is rejected before its bytes are copied. |
| Inner `len` past enclosing payload is truncated `ErrCorrupt` | Frame reader end bound is the current buffer, which for a Batch is the payload slice, not the outer stream. |
| Depth cap 8; 9th sub-stream is `ErrCorrupt` | `decodeStream` and Batch encode reject `depth > 8`. Top-level is 0. |
| Encoder will not emit a stream it would reject | Encode uses the same depth, version, and length checks and returns `nil, ErrCorrupt` instead of a partial buffer. |
| `Encode` writes version 2 | Leading byte is `versionV2`. |
| `Decode` accepts 1 and 2 only | Version check before the frame loop. 0 and 3 fail. |
| Version 1 is Put/Delete only | `allowBatch` is false for a version-1 stream. Kind 3 at any depth is `ErrCorrupt`. |
| v1 decode behaviors hold on a fixed fixture | Tests 3 and 8 use `v1Fixture()`, not `Encode` output. Decoder accepts version 1 and the existing frame checks. |
| Frozen v1 surface | No signature changes. Only new type is `Batch`. |
| Put/Delete-only encode may start with 2 | Documented allowed change. v1 tests do not read the version of `Encode` output. |

## Public tests (must pass, not modified)

1. `TestBatchRoundTrip` — one Batch of Put and Delete.
2. `TestNestedBatchRoundTrip` — Batch inside Batch, plus a sibling Delete.
3. `TestV1FixtureDecodesUnderV2` — literal version-1 bytes, Put alpha/one then Delete beta.
4. `TestMixedTopLevelRoundTrip` — Put, Delete, Batch, Put with a nil value.
5. `TestTruncatedBatch` — encoded Batch with the last two bytes dropped → `ErrCorrupt`.
6. `TestVersion3Rejected` — encoded stream with byte 0 set to 3 → `ErrCorrupt`.
7. `TestBatchNestedPastDepth8` — hand-built version-2 stream, Put wrapped in 9 Batches → `ErrCorrupt`.
8. `TestV1BehaviorsViaFixture` — fixture round-trip, last CRC byte flipped, last three bytes dropped.

v1 suite, unchanged, must also pass: round-trip, CRC flip, unknown type on a version-1 hand-built frame, truncation, oversize `len`, empty encode, `Decode(nil)` and `Decode([]byte{})`, duplicate keys, nil versus empty Value.

## Test approach

Run, from `worktree`:

```text
go test ./...
go vet ./...
go build ./...
```

The shipped suites are the acceptance tests. While implementing, a private scratch test (not added to the package unless needed to lock a rule the public tests miss) should cover:

- Eight nested Batches round-trip; the hand-built nine-deep stream fails.
- Empty Batch round-trip, including nil versus empty `Records` decoding equal.
- Batch payload whose inner `len` is legal relative to the outer buffer but past the Batch payload → `ErrCorrupt`, and the outer sibling is not consumed.
- `n` too small (trailing well-formed inner frame) and `n` too large (payload ends early) → `ErrCorrupt`.
- Version 0 rejected before frames.
- Encode of a nine-deep Batch returns `ErrCorrupt` and a nil buffer.
- Batch body over 16 MiB rejected on encode. The rejection happens when the next child would exceed the remaining payload room, and that child is not copied.
- Unknown type and bad CRC on an inner frame → `ErrCorrupt`.
- Version-1 hand-built stream that is only Put/Delete still decodes (already in v1 tests and the fixture).
- Version-1 hand-built stream with a valid Kind 3 frame → `ErrCorrupt`. A version-2 stream with the same frame decodes.
- Encode of many small records whose summed Batch payload exceeds 16 MiB returns `ErrCorrupt` and a nil buffer.

## File layout

Single file `worktree/walcodec.go`, same package, same style as the snapshot: small helpers, no new dependencies, `ErrCorrupt` as the only error. `go.mod` stays `module walcodec` / `go 1.25`.

Do not edit `instrument/` tests. Copy or keep the public test files already in `worktree/` so `go test ./...` runs both suites.

## Implementation order

1. Add `Batch`, `kindBatch`, `versionV2`, `maxNestDepth`. Point `Encode` at version 2.
2. Thread `depth` and a remaining-byte budget through `appendFrame`. Add a size-before-copy helper. Add the Batch encode path with the depth check, the running room counter, and the outer `len` cap. Fail before copying a child that would exceed the enclosing payload.
3. Split the frame loop into `decodeStream`. Accept versions 1 and 2. Pass `allowBatch` from the version byte. Add Batch decode with exact consumption and the enclosing-payload bound. Reject Kind 3 when `allowBatch` is false.
4. Run `go test`, `go vet`, and `go build`. Fix any v1 regression before adding scratch checks for depth, `n`, inner truncation, version-1 Kind 3, and the Batch size budget.
