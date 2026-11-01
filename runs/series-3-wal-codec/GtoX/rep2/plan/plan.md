# Plan: `walcodec` v2 — nested `Batch` records

No prior adversary findings exist at `plan/plan_findings.md`, so this plan does not rebut anything.

The implementation to extend is the converged v1 codec in `snapshot-v1/codec.go`. The phase-2 worktree currently holds only `go.mod` and the public test files; implementation will start from that v1 codec (same package, same signatures) and land in the worktree. This document does not implement anything.

## What stays

Frozen exported surface, names, and signatures:

- `type Record interface { Kind() uint8 }`
- `type Put struct { Key string; Value []byte }` and `func (Put) Kind() uint8` returning `1`
- `type Delete struct { Key string }` and `func (Delete) Kind() uint8` returning `2`
- `func Encode(recs []Record) ([]byte, error)`
- `func Decode(b []byte) ([]Record, error)`
- `var ErrCorrupt = errors.New("walcodec: corrupt stream")`

Unchanged wire rules for every frame, at every depth:

- Frame shape `[len uint32][type uint8][payload][crc uint32]`, all integers big-endian.
- `len` counts `type`+`payload` only; CRC is outside `len`.
- CRC is CRC32-IEEE (`hash/crc32.ChecksumIEEE`) over `type`+`payload`.
- `len` cap is 16 MiB (`16 << 20`). A `len` above the cap, or `len == 0` (no type byte), is `ErrCorrupt` before any allocation of the claimed size.
- `Put` payload is `[klen uint32][key][vlen uint32][value]`. `Delete` payload is `[klen uint32][key]`.
- A `nil` `Value` and an empty `Value` both encode as `vlen = 0` and both decode to a length-0 `[]byte`. They compare equal. Zero-length values decode as `[]byte{}` (non-nil), which matches the existing v1 decoder and the equality rule in the public tests.
- Duplicate keys are kept in order. The codec is a log, not a map.
- `Encode(nil)` and `Encode` of an empty slice write a one-byte version-only stream. Decoding that stream returns an empty slice and no error.
- `Decode(nil)` and `Decode([]byte{})` return `ErrCorrupt` (missing version byte).
- CRC mismatch, unknown type tag, truncated frame, trailing bytes inside a `Put` or `Delete` payload, and a length prefix that runs past its payload are `ErrCorrupt`.
- Decoded keys and values own their bytes (copy out of the input).
- `Encode` still rejects a nil `Record`, a nil `*Put` / `*Delete`, and any type it cannot encode, by returning `ErrCorrupt`. Value and pointer forms of `Put` and `Delete` stay accepted.
- Over-cap records are still rejected from lengths only, before the payload is allocated.
- On any decode failure, `Decode` returns `nil, ErrCorrupt`.
- Package stays `walcodec`. Build with `go build`, clean under `go vet`, and pass `go test ./...` against both public suites.

## What changes

1. Add `type Batch struct { Records []Record }` with `func (Batch) Kind() uint8` returning `3`. Kind constant `kindBatch = 3`.
2. `Encode` writes `version = 2` for every stream, including Put/Delete-only and empty input. That is the allowed behavior change. The v1 public suite never asserts that the written version byte is `1`.
3. `Decode` accepts version `1` and version `2`. Any other version byte, including `0` and `3`, is `ErrCorrupt`, checked before any frame.
4. Version `1` accepts only `Put` (type 1) and `Delete` (type 2). A type-3 frame in a version-1 stream is an unknown type for that version and is `ErrCorrupt`.
5. Version `2` accepts `Put`, `Delete`, and `Batch` (type 3). Any other type tag is `ErrCorrupt`.
6. A `Batch` payload is a framed sub-stream: `[n uint32]` followed by exactly `n` inner frames of the same `[len][type][payload][crc]` shape. The inner frames must consume the payload exactly. Fewer frames, extra frames, or trailing bytes are `ErrCorrupt`.
7. Nesting depth: the top-level stream is depth 0. Each `Batch` decodes and encodes its inner sub-stream at `depth+1`. A sub-stream at depth greater than 8 is `ErrCorrupt`. Eight nested `Batch` levels are legal; a ninth is not. The encoder enforces the same cap and will not emit a stream it would reject.
8. An inner `len` that would read past the enclosing `Batch` payload is `ErrCorrupt`. The decoder never reads into a sibling frame or an outer frame. The same 16 MiB `len` cap applies to every frame at every depth, still checked before allocation.

