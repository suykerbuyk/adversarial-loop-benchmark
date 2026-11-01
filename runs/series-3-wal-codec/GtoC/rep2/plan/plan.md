# Plan: `walcodec` v2 — nested `Batch` records

This plan extends the converged v1 codec. It does not implement anything. It answers the findings in `plan/plan_findings.md`; the point-by-point response is `plan/plan_rebuttal.md`.

Source of truth: `amendment.md`, `instrument/walcodec_v2_public_test.go`, and the v1 package in `snapshot-v1/` (`record.go`, `encode.go`, `decode.go`). The worktree currently holds only the public tests and `go.mod`. Implementation starts by bringing the v1 files into the package and amending them as specified below.

## What stays

Frozen exported names and signatures. Do not rename, retype, or remove any of these:

```go
type Record interface { Kind() uint8 }

type Put    struct { Key string; Value []byte }
type Delete struct { Key string }

func Encode(recs []Record) ([]byte, error)
func Decode(b []byte) ([]Record, error)

var ErrCorrupt = errors.New("walcodec: corrupt stream")
```

`Put{}.Kind()` stays `1`. `Delete{}.Kind()` stays `2`. `ErrCorrupt`'s text stays `"walcodec: corrupt stream"`. Callers still compare with `errors.Is`.

Wire rules that v2 does not change, and that both depths share:

- Big-endian `uint32` lengths and CRCs.
- Frame shape `[len uint32][type uint8][payload][crc uint32]`. `len` counts `type`+`payload` only. The CRC sits outside `len` and is CRC32-IEEE (`hash/crc32.ChecksumIEEE`) over `type`+`payload`.
- `len` cap is `16 << 20` (`16777216`). A larger `len` is `ErrCorrupt`, rejected before any allocation sized to that `len`.
- `len == 0` stays `ErrCorrupt` (a frame must contain a type byte). This is a v1 invariant the amendment does not relax.
- Put payload: `[klen uint32][key][vlen uint32][value]`. Delete payload: `[klen uint32][key]`. A length prefix that runs past the enclosing payload is `ErrCorrupt`. Trailing bytes after a well-formed Put or Delete payload are `ErrCorrupt`.
- `nil` and empty `Value` both encode as `vlen = 0` and both decode to a length-0 `[]byte`. They compare equal. Decode copies value bytes so the returned slice does not alias the input.
- Duplicate keys are kept, in stream order. The codec is a log, not a map.
- Empty input to `Encode` is a one-byte stream. Decoding that stream yields an empty non-nil slice and no error.
- `Decode(nil)` and `Decode([]byte{})` are `ErrCorrupt` (missing version byte). The version byte is checked before any frame.
- Unknown type tags are `ErrCorrupt`. A CRC mismatch is `ErrCorrupt`. A truncated frame (not enough bytes for `len`+body+crc) is `ErrCorrupt`.
- Decode returns `(nil, ErrCorrupt)` on every failure. It does not return a partial record slice.
- Unexported encode errors for an unsupported concrete type, and for a record whose frame body would exceed the 16 MiB cap, stay unexported and are not `ErrCorrupt`.

v1 public tests stay in the package unchanged and must keep passing. They never assert that `Encode` writes version `1`. The fixed v1 fixture in the v2 suite is the regression vehicle for version-1 bytes.

## What changes

### Exported addition

One new type, and nothing else on the public surface:

```go
type Batch struct { Records []Record } // Kind 3
func (Batch) Kind() uint8 { return 3 }
```

`Batch` is a value type, like `Put` and `Delete`. It implements `Record`. A compile-time assertion `_ Record = Batch{}` sits next to the existing ones.

Doc comments that describe v1 as the only version are updated in the same files (see Files). The new `Batch` comment states the payload layout and the depth convention so a later reader does not rediscover the off-by-one.

### Version byte

- `Encode` writes `version = 2` for every stream, including empty input and Put/Delete-only input. The v1 constant `version1 = 1` remains, used only by `Decode`.
- `Decode` accepts version `1` and version `2`. Any other byte, including `0` and `3`, is `ErrCorrupt` before any frame is read.
- Version `1` admits only Put (type 1) and Delete (type 2). A type-3 frame in a version-1 stream is `ErrCorrupt`.
- Version `2` admits Put, Delete, and Batch (type 3).

