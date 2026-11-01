# walcodec v2 amendment plan

## Scope and inspected baseline

Extend the v1 WAL codec with nested `Batch` records and version-2 streams. The v1 public symbols and signatures stay frozen: `Put`, `Delete`, `Record`, `Encode`, `Decode`, and `ErrCorrupt`. Add only `Batch{Records []Record}` with `Kind() == 3`.

The supplied `worktree` contains the v1 and v2 public test files and `go.mod`, but no codec implementation. The v1 tests specify the behavior to preserve: Put/Delete round trips, CRC and truncation rejection, unknown type rejection, the 16 MiB frame cap, one-byte encoding of an empty record list, missing-version rejection, duplicate order, and nil/empty value equivalence. Implementation should retain their frame and payload conventions as specified by the tests and amendment.

## Changes

1. **Version handling.** Make `Encode` write version byte 2 for every stream, including empty and Put/Delete-only streams. `Decode` checks for a version byte first, then accepts only versions 1 and 2 before examining any frame. Both versions permit Put and Delete; version 1 rejects Batch, while version 2 permits Batch recursively. All unsupported versions and version/type combinations return an error wrapping or matching `ErrCorrupt`.

2. **Batch API and wire encoding.** Add the exported `Batch` struct and a value-receiver `Kind()` returning 3. Encode its payload as a big-endian uint32 record count followed by that many normal framed records. Its outer frame CRC covers the type byte and the complete payload. Nested Batch records use the same framing and CRC rules. Retain existing Put/Delete payload formats. Encoding rejects unrepresentable lengths/counts, any individual frame over 16 MiB, unsupported record values, and nesting beyond the allowed depth.

3. **Bounded frame decoding.** Parse each stream from a bounded byte slice; a Batch parser receives only that Batch's payload. For every frame, require the length prefix, reject `len > 16 MiB` before allocation, and verify the full type/payload body and trailing CRC fit inside the current slice before taking subslices. Verify CRC32-IEEE over type plus payload. This prevents an inner length from consuming bytes belonging to a sibling or outer frame.

4. **Batch count and exact payload consumption.** Require the Batch's four-byte count prefix. Before count-driven iteration, check that `count <= remaining/9`, since even the smallest complete frame requires 9 bytes (length, type, and CRC). Do not allocate a record slice based on the untrusted count; append only successfully decoded records. Parse exactly `count` frames and require the parser offset to equal the Batch payload length. Missing frames, extra frames/trailing bytes, impossible counts, and malformed inner frames are `ErrCorrupt`.

5. **Legacy payload behavior.** Keep Put and Delete payload decoding compatible with v1, including their length-prefix checks and exact payload consumption. Reject unknown tags and malformed payloads as `ErrCorrupt`. Preserve order and duplicates, and decode nil and empty Put values equivalently. Verify compatibility using the fixed version-1 literal fixture in the v2 tests; do not derive it from the now-v2 encoder.

6. **Depth rule.** Pass sub-stream depth through recursive encode and decode. The top-level stream is depth 0; parsing a Batch's inner stream increments depth. Allow sub-stream depths through 8 and reject depth 9. Apply this same convention on encode and decode, so the encoder cannot produce a stream the decoder rejects. In particular, eight nested Batch frames around a leaf are allowed; a ninth is corrupt on decode and rejected by Encode.

7. **Error contract.** All malformed input conditions in Decode—bad or missing version, bad CRC, unknown/forbidden type, oversized or truncated frame, malformed payload/count, trailing Batch bytes, and excessive depth—must satisfy `errors.Is(err, ErrCorrupt)`.

## What stays the same

- Frame layout remains `[len uint32][type byte][payload][crc uint32]`, with big-endian integers. `len` counts the type byte and payload, excluding the CRC; CRC32-IEEE covers the type byte and payload.
- The 16 MiB maximum is applied to each frame independently at every nesting level.
- Put/Delete kinds, payloads, record ordering, duplicate handling, and v1 decode semantics remain unchanged.
- Encoding no records remains a version-only stream (now byte 2); decoding nil or empty input remains corrupt.
- Existing exported names and signatures remain unchanged; `Batch` is the only new exported type.

## Amendment and public-test mapping

| Rule or acceptance case | Planned satisfaction |
| --- | --- |
| Add `Batch{Records []Record}` with Kind 3 | Add exactly this public type and kind behavior. |
| Batch contains count plus full inner frames | Encode/decode the big-endian count and normal recursive frames. |
| Inner frames consume Batch payload exactly | Decode declared count and require exact final offset. |
| Inner frame cannot escape enclosing Batch | Parse from a payload-bounded slice; check frame extent before slicing. |
| 16 MiB cap at every depth | Reuse the frame length check in every recursive parser and encoder. |
| Version 2 emitted | Write 2 as the first byte for all `Encode` outputs. |
| Accept versions 1 and 2 only | Validate version before frame parsing; gate Batch on v2. |
| Preserve v1 behavior using fixed bytes | Decode the fixed v1 fixture and preserve its records, CRC failure, and truncation failure. |
| Batch Put/Delete round trip | Covered by `TestBatchRoundTrip`. |
| Nested Batch round trip | Covered by `TestNestedBatchRoundTrip`. |
| Mixed top-level round trip | Covered by `TestMixedTopLevelRoundTrip`. |
| Truncated Batch corrupt | Bounded frame checks and exact consumption reject it. |
| Version 3 corrupt | Early version validation rejects it. |
| Batch depth past 8 corrupt | Depth-aware decoder rejects the hand-built nine-Batch stream; encoder enforces the same cap. |
| v1 regression tests | Retain CRC, unknown type, truncation, oversize length, empty stream, missing version, duplicate order, and nil/empty-value behavior. |
| Adversarial count safety | Check count feasibility before iteration and avoid count-sized allocation; a `0xffffffff` count in a four-byte payload fails promptly as `ErrCorrupt`. |

## Validation after implementation

Run both public test files together. Add focused cases for empty Batch, trailing bytes, too-small/too-large counts (including `0xffffffff` with no frame bytes), an inner length crossing its Batch boundary, Batch under v1, versions 0 and 3, and the legal depth-8/rejected depth-9 boundary. Confirm malformed decode errors satisfy `errors.Is(err, ErrCorrupt)`. The implementation brief also calls for `go build ./...` and `go vet ./...`.