## Depth convention

The public test `TestBatchNestedPastDepth8` wraps a `Put` in 9 `Batch` frames and expects `ErrCorrupt`.

| Nesting | Sub-stream depth | Result |
| --- | --- | --- |
| top-level frames | 0 | legal |
| 1st `Batch` inner stream | 1 | legal |
| 8th `Batch` inner stream | 8 | legal |
| 9th `Batch` inner stream | 9 | `ErrCorrupt` |

A `Batch` frame that itself sits in a depth-8 sub-stream is well-formed as a frame, but opening its inner sub-stream would be depth 9 and is rejected. The encoder uses the same numbers: `encodeStream` at depth 8 may emit `Put` and `Delete` only; emitting a `Batch` there would require encoding a depth-9 sub-stream, which returns `ErrCorrupt` before any bytes are returned from `Encode`.

## Batch payload

```
Batch payload = n(4) frame_1 frame_2 ... frame_n
frame         = len(4) type(1) payload(len-1) crc(4)
```

- `n` is the count of inner records, not a byte length.
- `n == 0` is a legal empty batch: payload is exactly 4 zero bytes, and it decodes to `Batch{Records: empty slice}`.
- Each inner frame is parsed only from the remaining bytes of this payload. The outer frame's CRC already covers the whole payload; each inner frame still has its own CRC.
- After `n` successful inner frames, zero bytes must remain. A shortfall (claimed `n` not reached, or an inner frame truncated inside the payload) is `ErrCorrupt`.
- `n` that cannot fit even as empty frames inside the already-capped payload is `ErrCorrupt` without allocating `n` records up front. Bound the loop by remaining bytes, and stop with `ErrCorrupt` if the count is not met exactly.

## How each v2 rule is satisfied

**Kind 3 and the new type.** Add `Batch` and `Kind() == 3`. No other exported name or signature changes. `Encode`'s switch accepts `Batch` and `*Batch` (nil pointer → `ErrCorrupt`), matching the existing pointer handling for `Put` and `Delete`.

**Framed sub-stream payload.** `encodeBatchPayload` writes `n = len(Records)` as `uint32`, then one full frame per inner record. `decodeBatch` reads `n`, then decodes exactly `n` frames from the remainder, and requires the remainder to be empty. Inner frames use the same frame parser as the top-level stream, with the enclosing payload as the byte window.

**16 MiB cap at every depth.** The existing `len` check (`n32 < 1 || n32 > maxFrameLen`) stays in the shared frame reader, so it applies to outer and inner frames. A `Batch` frame's own `len` is type plus payload, and that `len` is capped the same way before the payload is sliced or copied. Inner `len` values are checked the same way against `maxFrameLen` and against the remaining enclosing payload.

**Inner `len` must not extend past the enclosing payload.** The frame reader takes an explicit end (the enclosing payload or the outer stream). If `header + len + crc` does not fit in that window, it returns `ErrCorrupt` and does not look past the window. A truncated batch therefore cannot consume the next outer frame.

**Exact consumption.** After `n` inner frames, leftover bytes are `ErrCorrupt`. If the payload ends before `n` frames are complete, that is `ErrCorrupt`. A payload shorter than 4 bytes cannot hold `n` and is `ErrCorrupt`.

**Depth cap 8.** `decodeStream(window, depth, allowBatch)` rejects `depth > 8` before reading frames. Top-level `Decode` calls it at depth 0. A `Batch` calls it at `depth+1`. `Encode` walks the same depths and returns `ErrCorrupt` when a `Batch` would be encoded at depth greater than 8. The encoder therefore never emits the 9-deep stream that the hand-built public test rejects.

**Version 2 on encode.** Replace the written version constant with `2`. Empty input is still one byte: `[]byte{2}`.