### Batch payload

```
Batch payload = n(4) frame_1 frame_2 ... frame_n
```

- `n` is a big-endian `uint32` count of inner records.
- Each inner frame is a full `[len][type][payload][crc]` frame, same shape and same 16 MiB `len` cap as a top-level frame.
- The `n` frames must consume the Batch payload exactly. Too few frames, an inner frame that would extend past the payload, or any trailing byte after `n` frames is `ErrCorrupt`.
- An inner `len` is bounds-checked against the remaining bytes of the enclosing payload, never against the outer stream. The decoder does not read a sibling frame or an outer frame to satisfy an inner `len`.
- An empty Batch (`Records == nil` or `len == 0`) encodes as `n = 0` and no inner frames. It decodes to `Batch{Records: []Record{}}` (empty non-nil slice), matching the top-level empty-stream convention.

`n` comes from untrusted input and can be `2^32-1`. The decoder never sizes an allocation from raw `n`. It does not call `make([]Record, n)` or `make([]Record, 0, n)`. Capacity, if used at all, is capped by a bound derived from the remaining payload before any child is parsed. The smallest on-wire frame is 9 bytes (4-byte `len` + 1-byte type + 4-byte CRC). Do not name a constant `minFrameLen` next to `maxFrameLen`: `maxFrameLen` limits the `len` field (type+payload, CRC outside it), while 9 is the full on-wire size, and the smallest legal `len` field is 1. Write the check inline, or name the constant `minWireFrameBytes` with a comment that its unit is the full frame including CRC:

```text
const minWireFrameBytes = u32Size + 1 + crcSize // 9; full on-wire frame, not the len field

if n > uint32(len(rest)/minWireFrameBytes) { return ErrCorrupt }
```

`n` is `uint32` and `len(rest)/9` is `int`. Compare in a width that cannot wrap: reject when `uint64(n) > uint64(len(rest)/minWireFrameBytes)`. A payload that cannot hold `n` minimum frames is `ErrCorrupt` with no large allocation. Growing the result with `append` from a zero or small capacity is also acceptable, because `append` is bounded by how many frames actually fit. The same spirit as the v1 oversize-`len` rule applies here: reject the impossible count before asking the runtime for memory proportional to the untrusted integer.

### Depth cap

Decoding and encoding carry a nesting depth. One unexported helper owns the rule so the encoder and the decoder cannot drift:

```go
// childDepth is the depth of a Batch's inner sub-stream.
// Top-level records are encoded and decoded at depth 0.
// A sub-stream at depth greater than maxBatchDepth is illegal.
func childDepth(parent int) (int, bool) {
    d := parent + 1
    if d > maxBatchDepth { // maxBatchDepth = 8
        return 0, false
    }
    return d, true
}
```

- `Encode` calls `appendFrame(dst, rec, 0)` for each top-level record. There is no other entry point.
- A Batch's children are written and read at `childDepth(parent)`. `false` means the sub-stream would sit at depth 9 or more.
- The check happens when a Batch is entered, before its inner frames are parsed or written, and before the children are visited. It runs even when `Records` is empty.
- There is no `if depth > 8` check inside `decodeStream`. That check was dead: `decodeStream` is only entered at depth 0 or at a depth `childDepth` already accepted. Batch payloads use their own loop and call `childDepth` before it.
- The encoder refuses to emit a stream it would itself reject. Over-deep encode returns `ErrCorrupt` wrapped with context (see Error policy).
- Depth counts Batch levels, not frame count. A Batch of many sibling Puts at depth 1 is legal. The public test wraps one Put in 9 Batch frames and expects `ErrCorrupt`; 8 wraps of that same shape must succeed.

The depth check is also the cycle stop. A self-referencing value is legal Go:

```go
s := make([]Record, 1)
s[0] = Batch{Records: s}
```

Only the encoder's check-on-entry returns an error at the 9th entry. A check that ran only after walking children would recurse forever on this value.

Worked bound, matching `TestBatchNestedPastDepth8`:

| Batch wraps around a Put | Inner sub-stream depth | Result |
| --- | --- | --- |
| 8 | 8 | legal |
| 9 | 9 | `ErrCorrupt` |

### Shared frame reader

v1 `decodeFrame` currently consumes a suffix of the whole stream. That is unsafe once a frame can be nested: an inner `len` must not be allowed to reach past its enclosing payload. Replace the "read from the rest of the buffer" model with a bounded reader used at every depth.

