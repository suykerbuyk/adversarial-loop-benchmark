# walcodec v2 implementation plan

## Baseline and scope

The target package is `worktree` (`module walcodec`). Its current contents include `go.mod` and both public test files, but no implementation file. First add `worktree/walcodec.go`, carrying forward the v1 implementation in `snapshot-v1/walcodec.go`, then extend it for v2. Keep the existing exported names and signatures for `Record`, `Put`, `Delete`, `Encode`, `Decode`, and `ErrCorrupt`; add only `Batch{Records []Record}` and its `Kind() uint8` method returning 3. Do not change tests or other files.

## Design and rules

1. **Versions and record kinds.** `Encode` always starts with version byte 2, including empty and Put/Delete-only streams. `Decode` rejects missing versions and every version except 1 and 2 before reading a frame. Version 1 accepts only Kind 1 Put and Kind 2 Delete; a well-formed Kind 3 in a v1 stream is `ErrCorrupt`. Version 2 accepts Kinds 1, 2, and 3. Unknown tags are corrupt for either version.

2. **Shared frame handling.** Preserve the v1 frame format: big-endian uint32 `len`, then `type+payload`, then big-endian CRC32-IEEE of exactly `type+payload`. Use one bounded frame reader and writer at all levels. The declared `len` is a uint64 for checks and is the number of bytes in `type+payload`; reject `len == 0` and `len > uint64(16<<20)`, while accepting exactly `16<<20`. Before slicing, require the complete body and four CRC bytes to remain in the current stream slice. Check CRC before decoding the payload. These constraints apply identically to inner and outer frames.

3. **Payload exactness.** Put remains `[keyLen][key][valueLen][value]`; Delete remains `[keyLen][key]`. Reject absent/short field lengths, fields exceeding the bounded payload, and any unconsumed payload bytes. Decode empty values to a zero-length value, preserving nil/empty equivalence. Batch payload is `[count uint32][count complete inner frames]`; require the four-byte count, decode exactly count frames, and require the cursor to equal the payload length afterward. This rejects count underflow, extra frames/trailing bytes, and malformed fields at every depth.

4. **Batch bounds and hostile counts.** The recursive stream parser receives only the Batch frame's payload slice (after its type byte), never the enclosing or top-level stream. Consequently an inner frame whose body or CRC extends past its Batch payload returns `ErrCorrupt`, even if bytes belonging to a following outer frame exist. Before any allocation whose size depends on count, compute `(uint64(len(payload))-4)/9` (minimum frame footprint: 4-byte length + 1-byte type + 4-byte CRC) and reject larger counts. Do not allocate using raw untrusted `count`; append decoded records only as bounded frames succeed. A count of zero is valid and produces an empty `Records` slice.

5. **Depth.** Top-level stream depth is 0. When a Batch at depth `d` opens its child stream, that sub-stream is depth `d+1`; reject precisely when the child depth is greater than 8 (`if d+1 > 8`). Thus eight nested Batches are legal and a ninth is rejected. Apply the same rule to decoding and encoding, even for empty Batches; an empty Batch at legal depth 8 is valid, but a Batch at depth 8 cannot open depth 9. Decode returns `ErrCorrupt` for excessive depth. Encode returns the existing non-corruption `errEncode`, with a nil output buffer, for excessive depth.

6. **Encode dispatch and size checks.** Preserve v1 accepted concrete forms: `Put`, non-nil `*Put`, `Delete`, non-nil `*Delete`; add `Batch` and non-nil `*Batch`. Reject nil pointers and all other `Record` implementations with `errEncode` and nil bytes. Validate key/value representability and arithmetic before uint32 conversion/allocation. Every encoded frame, including a Batch's outer frame (`type+4-byte count+all inner frames`), must have `len <= 16<<20`; do not assume capped child frames make their containing Batch legal. Enforce this on the same encoded `len` definition used by Decode, at every level, and do not return an output stream the decoder would reject.

## What stays unchanged

The wire encoding of v1 Put/Delete records, CRC definition, big-endian fields, error sentinel behavior, record order and duplicate preservation, and the frozen v1 API stay intact. Empty top-level input remains a one-byte stream and decodes to zero records; its encoded byte is now 2. `Decode(nil)` and `Decode([]byte{})`, short frame headers, malformed Put/Delete fields, trailing payload data, zero/oversize lengths, CRC failures, unknown tags, and truncation remain `ErrCorrupt`.

## Acceptance and regression cases

The implementation belongs in `worktree/walcodec.go`, package `walcodec`. Run both public test files there. In addition to those suites, the plan's validation cases are:

- Confirm `Encode(nil)` and a Put/Delete-only Encode both begin with byte 2; decode version 1, version 2, and reject bytes 0 and 3 before frame parsing.
- Decode the fixed v1 fixture to Put alpha/one and Delete beta; independently confirm its CRC flip and truncation are `ErrCorrupt`.
- Round-trip Batch of Put/Delete, nested Batch, mixed top-level records, empty Batch, and an eight-level Batch; reject a nine-level Batch on both Encode and Decode. The Encode rejection returns nil bytes and does not match `ErrCorrupt`.
- Reject Kind 3 under version 1, and verify Kind 99 is rejected under both versions.
- Reject truncated Batch count header, count larger than the payload's minimum-frame capacity (including `0xffffffff` without count-sized allocation), fewer than `n` frames, and trailing bytes after `n` frames.
- Construct a valid-CRC outer Batch whose inner frame length/CRC would fit only by reading into bytes after the outer Batch payload; require `ErrCorrupt`. This verifies parser slice isolation, beyond a top-level truncation check.
- With valid outer CRCs, independently corrupt an inner CRC, use inner tag 99, inner zero length, and inner length above 16 MiB; each must be `ErrCorrupt`.
- Check the 16 MiB frame boundary on both Encode and Decode: exactly `16<<20` type-plus-payload bytes are within the cap; `16<<20+1` is rejected. Include a Batch whose children individually fit but whose containing frame exceeds the cap.
- Recheck malformed Put/Delete field lengths and trailing bytes, short outer header, `Decode(nil)`/empty, and equivalent nil/empty Put values. Include malformed Put/Delete payloads and exact-consumption failures inside a Batch as well as at top level.

Run `go test`, `go vet`, and `go build` from `worktree` after implementation. These commands validate the same package containing both public suites and the new source.