**Decode versions 1 and 2 only.** Read the version byte first. If the buffer is empty, `ErrCorrupt`. If the byte is not 1 or 2, `ErrCorrupt` with no frame parse. Version 1 sets `allowBatch = false`. Version 2 sets `allowBatch = true`. A type-3 frame when `allowBatch` is false is `ErrCorrupt`, same as any other unknown tag.

**v1 regression via a fixed fixture, not via replaying `Encode`.** `Decode` of a stream whose first byte is `1` uses the v1 type set and the same frame, CRC, truncation, and oversize checks. The shipped fixture `v1Fixture()` (version byte `0x01`, `Put{"alpha","one"}`, `Delete{"beta"}`) decodes to those records. Flipping its last CRC byte and truncating it both return `ErrCorrupt`. v2 `Encode` is not used to manufacture that fixture. The v1 public suite still passes because it never requires the written version to be 1, and `Decode` still accepts the version-1 bytes that suite builds by hand (`TestUnknownType`, `TestOversizeLen`).

**Encoder will not emit a stream it would reject.** Before appending a frame, check that `1+len(payload)` fits in 16 MiB and in `uint32`, using lengths only for `Put` and `Delete` (existing `putFits` / `deleteFits`). For `Batch`, encode the inner sub-stream first under the depth cap, then check the outer frame size (`type + 4 + inner bytes`) against the same cap before allocating the outer frame. A `Batch` whose framed payload would make `len` exceed 16 MiB returns `ErrCorrupt` and does not return a partial stream. `n` must fit in `uint32` (true for any slice length that fits in the frame cap). Nil records, unknown concrete types, and depth greater than 8 return `ErrCorrupt`.

## Decode and encode shape

Keep one frame parser and one record dispatcher so the cap, CRC, and truncation rules cannot drift between depths.

```
Encode(recs):
  return encodeStream(recs, depth=0) prepended with version 2
  on error return nil, ErrCorrupt

encodeStream(recs, depth):
  if depth > 8: ErrCorrupt
  for each rec:
    Put / *Put, Delete / *Delete: existing fit check, then one frame
    Batch / *Batch:
      if depth+1 > 8: ErrCorrupt
      inner = encodeStream(records, depth+1)
      payload = uint32(len(records)) || inner
      if 1+len(payload) > maxFrameLen: ErrCorrupt
      emit frame type 3
    nil or other type: ErrCorrupt

Decode(b):
  if len(b) < 1: ErrCorrupt
  version = b[0]
  if version == 1: allowBatch = false
  else if version == 2: allowBatch = true
  else: ErrCorrupt
  return decodeStream(b[1:], depth=0, allowBatch)

decodeStream(window, depth, allowBatch):
  if depth > 8: ErrCorrupt
  read frames until window is exhausted
  each frame: cap check, bounds check against this window only, CRC, then dispatch
  type 1 -> Put, type 2 -> Delete
  type 3 -> only if allowBatch; payload must be n plus exactly n frames
            decoded with decodeStream(rest, depth+1, allowBatch)
  other type: ErrCorrupt

decodeBatch(payload, depth, allowBatch):
  need 4 bytes for n
  read n
  rest = payload[4:]
  out = make([]Record, 0)   // not make([]Record, n)
  for i in 0..n-1:
    frame, next, err = readFrame(rest)  // fails if frame extends past rest
    rec = dispatch(frame, depth+1, allowBatch)
    append rec
    rest = next
  if len(rest) != 0: ErrCorrupt
  return Batch{Records: out}
```

`readFrame` returns `ErrCorrupt` when fewer than 4 bytes remain, when `len` is 0 or above 16 MiB, or when the window does not contain `len` plus the 4-byte CRC. It does not allocate a buffer of size `len` for an over-cap or truncated claim; it slices only after both checks pass. Record payloads are still copied into owned key strings and value slices.

Version 1 never passes `allowBatch`, so a nested type 3 is rejected even if someone hand-builds it inside a version-1 stream. Version 2 passes `allowBatch` into nested calls, so a `Batch` may contain `Put`, `Delete`, or `Batch`.