`Decode` computes an `allowBatch` flag once from the version byte and passes that flag down. Nested sub-streams have no version byte of their own; they inherit the parent's allowed kinds. Version 1 sets `allowBatch = false`. Version 2 sets `allowBatch = true`. Frame parsing does not re-interpret a stream header at every depth. Adding a future version is a one-place change in `Decode`.

```text
Decode(b):
  missing version byte -> ErrCorrupt
  version not in {1, 2} -> ErrCorrupt          // before any frame
  allowBatch = (version == 2)
  return decodeStream(b[1:], allowBatch, depth=0)

decodeStream(buf, allowBatch, depth) -> []Record
  // depth is 0 here, or a depth childDepth already accepted. No second cap check.
  while buf is non-empty:
    rec, consumed = decodeFrame(buf, allowBatch, depth)
    append rec
  return records (empty non-nil if none)

decodeFrame(buf, allowBatch, depth) -> (Record, consumed)
  reject if fewer than 4 bytes remain in buf
  read len; reject if len == 0 or len > 16 MiB   // before allocating to len
  reject if buf does not contain len + 4 crc bytes
  check CRC over type||payload
  type 1 Put, type 2 Delete
  type 3 Batch only when allowBatch; otherwise unknown tag -> ErrCorrupt
  any other type -> ErrCorrupt
  payload parsers must consume their payload exactly

decodeBatchPayload(payload, allowBatch, depth):
  need 4 bytes for n, else ErrCorrupt
  read n as big-endian uint32
  rest = payload[4:]
  if uint64(n) > uint64(len(rest)/minWireFrameBytes) { return ErrCorrupt }
  child, ok = childDepth(depth)
  if !ok { return ErrCorrupt }                   // before parsing children
  for i in 0..n-1:
    rec, consumed = decodeFrame(rest, allowBatch, child)
    rest = rest[consumed:]
  if len(rest) != 0 { return ErrCorrupt }        // trailing bytes
```

`decodeFrame` receives only the enclosing slice. `frameLen > len(buf)` is `ErrCorrupt` and does not look at bytes outside that slice. An inner `len` equal to "bytes left in the outer stream" but larger than the Batch payload therefore fails inside the payload slice.

### Encoder

One function, `appendFrame(dst, rec, depth)`, owns the frame envelope for every kind. `Encode` calls `appendFrame(dst, rec, 0)` and discards `dst` on error.

`appendPut` and `appendDelete` stop writing their own `len` / type / CRC. They become payload-only writers: they append `[klen][key][vlen][value]` or `[klen][key]` into `dst` and return it. The size check stays in front of any append, exactly as v1 does it today. A multi-GiB `Value` must not be copied before the error returns, and the `uint32` casts must not see a length that does not fit.

```text
putBodyLen(r):
  keyLen, valLen = len(r.Key), len(r.Value)
  if keyLen > maxFrameLen or valLen > maxFrameLen: return errTooLarge
  bodyLen = 1 + u32Size + keyLen + u32Size + valLen   // type + payload
  if bodyLen > maxFrameLen: return errTooLarge
  return bodyLen

deleteBodyLen(r):
  keyLen = len(r.Key)
  if keyLen > maxFrameLen: return errTooLarge
  bodyLen = 1 + u32Size + keyLen
  if bodyLen > maxFrameLen: return errTooLarge
  return bodyLen

appendFrame(dst, rec, depth):
  switch rec:
    Put:
      bodyLen = putBodyLen(rec)          // errTooLarge before any append
      return appendClosedFrame(dst, kindPut, bodyLen, payload-only writer)
    Delete:
      bodyLen = deleteBodyLen(rec)
      return appendClosedFrame(dst, kindDelete, bodyLen, payload-only writer)
    Batch:
      child, ok = childDepth(depth)
      if !ok:
        return fmt.Errorf("%w: batch nesting exceeds depth 8", ErrCorrupt)
        // before walking children, even if Records is empty
      return appendBatchFrame(dst, rec, child)
    default:
      return errUnsupported

appendClosedFrame(dst, typ, bodyLen, writePayload):
  // bodyLen already known and <= maxFrameLen. Used for Put and Delete only.
  start = len(dst)
  dst = appendU32(dst, uint32(bodyLen))
  dst = append(dst, typ)
  dst = writePayload(dst)                // payload only; no envelope
  return appendU32(dst, crc32.ChecksumIEEE(dst[start+u32Size:]))

appendBatchFrame(dst, batch, childDepth):
  start = len(dst)
  dst = append 4 zero bytes              // reserved len
  dst = append kindBatch
  dst = appendU32(dst, uint32(len(batch.Records)))
  for rec in batch.Records:
    dst = appendFrame(dst, rec, childDepth)
    bodyLen = len(dst) - start - 4       // type + payload so far
    if bodyLen > maxFrameLen: return errTooLarge
  bodyLen = len(dst) - start - 4
  if bodyLen > maxFrameLen: return errTooLarge
  fill len at dst[start:start+4] as big-endian uint32(bodyLen)
  return append CRC over dst[start+4:]
```

