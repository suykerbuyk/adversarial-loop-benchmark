# Plan: `walcodec` v2 — nested `Batch` records

Amendment source: `amendment.md`. Existing code: `snapshot-v1/walcodec.go` (the worktree currently holds tests and `go.mod` only; implementation starts from that snapshot). No `plan/plan_findings.md` exists, so there is nothing to rebut.

Every v1 rule stays in force except the version byte and the new `Batch` type. Public v1 tests (`walcodec_public_test.go`) and public v2 tests (`walcodec_v2_public_test.go`) both run in package `walcodec`.

## What stays

- Package name `walcodec`.
- Exported names and signatures, unchanged:
  - `type Record interface { Kind() uint8 }`
  - `type Put struct { Key string; Value []byte }` with `Kind() == 1`
  - `type Delete struct { Key string }` with `Kind() == 2`
  - `func Encode(recs []Record) ([]byte, error)`
  - `func Decode(b []byte) ([]Record, error)`
  - `var ErrCorrupt = errors.New("walcodec: corrupt stream")` — every rejected stream returns this value, not wrapped, and the slice is `nil`.
- Wire frame shape, big-endian, unchanged at every depth:
  - `stream = version(1) frame*`
  - `frame = len(4) type(1) payload(len-1) crc(4)`
  - `len` counts `type`+`payload` only. CRC32-IEEE (`hash/crc32.ChecksumIEEE`) covers `type`+`payload` and sits outside `len`.
- Inclusive `len` cap `16 << 20` (16 MiB), checked before any allocation sized from `len`. A `len` of 0 is still corrupt (a frame needs a type byte).
- Put payload `[klen uint32][key][vlen uint32][value]` and Delete payload `[klen uint32][key]`, exact consumption, no trailing bytes.
- `nil` and empty `Value` both encode as `vlen = 0` and both decode to a length-0 `[]byte`.
- Duplicate keys kept in stream order. The codec remains a log, not a map.
- Empty input encodes as a one-byte version-only stream. Decoding that returns a non-nil empty slice and a nil error. `Decode(nil)` and `Decode([]byte{})` stay `ErrCorrupt`.
- Encode error policy from v1, kept distinct from `ErrCorrupt`:
  - `encodeErr` (`walcodec: unknown record`) for a nil interface, a nil `*Put` / `*Delete` / `*Batch`, or a foreign `Record`.
  - `errTooLarge` (`walcodec: record exceeds size limit`) when a key, value, or `type`+`payload` body does not fit in `uint32` or exceeds 16 MiB.
  - On any encode error the returned slice is `nil`.
- Bounds arithmetic stays in `uint64` before any slice. `int(klen)` is never used as a length comparison. Frame cursor advances in `int` only after the cap and remaining-length checks prove the add fits.
- Value and key bytes are copied out of the input buffer on decode (`takeBytes` / `takeString`). Callers can reuse or drop the stream.

## What changes

### New type

```go
type Batch struct { Records []Record } // Kind 3
func (Batch) Kind() uint8 { return 3 }
```

`*Batch` is accepted by `Encode` the same way `*Put` and `*Delete` are. A nil `*Batch` is `encodeErr`. A `Batch` whose `Records` is nil and a `Batch` whose `Records` is empty both encode as `n = 0` with no inner frames, and both decode as `Batch{Records: non-nil empty slice}` so length comparisons treat them as equal (same convention as nil vs empty `Value`).

### Batch payload

```
Batch payload = n(4) frame_1 frame_2 ... frame_n
```

Each inner frame is a full `[len][type][payload][crc]` frame of the same shape as a top-level frame. Inner records may be `Put`, `Delete`, or nested `Batch`.

The `n` frames must consume the payload exactly. Fewer well-formed frames than `n`, more bytes than `n` frames, or any trailing byte after the `n`th frame is `ErrCorrupt`. `n = 0` is legal only when the payload is exactly 4 bytes.

### Version

- `Encode` writes `version = 2` for every stream, including Put/Delete-only input and empty input. A one-byte stream is now `{2}`.
- `Decode` accepts version `1` and version `2`, validated before any frame. Any other byte, including `0` and `3`, is `ErrCorrupt`.
- Version 1 may contain only `Put` (type 1) and `Delete` (type 2). A type-3 frame in a version-1 stream is `ErrCorrupt` (unknown type for that version).
- Version 2 may contain types 1, 2, and 3. Any other type tag is `ErrCorrupt`.

This is the allowed behavior change called out in the amendment. v1 public tests do not assert the written version byte, and they decode whatever `Encode` emits, so they still pass.

### Depth

Decoding and encoding carry a nesting depth. The top-level stream is depth 0. A `Batch` frame that itself sits at depth `d` has an inner sub-stream decoded (and encoded) at depth `d+1`. A sub-stream at depth greater than 8 is `ErrCorrupt`. Equivalently, 8 levels of `Batch` wrapping are legal; a 9th is rejected.

