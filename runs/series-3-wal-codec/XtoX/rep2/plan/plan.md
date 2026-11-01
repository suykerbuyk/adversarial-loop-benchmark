# walcodec v2 amendment implementation plan

## Scope and inspected baseline

Add nested `Batch` records while retaining the v1 exported names and signatures for `Record`, `Put`, `Delete`, `Encode`, `Decode`, and `ErrCorrupt`. Add `Batch{Records []Record}` with `Kind() uint8` returning 3. `Encode` will write version 2; `Decode` will accept versions 1 and 2. Version 1 permits only Put/Delete, while version 2 permits Put/Delete/Batch.

The supplied `worktree` currently contains `go.mod` and the v1/v2 public test files, but no Go implementation source. The implementation work therefore needs to add the package source, using the v1 specification and public suite as the behavior baseline. No implementation is performed as part of this plan.

## Planned changes

1. **Record types and framing.** Implement the existing v1 record types and wire format, and add `Batch` with kind 3. Keep the common frame layout `[len uint32][type][payload][crc uint32]`, big-endian lengths, CRC32-IEEE over type plus payload, and a 16 MiB maximum `len`.
2. **Encoder.** Emit version byte 2, including for Put/Delete-only and empty inputs. Encode each frame in order. Encode a Batch payload as a uint32 child count followed by the complete child frames, recursively. Check field/count representability and each frame's length cap before constructing output; return an encode error for unsupported or invalid records and over-depth input. Start at stream depth 0 and increment for a Batch child stream, rejecting a sub-stream depth above 8.
3. **Decoder and version policy.** Reject missing or unknown versions before parsing any frame. Accept only 1 and 2. Use a shared bounded frame parser for top-level and nested streams. Check the 16 MiB cap before allocating or slicing based on a frame length; ensure frame body and CRC fit inside the current stream boundary; then verify CRC and parse the type/payload. Pass the version through recursion so version 1 rejects Batch and version 2 recognizes all three types.
4. **Batch decoding and resource bounds.** Require the four-byte child count. Before allocating child storage or looping according to the untrusted count, compare it with the number of complete minimum-size frames the remaining payload could contain: every frame requires at least 9 bytes (4-byte length, a `len` of at least 1 for the type, and 4-byte CRC), so reject if `n > remaining/9`. Parse children incrementally within only the Batch payload's remaining bytes, using bounded frame parsing; never permit a child body or CRC to reach a sibling/outer frame. Require exactly `n` frames and exact payload consumption, rejecting trailing bytes, missing children, and malformed frames as `ErrCorrupt`. The feasibility check prevents a tiny malicious payload with a huge count from triggering huge allocation or count-driven work.
5. **Depth and error mapping.** The top-level stream is depth 0. Each Batch child sub-stream uses depth + 1; reject depth greater than 8 during both encode and decode. All malformed wire data, including bad version, tag, count, bounds, CRC, payload shape, truncation, oversized frame, and excess depth, must produce an error matching `ErrCorrupt` from Decode.

## What stays

- Existing public names and signatures remain unchanged; `Batch` is the sole API addition.
- Put and Delete kinds and payloads remain `[klen][key][vlen][value]` and `[klen][key]` respectively.
- Frame layout, integer byte order, CRC coverage, and 16 MiB per-frame cap remain unchanged at every nesting depth.
- Stream and child record order, including duplicate records, is preserved. Nil and empty Put values both encode as length zero and decode to a zero-length value.
- Empty Encode input remains a version-only stream (now byte 2); `Decode(nil)` and `Decode([]byte{})` remain corrupt.
- `Encode` changing its version byte to 2 is intentional. V1 compatibility is verified using the fixed version-1 byte fixture, never by expecting current Encode output to start with 1.

## v2 rule and acceptance mapping

| Requirement | Planned satisfaction |
| --- | --- |
| Batch kind 3 and recursive contents | Add `Batch.Kind() == 3`; serialize/parse its count and nested frames recursively. |
| Batch count and exact framing | Read `n`, check minimum-frame feasibility before allocation/work, parse exactly `n`, and require the cursor to end at the Batch payload boundary. |
| Child cannot cross enclosing payload | Every recursive frame parser is bounded to the current sub-stream slice, which itself is only the parent Batch payload after its count. |
| 16 MiB cap at every depth | Enforce the same cap before length-driven allocation/slicing in every frame parser and before emitting every frame. |
| Top-level depth 0, maximum 8 | Increment depth for each Batch sub-stream and reject depth > 8 in both directions. |
| Encode writes version 2 | Initialize every encoded stream with 2, including streams containing only v1 record kinds. |
| Decode accepts v1 and v2 only | Validate the first byte before frame parsing; route v1 to Put/Delete-only types and v2 to all supported types. |
| v1 fixed-fixture compatibility | Decode the supplied literal version-1 fixture as the same Put/Delete sequence; retain its CRC and truncation errors. |
| Public case 1: Batch with Put/Delete round-trip | Recursive encoder and decoder preserve child order and values. |
| Public case 2: nested Batch round-trip | Apply the same recursive framing to nested children. |
| Public case 3: v1 fixture decodes | Exercise the version-1 decoder path with the literal fixture. |
| Public case 4: mixed top-level round-trip | Top-level framing handles Put, Delete, Batch, then subsequent records in order. |
| Public case 5: truncated Batch is corrupt | Bounded child parsing, exact consumption, and CRC checks return `ErrCorrupt`. |
| Public case 6: version 3 is corrupt | Reject unsupported version before inspecting frames. |
| Public case 7: depth beyond 8 is corrupt | Decoder rejects the sub-stream at depth 9; encoder likewise refuses to emit it. |
| Public case 8: v1 regressions via fixed fixture | Confirm fixture records, CRC-flip rejection, and truncation rejection independent of Encode's v2 version byte. |
| Existing v1 behaviors | Preserve field decoding, CRC/tag/truncation/oversize validation, ordering, empty stream semantics, and nil/empty value equivalence while accommodating v2 output. |