`bodyLen` for a closed frame is the size of `type`+`payload`, matching v1. It cannot be 0: the type byte is always written. The depth error returns at the `childDepth` check, not at the size check. The only size failure at the close of a frame is `bodyLen > maxFrameLen` → `errTooLarge`. That running check after each child applies only to Batch. Put and Delete never enter the in-place path; they reject up front and then write a known-size body.

An oversized Batch is `errTooLarge`, not `ErrCorrupt`. `Encode` already discards `dst` on error, so a partial Batch write is not returned.

### Files

| File | Action |
| --- | --- |
| `record.go` | Add `Batch`, `kindBatch = 3`, `version2 = 2`, `maxBatchDepth = 8`, `minWireFrameBytes = u32Size + 1 + crcSize` with a comment that the unit is the full on-wire frame (CRC included), not the `len` field. Keep `version1`. Add the `Record` assertion for `Batch`. Update the `ErrCorrupt` comment: it is the decode error (returned bare), and `Encode` also returns it wrapped when nesting exceeds depth 8. It is not the only package error. |
| `encode.go` | Write `version2`. `Encode` calls `appendFrame(dst, rec, 0)`. `appendPut` / `appendDelete` become payload-only writers; their size checks stay before any append. Add `appendBatchFrame` for in-place children. Reject `!childDepth(depth)` with wrapped `ErrCorrupt` before recursing. Reject a non-Put/Delete/Batch concrete value with the existing `errUnsupported`. Update the doc comment: `Encode` writes a version-2 stream, and a nest past depth 8 returns an error that `errors.Is` matches to `ErrCorrupt`. |
| `decode.go` | Accept versions 1 and 2. Compute `allowBatch` once in `Decode` and pass it down. Bound every frame read to the current buffer. Add `decodeBatchPayload` with the `uint64(n) > uint64(len(rest)/minWireFrameBytes)` reject-before-allocate check and a `childDepth` check before children. Delete any stream-level `depth > 8` check. On version 1, type 3 is unknown. Update the doc comment: `Decode` parses version 1 (Put/Delete) and version 2 (Put/Delete/Batch). |
| `walcodec_v2_internal_test.go` | New. Mandatory private suite. Stands alone: no reference to any identifier defined in `walcodec_public_test.go` or `walcodec_v2_public_test.go`. Every top-level identifier, including helpers, carries the `priv` prefix. |
| `walcodec_fuzz_test.go` | New. Stands alone under the same rule. The fuzz target is `FuzzPrivDecode`. No new dependencies. |
| `walcodec_public_test.go` | Unchanged. |
| `walcodec_v2_public_test.go` | Unchanged. Already in the worktree; run it beside the v1 suite. |

No new exported helpers. No change to `go.mod`.

## How each v2 rule is satisfied

1. **Kind 3, payload is a framed sub-stream.** `Batch.Kind()` returns 3. `appendBatchFrame` writes big-endian `n` then `n` full frames into the destination. `decodeBatchPayload` reads that layout with the shared frame decoder. Public case 1 (`TestBatchRoundTrip`) encodes a Batch of one Put and one Delete and decodes it back. That public case does not pin the wire layout. Private `TestPrivBatchGoldenBytes` hand-builds the bytes and requires `Encode` to match them and `Decode` to return the records. Private `TestPrivEmptyBatchRoundTrip` covers `n = 0`.