## Public tests, mapped to the plan

Shipped v2 cases in `instrument/walcodec_v2_public_test.go`:

1. `TestBatchRoundTrip` — `Encode` emits version 2 and a type-3 frame whose payload is `n=2` plus a `Put` frame and a `Delete` frame; `Decode` rebuilds `Batch{Records: [...]}`.
2. `TestNestedBatchRoundTrip` — inner `Batch` is a frame inside the outer payload; depth 1 then depth 2, both under the cap.
3. `TestV1FixtureDecodesUnderV2` — `Decode(v1Fixture())` accepts version 1 and returns `Put{"alpha","one"}` and `Delete{"beta"}`.
4. `TestMixedTopLevelRoundTrip` — top-level `Put`, `Delete`, `Batch`, and `Put` with a nil value, in order. Nil value decodes to length 0 and compares equal under `string(x.Value) == string(y.Value)`.
5. `TestTruncatedBatch` — `Decode(b[:len(b)-2])` fails in the shared frame reader (outer CRC truncated, or an inner frame cut by the enclosing window) with `ErrCorrupt`.
6. `TestVersion3Rejected` — `b[0] = 3` fails the version check before frames.
7. `TestBatchNestedPastDepth8` — nine wraps put the innermost sub-stream at depth 9; `decodeStream` returns `ErrCorrupt`. The encoder refuses the same shape, which is why the test builds the bytes by hand.
8. `TestV1BehaviorsViaFixture` — fixture round-trip, last-byte CRC flip, and a 3-byte truncation all go through version-1 decode and return `ErrCorrupt` for the damaged copies.

v1 suite regression, still required:

- Round-trip `Put`/`Delete`, CRC flip, unknown type 99 on a version-1 hand-built stream, truncation, oversize `len` on a version-1 prefix, empty encode (now one byte `0x02`, still length 1), `Decode(nil)` / empty, duplicate keys, nil vs empty value.

## Edge cases the implementation must get right

- Empty `Batch{Records: nil}` and `Batch{Records: []Record{}}` both encode as `n = 0` and decode to a `Batch` whose `Records` length is 0.
- `n` larger than the number of complete frames in the payload, including `n` huge relative to a short payload, is `ErrCorrupt` and does not allocate `n` slots.
- Trailing bytes after `n` well-formed frames are `ErrCorrupt`.
- An inner CRC mismatch is `ErrCorrupt` and does not skip ahead to resync.
- A `Batch` payload whose inner `len` is within 16 MiB but extends past the payload is `ErrCorrupt` (truncation), not a read of the next outer frame.
- An inner `len` above 16 MiB is `ErrCorrupt` before allocation, same as top-level.
- Unknown type inside a version-2 batch is `ErrCorrupt`. Type 3 inside a version-1 stream is `ErrCorrupt`.
- Depth is counted by `Batch` nesting, not by total records. A flat stream of many batches is depth 1 for each inner sub-stream.
- Eight nested batches around a `Put` encode and decode. Nine do not encode, and a hand-built nine is `ErrCorrupt` on decode.
- `Encode` of a tree that fails the depth or size check returns `nil, ErrCorrupt`, not a partial buffer.
- Pointer and value `Batch` values are accepted; a nil `*Batch` is `ErrCorrupt`.
- Keys and values copied out of batch payloads do not alias the input buffer.

## Files

- Bring `snapshot-v1/codec.go` into the worktree as `codec.go` and apply the changes above there. Do not split the package.
- Leave `walcodec_public_test.go` and `walcodec_v2_public_test.go` unchanged.
- Leave `go.mod` (`module walcodec`, `go 1.25`) unchanged.

## Verification (implementation phase, not this phase)

- `go test ./...` in the worktree, which runs both public suites.
- `go vet ./...`
- A small extra check, not required to ship: depth 8 round-trips; depth 9 encode returns `ErrCorrupt`; version 0 is `ErrCorrupt`; a version-1 stream with a type-3 frame is `ErrCorrupt`; a batch with trailing payload bytes is `ErrCorrupt`; an inner `len` past the batch payload is `ErrCorrupt`.
