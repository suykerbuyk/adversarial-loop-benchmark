# walcodec v2 amendment plan

## Baseline and deliverable

The implementation target is explicitly `worktree/walcodec.go`, created by carrying forward `snapshot-v1/walcodec.go` and extending it for v2. The worktree already contains `go.mod` and both public suites; the implementation must land in this package so both suites compile together. `snapshot-v2/` is not part of this deliverable and should remain untouched unless the harness later explicitly requires it. This plan specifies changes only; no implementation is being made in this phase.

The v1 implementation is the behavioral baseline. Preserve its framing and validation for v1 streams, with the version behavior and new Batch support changed only as the amendment permits.

## Changes

1. **Public record type and constants.** Preserve `Record`, `Put`, `Delete`, `Encode`, `Decode`, and `ErrCorrupt` names/signatures. Add `Batch{Records []Record}` with value-receiver `Kind() uint8` returning 3. Define named shared constants for encode version 2, accepted decode versions 1 and 2, `maxFrameLen = 16 << 20`, and `maxNestDepth = 8`; use the same frame/depth limits in both directions. Do not add exported symbols or sentinels. `Decode` constructs `Batch` values, not pointers.

2. **Encoder version and frames.** `Encode` always starts with version byte 2, including empty and Put/Delete-only streams; empty input remains a one-byte stream. Keep each frame as big-endian `len`, `type + payload`, and CRC32-IEEE over `type + payload`. Keep existing Put/Delete field encodings and nil/empty Value equivalence. Update stale v1-only comments and replace the single-version assumption with separate encode and accepted-decode version definitions.

3. **Recursive Batch encoding.** A Batch payload is a big-endian uint32 count followed by exactly that many complete inner frames, with no nested version byte. Encode children recursively in order, enforcing the same 16 MiB body-length cap at every level. Prefer writing frames into the destination in place and backpatching their lengths/CRCs, including Batch frames, rather than assembling full child streams in temporary buffers. Check all field/count/frame arithmetic in uint64 before narrowing or appending. An over-depth encode returns an ordinary unexported encoding error (or wraps the existing `ErrCorrupt`); it must not introduce a new exported error. Unsupported records and nil records return errors, not panics.

4. **Depth convention and early checks.** The root stream has depth 0. When a Batch introduces its contents, enter depth + 1. Permit sub-stream depths 0 through 8; reject entry to any depth greater than 8. In both encoder and decoder, check the proposed sub-stream depth immediately on Batch entry, before walking children, reading the inner count, or returning success for an empty Batch. This makes empty Batch handling symmetric at the boundary and prevents a self-referential `Records` slice from recursing until stack exhaustion: the encoder rejects it at the depth check.

5. **Record and nil handling.** Preserve the v1 acceptance of `Put`, `Delete`, `*Put`, and `*Delete`; accept `Batch` and, consistently, non-nil `*Batch`. Reject nil interfaces and typed nil pointers (including nil `*Batch`) at any nesting level with an ordinary encode error, without dereferencing them. Decoding returns value forms `Put`, `Delete`, and `Batch`.

6. **Decoder version gate and bounded frames.** Validate the first byte before inspecting frames. Accept only versions 1 and 2; every other version, including 0, 3, and 255, returns `ErrCorrupt`. A v1 stream accepts only Put/Delete; Kind 3 remains corrupt in v1. Parse using a function scoped to the current stream slice, so an inner parser cannot see sibling or outer bytes. Before slicing or checksum calculation, require a complete 4-byte length, `1 <= len <= 16 MiB`, and body plus four CRC bytes within the current slice. Use uint64 for bounds additions/comparisons before converting to `int`. Verify CRC before interpreting each body. Any malformed input returns an error matching `ErrCorrupt`.

7. **Batch decoder exactness and allocation safety.** For Kind 3 in v2, require the four-byte count. Before allocating or iterating based on an untrusted count, reject `n > (len(payload)-4)/13` as `ErrCorrupt` (13 bytes is the smallest possible complete frame: 4-byte length, 1-byte type, and 4-byte empty key-length payload plus 4-byte CRC); also reject a missing count prefix. Do not preallocate a result slice from unchecked `n`. Decode exactly `n` inner frames from the remaining bounded Batch payload, at depth + 1, and require exact consumption with no trailing bytes. This rejects too few or too many frames, malformed/truncated inner lengths, and trailing bytes. A decoded empty Batch may use a nil `Records` slice; nil and empty Batch input both encode as count zero, and callers should treat these representations as equivalent, as for nil/empty Put.Value.

## What stays the same

- Existing exported signatures, Put/Delete fields and Kind values, record order, and duplicate-key retention.
- The frame layout, big-endian uint32 encoding, CRC32-IEEE coverage, and independent 16 MiB frame cap at every nesting level.
- Empty top-level encoding as a version-only byte, and the established nil/empty Put.Value equivalence.
- `ErrCorrupt` as the decoder's malformed-stream sentinel and all v1 decode behaviors when given version-1 bytes.
- No new exported error or other public symbol beyond `Batch`.

## Rule-to-implementation and validation map

| Rule or boundary | Planned implementation and validation |
| --- | --- |
| Implementation is delivered in tested package | Create `worktree/walcodec.go` from `snapshot-v1/walcodec.go`; leave `snapshot-v2/` alone unless harness requirements say otherwise. |
| Batch Kind 3 and frozen surface | Add Batch value type/method; retain all existing names and signatures; assert Kind and value-form decode. |
| Batch wire payload and exact count | Encode count plus complete frames; test correct count, too-large and too-small counts, and trailing bytes. |
| Recursive Put/Delete/Batch support | Recursive v2 encode/decode; round-trip simple, nested, and mixed records with order preserved. |
| Frame size at every depth | Share named cap constant; test oversized inner frame as well as existing outer oversize case. |
| No reads across enclosing payload | Parse each nested stream from its own slice; construct a valid-CRC outer Batch whose inner length runs beyond its payload and require `ErrCorrupt`. |
| Depth convention and empty substreams | Check depth on entry before count/loop; test exactly 8 nested Batches succeeds on both encode/decode, 9 fails on both, and an empty Batch crossing into depth 9 is rejected. |
| Encoder recursion safety | Early depth guard before recursion; test a self-referential Batch Records slice returns an error without panic. |
| Version rules | Encode version 2; test Decode rejects 0, 3, and 255 before frames; test Kind 3 in a v1 stream returns `ErrCorrupt`. |
| v1 compatibility | Decode the public fixed version-1 fixture to Put/Delete; fixture CRC flip and truncation remain `ErrCorrupt`, independent of Encode's v2 byte. |
| Nil and pointer behavior | Preserve non-nil Put/Delete pointer support; support non-nil *Batch; test nil interface and typed nil pointers at top level and nested levels error without panic. |
| Empty Batch representation | Test nil and empty Records encode identically and decode as an empty Batch with nil Records. |
| Public suite coexistence and deliverable checks | Keep implementation helper names distinct from both test files (`be2`, `mkFrame2`, `recEq2`, `beU32`, `mkFrame`, `recEq`, etc.). Run gofmt, `go build`, `go vet`, and both public test files together in `worktree/` as acceptance checks. |