Concrete mapping used by the public test `TestBatchNestedPastDepth8` (a Put wrapped in 9 Batch frames, version byte 2):

| Batch level | Batch frame depth | Inner sub-stream depth | Result |
|---|---|---|---|
| 1st (outermost) | 0 | 1 | ok |
| 8th | 7 | 8 | ok |
| 9th | 8 | 9 | `ErrCorrupt` |

A stream of exactly 8 nested Batches around a Put must decode. The encoder refuses to emit a stream it would reject: `Encode` returns an error (not a partial buffer) if the input would place a sub-stream at depth greater than 8. That encode error is not `ErrCorrupt` (the input is caller data, not a corrupt stream). Use a dedicated sentinel, for example `errTooDeep = errors.New("walcodec: batch nesting exceeds depth 8")`, so it is not confused with `encodeErr` or `errTooLarge`.

Depth is checked when entering a Batch sub-stream, before parsing inner frames. A Batch whose own frame is at depth 8 is itself a legal record at that depth; opening its payload would be depth 9 and is rejected even if `n = 0`.

### Shared frame walker

Replace the single top-level loop with one walker used at every depth:

```
decodeFrames(buf, depth, limitTypes) -> []Record
```

- `Decode` checks `len(b) >= 1`, then version in `{1, 2}`, then `decodeFrames(b[1:], 0, version)`.
- For each frame, in order, before any allocation sized from `len`:
  1. Need 4 bytes for `len`, else truncated → `ErrCorrupt`.
  2. `frameLen > 16<<20` or `frameLen < 1` → `ErrCorrupt`.
  3. Remaining bytes after the `len` field must be at least `uint64(frameLen)+4`, compared in `uint64`. Short → `ErrCorrupt`. This is the rule that an inner `len` which extends past its enclosing payload never reads a sibling or outer frame: the walker is given only the enclosing payload (or the top-level tail), so the remaining-length check is against that slice alone.
  4. CRC32-IEEE over `type`+`payload` must match. Mismatch → `ErrCorrupt`.
  5. Type must be allowed for this version. Version 1: 1 or 2. Version 2: 1, 2, or 3. Else `ErrCorrupt`.
  6. Put and Delete payloads use the existing exact-consume `takeLen` path (uint64 length compare, copy out).
  7. Batch payload: need 4 bytes for `n`; if `depth+1 > 8` → `ErrCorrupt`; then `decodeFrames` on the bytes after `n`, at `depth+1`. The returned count must equal `n`, and those frames must consume that slice exactly (the walker already consumes its buffer exactly, so a length mismatch or a short inner frame fails inside the recursive call or at the count check).
- Cursor steps with `i = bodyEnd + 4` in `int` only after steps 2–3 prove `frameLen+4` fits.
- Every failure path returns `(nil, ErrCorrupt)` from `Decode`. No partial record slice is returned.

Encoding mirrors this:

```
encodeRecord(rec, depth) -> frame bytes
```

- `Encode` writes `{2}`, then each top-level record at depth 0.
- Put and Delete framing stays as in v1 (uint64 body size, one buffer, CRC over `type`+`payload`).
- Batch: if `depth+1 > 8`, return `errTooDeep` and `Encode` returns `(nil, errTooDeep)`. Otherwise encode each inner record at `depth+1`, prefix with `n` as uint32, then wrap that payload in one outer frame of type 3. `n` is `len(Records)`. A count that does not fit in `uint32` is `errTooLarge` (compare in `uint64` before the conversion). The Batch body size (`1 + 4 + sum of inner frame lengths`) is accumulated in `uint64` and rejected with `errTooLarge` if it exceeds 16 MiB or `^uint32(0)`, before allocation. Inner frames are encoded first into a temporary buffer only after each inner encode succeeds; any inner error aborts the whole `Encode` with a nil slice.
- Empty `Records` is a 4-byte `n = 0` payload, body length 5, which is under the cap.

### v1 regression fixture

Do not derive v1 compatibility by inspecting `Encode`'s first byte. `Decode` must accept the fixed fixture in the v2 public test:

```
01 00 00 00 11 01 00 00 00 05 61 6c 70 68 61 00 00 00 03 6f 6e 65 20 74 5b 79
00 00 00 09 02 00 00 00 04 62 65 74 61 93 aa 41 97
```

That is version 1, then `Put{"alpha","one"}`, then `Delete{"beta"}`. CRC flip of the last byte and a 3-byte truncation of that fixture are `ErrCorrupt`.

## How each v2 rule is satisfied