2. **Recursion: Put, Delete, or Batch inside a Batch.** The payload parser dispatches all three types when `allowBatch` is true. Nested calls pass the child depth from `childDepth` and a slice of the inner payload, so recursion is the same function, not a second format. Public case 2 (`TestNestedBatchRoundTrip`) covers Batch-in-Batch. The golden stream includes `Batch{Put, Delete, Batch{Put}}`.

3. **16 MiB cap at every depth.** `decodeFrame` rejects `len > 16<<20` before allocating, whether the frame is top-level or inner. Put and Delete compute `bodyLen` and return `errTooLarge` before any append. Batch checks the running body after each child is closed, so an oversized Batch fails when the body crosses the cap. The running check applies only to Batch.

4. **Inner `len` past the enclosing payload is truncated, not a read into the next frame.** `decodeFrame` takes the enclosing slice as its only buffer. `frameLen > len(buf)` is `ErrCorrupt` and does not look at bytes outside that slice. This rule is not what `TestTruncatedBatch` proves: that test cuts two bytes off the outer stream, so the outer CRC is short and the inner frames need not be reached. The private case `TestPrivInnerLenDoesNotSpill` first decodes its unmodified base stream successfully, then applies the single defect: the outer frame still has a valid CRC and is followed by extra bytes, while the inner `len` equals the bytes left in the outer stream and extends past the Batch payload. Decode must return `ErrCorrupt`.

5. **`n` frames consume the payload exactly.** After `n` frames, leftover bytes are `ErrCorrupt`. If the payload ends before `n` frames are complete, the short read is `ErrCorrupt`. `n` itself requiring 4 bytes that are not present is `ErrCorrupt`. Private cases: `TestPrivBatchNTooLarge` (`n` larger than the frames present) and `TestPrivBatchNTrailingBytes` (`n` smaller, trailing bytes). Each first decodes the unmodified base stream, then applies the single defect. `TestPrivBatchNMaxUint32` uses `n = 0xFFFFFFFF` and an empty frame list and expects `ErrCorrupt` without a huge allocation.

6. **Depth cap 8, top-level is 0.** Entering a Batch at depth `d` requires `childDepth(d)` to succeed, which is `d+1 <= 8`. The 9-wrap hand-built stream in `TestBatchNestedPastDepth8` sits at depth 9 and is rejected. That public test does not prove that 8 is accepted, so a decoder that rejects at 8 still passes it. Private `TestPrivDepth8AcceptsHandBuilt` hand-builds the 8-wrap stream in the same shape as the public 9-wrap stream (a Put wrapped in 8 Batch frames, version byte 2) and asserts `Decode` accepts it and returns the nested records. It does not go through `Encode`. Private `TestPrivDepth9EncodeRefused` expects `Encode` of 9 wraps to return an error that `errors.Is` matches to `ErrCorrupt`. Private `TestPrivEmptyBatchAtDepth9` rejects an empty Batch at the 9th level on both sides, including the self-referencing value on encode. The encoder applies the same `childDepth` check before walking children and will not emit a 9-deep stream.

7. **Encode writes version 2.** The first byte of every `Encode` result is `2`, including `Encode(nil)` (still one byte) and Put/Delete-only input. That version bump is the allowed behavior change called out in the amendment. v1 tests do not assert the version byte. Private `TestPrivEncodeVersionIs2` asserts the first byte of a Put-only encode and of `Encode(nil)`. The golden-bytes test also requires byte 0 to be `2`.

8. **Decode accepts version 1 and version 2 only.** Version check is the first decode step, before frames. Version 3 is `TestVersion3Rejected`. Version 0 is the same path and is pinned by private `TestPrivVersion0Rejected`. Version 1 still decodes the fixed fixture. `Decode([]byte{1})` and `Decode([]byte{2})` succeed with an empty slice (`TestPrivVersionOnlyStreams`).

9. **Version-1 streams are Put/Delete only.** `Decode` sets `allowBatch = false` for version 1, so a Kind-3 frame is an unknown tag and `ErrCorrupt`. Version 2 sets the flag. The public fixture has no Batch, so this is pinned by private `TestPrivVersion1RejectsBatch`, which first decodes the same frames under version 2 and then flips only the version byte to 1.

