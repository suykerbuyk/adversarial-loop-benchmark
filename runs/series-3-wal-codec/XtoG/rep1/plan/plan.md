# walcodec v2 amendment plan

## Baseline and scope

The inspected v1 implementation is `snapshot-v1/walcodec.go`. `worktree/` contains the v1 and v2 public suites and `go.mod`, but no implementation source. This is a plan only; implementation files will not be changed here. Preserve the existing exported names and signatures for `Put`, `Delete`, `Record`, `Encode`, `Decode`, and `ErrCorrupt`, and add only the exported `Batch` type.

## Changes

1. Add `Batch{Records []Record}` whose `Kind() uint8` is 3. Preserve Put=1 and Delete=2.
2. Change `Encode` to emit version byte 2 for every input, including empty and Put/Delete-only inputs. Keep all existing field encodings and framing rules.
3. Refactor encoding and decoding into recursive, depth-aware record-stream helpers. Each frame at every depth is `[len][type][payload][crc]`; `len` counts type plus payload, and the CRC-32 IEEE covers type plus payload. Keep the 16 MiB maximum for each frame body.
4. Encode a Batch payload as a 4-byte big-endian count followed by exactly that many complete inner frames. Compute each encoded frame body size recursively before allocation. A Batch body size is `1 + 4 + sum(4 + innerBodyLen + 4)` for its outer type byte, count, and each inner frame's length, body, and CRC. Require that body size be at most 16 MiB before allocating/emitting the outer frame; apply the same check to every frame at every depth. Check counts and field lengths for uint32 representability. An empty or nil `Records` slice encodes as count zero, with the count field present.
5. Validate the input version before parsing frames. Decode version 1 with only Put and Delete accepted; decode version 2 with Put, Delete, and Batch accepted. Reject every other version as `ErrCorrupt`. In version 1, a kind-3 frame is an unknown kind: validate its frame bounds and CRC, then return `ErrCorrupt` without interpreting its payload as a sub-stream.
6. Decode a Batch using only its enclosing frame payload slice. Require the 4-byte count, decode exactly that many inner frames, and require exact payload consumption. Check every inner length, header, body, and CRC against the remaining bytes of that payload, never against a sibling or outer stream. Reject short counts, excess frames, trailing bytes, malformed record fields, bad CRCs, and lengths over the cap as `ErrCorrupt`.
7. Apply the precise depth rule symmetrically. Top-level stream is depth 0. Before decoding or encoding a sub-stream, if its prospective depth is greater than 8, return `ErrCorrupt` during Decode or the encode error sentinel during Encode, without processing that sub-stream. A Batch whose inner sub-stream is depth 8 is permitted only if those records are Put or Delete; a Batch at depth 8 cannot contain another Batch because that would create depth 9. Thus eight Batch levels are legal and nine are not.
8. Preserve distinct error classes. For encoding, use the existing encode-error sentinel behavior and do not wrap `ErrCorrupt`; apply it to nil `*Batch`, nil Record values at any depth, unknown concrete record types, fields larger than uint32, an oversized frame body, and over-depth nesting. For decoding, malformed data returns `ErrCorrupt` such that `errors.Is(err, ErrCorrupt)` succeeds, including bad versions/kinds, truncated inner frames, count mismatches, trailing bytes, and invalid CRCs.

## What stays

- Keep the v1 exported surface and the Put/Delete wire formats unchanged.
- Keep the version-byte plus zero-or-more-frame top-level format, big-endian uint32 lengths, 16 MiB body cap, and IEEE CRC-32.
- Preserve record order and duplicate keys. Decode nil and empty Put values as equivalent zero-length values.
- Keep the v1 decoder's corruption handling, but verify it against the fixed version-1 byte fixture, not by modifying the new version-2 Encode output.

## Rule-by-rule acceptance mapping

| Amendment rule | How the plan satisfies it |
| --- | --- |
| Frozen v1 API; add Batch | Retain all existing names/signatures, add only `Batch` with kind 3. |
| Batch wire shape and recursion | Count is a big-endian uint32 and the payload contains exactly that many full frames; recursive helpers support nested Batch records. |
| Exact inner stream bounds | Parse inner frames against the enclosing payload slice and reject truncation, excess/missing frames, or trailing bytes. |
| Per-frame 16 MiB limit | Enforce on each frame body at all nesting levels. Encoding computes complete body size including the outer type, count, and every inner frame's length/body/CRC before allocation. |
| Depth cap | Top-level depth 0; check prospective sub-stream depth before processing it; depth 8 is allowed, depth 9 returns the appropriate error. The same rule is used by Encode and Decode. |
| Version behavior | Encode always emits 2. Decode validates version first, supports v1 Put/Delete and v2 Put/Delete/Batch, rejects all other versions. A v1 Batch-kind frame is CRC-checked then rejected as unknown without recursion. |
| Error classification | Encode validation failures use the encode sentinel and do not wrap `ErrCorrupt`; malformed streams consistently satisfy `errors.Is(err, ErrCorrupt)`. |
| Empty Batch | Nil and empty record slices encode with a present zero count and decode to a Batch with zero records. |
| v1 regression | Decode the fixed version-1 fixture and preserve its records, CRC failure, and truncation failure. Keep all v1 public tests alongside the v2 suite. |

## Public suite coverage

The v2 public cases cover Batch and nested Batch round trips, fixed-fixture v1 decoding, mixed top-level records, truncated Batch handling, version 3 rejection, nine-level nesting rejection, and v1 fixture records/CRC/truncation regression. The v1 suite continues to cover Put/Delete round trips, unknown types, CRC and truncation errors, oversize lengths, empty streams, duplicate ordering, and nil/empty values. The explicit version-1 Batch-kind behavior, eight-level boundary, empty Batch, and detailed encode error classes should also be retained as implementation constraints even where the supplied public tests do not directly exercise them.