1. **`Batch{Records []Record}`, Kind 3.** New struct and `Kind()` method. Payload is a framed sub-stream, not a flat concatenation of payloads.
2. **Payload = `n uint32` plus `n` full frames.** Encoder writes that layout. Decoder reads `n`, then exactly `n` frames, and requires the payload to be fully consumed.
3. **Put, Delete, or nested Batch inside a Batch.** The recursive walker accepts all three types when version is 2. Version 1 never reaches a Batch payload.
4. **Same 16 MiB `len` cap at every depth.** One constant, checked in the shared walker before allocation, for outer and inner frames.
5. **Inner `len` past the enclosing payload is truncated `ErrCorrupt`.** The walker receives only the enclosing payload slice. The remaining-length check cannot see sibling or outer bytes.
6. **Depth cap 8, top-level is depth 0.** Entering a Batch sub-stream uses `depth+1`. `depth+1 > 8` is `ErrCorrupt` on decode and `errTooDeep` on encode. Nine wraps fail; eight wraps succeed.
7. **Encoder will not emit a stream it would reject.** Depth and size are enforced on the way in. `Encode` returns a nil slice on any error.
8. **`Encode` writes version 2.** First byte is the constant `2`, including empty input.
9. **`Decode` accepts version 1 and version 2 only.** Checked before frames. Version 3, 0, and any other byte are `ErrCorrupt`.
10. **Frozen v1 surface.** No rename, no signature change. `Batch` is the only addition.
11. **v1 decode behaviors on a fixed version-1 fixture.** Version 1 path uses the same CRC, truncation, oversize, unknown-type, and exact-payload checks. The fixture is accepted as `Put{"alpha","one"}` then `Delete{"beta"}`.

## Public tests, mapped

| Test | What the implementation must do |
|---|---|
| `TestBatchRoundTrip` | Encode version 2; decode a Batch of Put+Delete back to equal records. |
| `TestNestedBatchRoundTrip` | Batch-in-Batch round-trips, depth 2, well under the cap. |
| `TestV1FixtureDecodesUnderV2` | `Decode` of the literal version-1 bytes returns the two fixture records. |
| `TestMixedTopLevelRoundTrip` | Put, Delete, Batch, Put (nil value) round-trip. Nil value decodes to length 0. |
| `TestTruncatedBatch` | Dropping the last 2 bytes of an encoded Batch stream is `ErrCorrupt` (CRC or length short). |
| `TestVersion3Rejected` | Patching byte 0 to 3 on an otherwise valid encode is `ErrCorrupt` before frames. |
| `TestBatchNestedPastDepth8` | Hand-built version-2 stream of 9 nested Batch frames around a Put is `ErrCorrupt`. |
| `TestV1BehaviorsViaFixture` | Fixture round-trips; last-byte CRC flip is `ErrCorrupt`; `fixture[:len-3]` is `ErrCorrupt`. |
| v1 suite | Still passes with version 2 on the wire. `TestUnknownType` builds a version-1 stream with type 99 and must still be `ErrCorrupt`. `TestOversizeLen` uses version 1 and `len = 16<<20+1`. `TestEmptyRecsVersionOnly` only checks length 1 and an empty decode. |

## Edge cases the plan commits to

- Trailing bytes after `n` well-formed inner frames, or `n` larger than the number of frames that fit, is `ErrCorrupt`.
- A Batch `len` that claims bytes past the outer frame is `ErrCorrupt` at the outer frame check, before the inner walk.
- An inner CRC mismatch is `ErrCorrupt` for the whole `Decode`, with a nil slice.
- Unknown type inside a Batch is `ErrCorrupt`.
- Oversize inner `len` (`> 16 MiB`) is `ErrCorrupt` with no allocation sized from that `len`.
- `n` whose following frames would need a read past the Batch payload is `ErrCorrupt`, not a read into the next outer frame.
- Eight nested Batches around a Put decode successfully. The 9th nesting level does not.
- Encode of a 9-deep Batch tree returns `(nil, errTooDeep)` and does not write version 2 plus a partial tree.
- A Batch whose encoded body would exceed 16 MiB returns `(nil, errTooLarge)`.
- `Encode(nil)` is `[]byte{2}, nil`. `Decode([]byte{2})` is a non-nil empty slice, nil error. `Decode([]byte{1})` is the same (version-1 empty stream).
- Interface nil, nil `*Batch`, and a type that is not Put, Delete, or Batch remain encode errors, not `ErrCorrupt`.

## Files

- Restore and edit `worktree/walcodec.go` from `snapshot-v1/walcodec.go`.
- Leave both public test files unchanged.
- `go.mod` stays `module walcodec` / `go 1.25`.

## Verification (implementation phase, not this plan)

- `go test ./...` in `worktree` must pass both public suites.
- `go vet ./...` must be clean.
- Extra local checks, not shipped as new test files unless asked: exactly 8 nested Batches round-trip; 9-deep `Encode` errors with a nil slice; version 0 rejected; type 3 inside a version-1 stream rejected; inner `len` longer than the Batch payload rejected; trailing byte after `n` frames rejected; `n` smaller than the frames present rejected; empty Batch round-trip; nil vs empty `Records` decode equal.