10. **v1 decode behaviors hold against a fixed fixture, not against re-encoding.** `v1Fixture()` is the literal 43-byte stream (version `1`, Put `alpha`/`one`, Delete `beta`, both CRCs valid). `TestV1FixtureDecodesUnderV2` and `TestV1BehaviorsViaFixture` decode it, flip its last CRC byte, and truncate it. The implementation does not special-case those bytes; the shared version-1 path produces the records and the corrupt/truncated errors. The private file repeats those literal bytes itself (`privV1Fixture`) so the regression still runs if the shipped public file is removed.

11. **Mixed top-level stream.** `decodeStream` at depth 0 accepts Put, Delete, and Batch in any order under version 2. `TestMixedTopLevelRoundTrip` covers that, including a nil Value (encodes as `vlen = 0`). The golden stream is the same shape: a top-level Put, a nested Batch, and an empty Batch.

12. **Frozen surface.** No v1 symbol changes name or signature. The only addition is `Batch`.

13. **v1 regression suite.** `go test` runs both public files plus the private file and the fuzz seed corpus. Cases that `Encode` then `Decode` still pass because they compare records, not the version byte. `TestEmptyRecsVersionOnly` still expects a one-byte stream. `TestUnknownType` builds a version-1 stream with type 99; version 1 still rejects it. `TestOversizeLen` uses version 1 and a `len` of `16<<20+1`; the cap check is unchanged and still happens before allocation.

## v1 rules that remain in force

These are not restated as new work, but the v2 decoder must not drop them:

- CRC checked on every frame at every depth before the payload is trusted.
- Unknown type is `ErrCorrupt` (type 99 at top level; any type other than 1, 2, or 3 at version 2; any type other than 1 or 2 at version 1).
- Truncation of a top-level frame is `ErrCorrupt`, including a short CRC.
- Oversize `len` is `ErrCorrupt` with no allocation of that size. The implementation reads the 4-byte `len`, compares it to the cap, and returns before `make` of a body buffer. Small header slices are fine.
- Duplicate keys kept in order, including duplicates inside a Batch and across the Batch boundary.
- Empty and nil values decode equal, including inside a Batch.
- `Decode` of a version-only stream (`[]byte{1}` or `[]byte{2}`) returns an empty slice and no error.

## Error policy

| Condition | Error |
| --- | --- |
| Missing version, version other than 1 or 2, bad CRC, unknown type, truncated frame, oversize `len`, `len == 0`, payload length overrun, trailing payload bytes, Batch `n` mismatch, `n` that cannot fit in the remaining payload, inner frame past enclosing payload, nesting depth > 8 on decode | `ErrCorrupt` |
| Nesting depth > 8 on encode | `fmt.Errorf("%w: batch nesting exceeds depth 8", ErrCorrupt)` so `errors.Is(err, ErrCorrupt)` is true |
| Concrete type other than Put, Delete, Batch | unexported `errUnsupported` |
| Frame body would exceed 16 MiB | unexported `errTooLarge` |
| Nil `Record` interface in the encode slice | `errUnsupported` (type switch misses) |

Decode never wraps `ErrCorrupt`. Encode returns wrapped `ErrCorrupt` only for the depth cap. Size and type problems stay the unexported errors, so `errors.Is(err, ErrCorrupt)` is false for those. The wrap is the caller-visible distinction: a refused nest matches `ErrCorrupt`; a record that does not fit the frame format does not. The amendment does not fix the encode error identity; this choice is pinned by `TestPrivDepth9EncodeRefused` and by `TestPrivTooLargeIsNotCorrupt`.

`errUnsupported` and `errTooLarge` stay unexported. Callers outside the package can distinguish them from a refused nest only via `errors.Is(..., ErrCorrupt)`. That is the intended distinction, and the `Encode` doc comment says so.

## Test approach

Do not edit the shipped suites. Acceptance is:

```text
go test ./...
go vet ./...
go build ./...
```

Both public files live in package `walcodec` and run together. v2 helpers are already `2`-suffixed, so they do not collide with v1 helpers.

The private and fuzz files must stand alone. The hidden grader may replace or remove the shipped public test files. A private file that calls `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `beU32`, `mkFrame`, `recEq`, `recEq2`, or any other identifier defined in those files fails to compile, and every test fails. Copy the bytes and helpers the private file needs, under `priv`-prefixed names. Do not call into the public tests to obtain a stream.

Every top-level identifier in `walcodec_v2_internal_test.go` and `walcodec_fuzz_test.go` carries the `priv` prefix, including the fuzz target `FuzzPrivDecode`. A grader file that defines its own `FuzzDecode` must not collide.

Every negative private test has a positive control. Build one well-formed base stream, `Decode` it, and require the expected records. Then apply exactly one defect and require `ErrCorrupt`. A decoder that rejects every Batch fails the control.

`walcodec_v2_internal_test.go` is mandatory. One case each:

| Test | Pins |
| --- | --- |
| `TestPrivBatchGoldenBytes` | Hand-built version-2 stream, using only `priv` helpers: a top-level Put, a `Batch{Put, Delete, Batch{Put}}`, and an empty Batch. `Decode` returns those records. `Encode` of those records returns exactly those bytes. Pins big-endian `n`, `n` as a record count, `len` excluding CRC, and CRC over `type`+`payload` only. |
| `TestPrivDepth8AcceptsHandBuilt` | Hand-built 8-wrap stream in the same shape as the public 9-wrap stream (Put inside 8 Batch frames, version 2). `Decode` accepts it and returns the nested Put. Does not call `Encode`. |
| `TestPrivDepth9EncodeRefused` | `Encode` of 9 wraps returns an error with `errors.Is(err, ErrCorrupt)`. |
| `TestPrivEmptyBatchAtDepth9` | empty Batch at the 9th level is `ErrCorrupt` on decode; the unmodified 8-wrap empty Batch decodes first. Encode of a self-referencing Batch returns wrapped `ErrCorrupt` and does not recurse forever. |
| `TestPrivInnerLenDoesNotSpill` | Unmodified base decodes. Then: outer frame CRC valid, extra bytes follow the outer frame, inner `len` equals bytes left in the outer stream and past the Batch payload; `ErrCorrupt`. |
| `TestPrivVersion1RejectsBatch` | Same type-3 frames decode under version 2. Version byte flipped to 1 is `ErrCorrupt`. |
| `TestPrivVersion0Rejected` | `Decode([]byte{0})` and a version-0 stream with an otherwise valid Put are `ErrCorrupt`. The same Put under version 2 decodes first. |
| `TestPrivEncodeVersionIs2` | first byte of `Encode` of a Put, and of `Encode(nil)`, is `2`. |
| `TestPrivBatchNTooLarge` | Base stream decodes. Then `n` larger than the frames present is `ErrCorrupt`. |
| `TestPrivBatchNTrailingBytes` | Base stream decodes. Then `n` smaller than the bytes that follow, with trailing bytes, is `ErrCorrupt`. |
| `TestPrivBatchNMaxUint32` | `n = 0xFFFFFFFF` and no frames is `ErrCorrupt`. |
| `TestPrivVersionOnlyStreams` | `Decode([]byte{1})` and `Decode([]byte{2})` return an empty slice. |
| `TestPrivEmptyBatchRoundTrip` | empty and nil `Records` round-trip to an empty non-nil slice. |
| `TestPrivNilValueInBatch` | nil and empty `Value` inside a Batch decode equal. |
| `TestPrivDupKeysInBatch` | duplicate keys inside a Batch stay in order. |
| `TestPrivTooLargeIsNotCorrupt` | a Put whose body exceeds 16 MiB returns an error that is not `ErrCorrupt`, and the call returns before copying the value. |

Golden layout the private test builds by hand, not by calling `Encode` first:

```text
stream = version(1) frame*
frame  = len(4) type(1) payload(len-1) crc(4)
crc    = CRC32-IEEE over type||payload, big-endian, outside len
Put payload    = klen(4) key vlen(4) value
Delete payload = klen(4) key
Batch payload  = n(4) frame_1 ... frame_n
```

`TestPrivBatchGoldenBytes` uses short keys so the expected bytes are obvious: top-level `Put{"p", "v"}`, then `Batch{ Put{"a","1"}, Delete{"b"}, Batch{ Put{"c","d"} } }`, then `Batch{}`. The test computes each CRC with `hash/crc32` inside the private file. It asserts `bytes.Equal(Encode(recs), handBuilt)` and `privRecsEq(Decode(handBuilt), recs)`. A shared bug (little-endian `n`, `n` omitted, `n` counting bytes, `len` including inner CRCs, CRC covering the `len` field) fails the byte equality even when `Encode` and `Decode` agree with each other.

`walcodec_fuzz_test.go` adds `FuzzPrivDecode`. It must not panic. The format is canonical: exact consumption, a fixed field order, and no optional encodings. If `Decode(b)` succeeds with records `recs`:

- `Encode(recs)` must return `err == nil`. A non-nil encode error fails the fuzz case.
- Let `out` be that encode result. `out[0]` must be `2`.
- If `b[0] == 2`, `bytes.Equal(out, b)` must hold.
- If `b[0] == 1`, `bytes.Equal(out, append([]byte{2}, b[1:]...))` must hold.

A records-only comparison is not the oracle. The byte equality catches a decoder that accepts a non-canonical layout and an encoder that does not reproduce the bytes it just decoded. Version 1 is the only allowed rewrite, and the rewrite is exactly the first byte.

Seed the corpus with bytes the fuzz file constructs itself (no call into public or other test helpers):

- the literal v1 fixture bytes (version 1, Put `alpha`/`one`, Delete `beta`)
- a hand-built 9-level nested Batch stream (Put wrapped in 9 Batch frames, version 2)
- a hand-built 8-level nested Batch stream
- `Encode` output of a mixed Put/Delete/Batch stream (depth 1 and depth 2), obtained inside the fuzz file by calling `Encode`
- `Encode(nil)` and a version-2 version-only stream
- the inner-len spill stream, built in the fuzz file
- a version-0 byte and a version-3 byte prefixed onto an otherwise valid frame

No third-party fuzz library. Go 1.25's `testing.F` is enough. The fuzzer is a regression net, not a substitute for the table above.

## Risks to avoid

- Treating depth as "frames deep" or counting the top-level stream as depth 1. The grader's convention is top-level 0, reject a sub-stream at depth 9. Off-by-one fails `TestBatchNestedPastDepth8` or rejects a legal 8-deep Batch. `TestPrivDepth8AcceptsHandBuilt` catches the strict off-by-one without trusting `Encode`.
- Letting `decodeFrame` use the outer buffer's length. That would accept an inner `len` that spills into the parent CRC or the next sibling, which the amendment forbids. `TestTruncatedBatch` does not catch this; `TestPrivInnerLenDoesNotSpill` does, and only after the base stream decodes.
- Sharing a layout bug between encoder and decoder. Every public positive Batch test is Encode-then-Decode. `TestPrivBatchGoldenBytes` compares both sides to hand-built bytes.
- A negative test whose base stream is also rejected. A decoder that rejects every Batch would pass it. Every defect test decodes the unmodified base first.
- Calling a helper defined in a shipped public test file from a private or fuzz file. Those files may be removed. Standalone `priv` helpers only.
- Naming the fuzz target `FuzzDecode`. A grader file with the same name fails the package compile. The name is `FuzzPrivDecode`.
- Forgetting to reject type 3 on version 1. The fixture has no Batch, so the public v1 fixture tests would still pass. `TestPrivVersion1RejectsBatch` pins it.
- Writing version 1 from `Encode`. Every v2 round-trip test decodes with the v2 decoder, but the amendment requires version 2. `TestPrivEncodeVersionIs2` and the golden test read the first byte.
- Returning a partial slice on error. v1 returns `nil, ErrCorrupt`. Keep that.
- Allocating a body of size `len` before the cap check. The v1 oversize test exists specifically to forbid that, and the same check must hold for inner frames.
- Allocating from raw Batch `n` (`make([]Record, 0, n)` with `n = 0xFFFFFFFF`). Reject `uint64(n) > uint64(len(rest)/minWireFrameBytes)` first.
- Checking `frameLen < minFrameLen` against the `len` field. That field's minimum is 1. The 9-byte figure is the full on-wire frame. The constant is `minWireFrameBytes`, used only for the `n` bound.
- Checking depth only after walking children. A self-referencing Batch would recurse without bound. Check on entry via `childDepth`.
- Copying a multi-GiB Put or Delete before `errTooLarge`. Size the body first; `appendPut` / `appendDelete` write payload only after that check.
- Changing `Put`, `Delete`, `Record`, `Encode`, `Decode`, or `ErrCorrupt` while adding `Batch`.
- Leaving the v1 doc comments in place. They would claim `Encode` writes version 1 and that `ErrCorrupt` is decode-only.
