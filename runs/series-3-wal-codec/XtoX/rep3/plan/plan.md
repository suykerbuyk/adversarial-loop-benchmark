# walcodec v2 amendment plan

## Starting point

The amendment and both public suites are present. `worktree/` contains the v1 public tests and `go.mod`, but no implementation source; the v1 implementation snapshot is `snapshot-v1/walcodec.go`. No `plan/plan_findings.md` exists, so there are no prior adversary findings to answer and no rebuttal file is needed. This is a design plan only; no implementation changes are part of this work.

## Changes

1. Add `Batch struct { Records []Record }` with value receiver `Kind() uint8` returning 3. Keep `Record`, `Put`, `Delete`, `Encode`, `Decode`, and `ErrCorrupt` names and signatures unchanged.
2. Have `Encode` write version byte 2 for every stream, including empty and Put/Delete-only streams. Extend record dispatch to accept `Batch` and encode its payload as a big-endian uint32 record count followed by complete inner frames. Inner frames use the existing frame format and CRC32-IEEE over type plus payload.
3. Refactor encoding into a recursive stream/frame helper that accepts the current sub-stream depth. The top-level call uses depth 0; encoding a Batch's records calls it at depth + 1 and rejects depth greater than 8 with `ErrCorrupt`. Apply the 16 MiB cap to each frame's type-plus-payload length at every depth, and check uint32 representability and aggregate-size arithmetic before allocation/appending.
4. Refactor decoding to validate the initial version byte before parsing frames, accepting only 1 or 2. The top-level parser uses depth 0 and receives an explicitly bounded byte slice. For each frame, validate the length cap and ensure the entire frame plus CRC fits that slice before taking frame/CRC subslices. Check CRC before decoding the type payload.
5. For version 1, retain only Put and Delete as accepted frame types, preserving the existing payload parsing and exact-consumption checks. For version 2, additionally decode type 3: read its uint32 count, parse exactly that many inner frames recursively from only the Batch payload, and require the inner parser to consume the payload exactly. Reject too few frames, extra frames/trailing bytes, malformed lengths, invalid inner records, or a nested sub-stream depth above 8 as `ErrCorrupt`.

## Rules and acceptance coverage

| Amendment rule | Planned satisfaction |
| --- | --- |
| Frozen v1 API plus `Batch` | Preserve all existing exported names/signatures; add only the exported Batch type and its `Kind` method. |
| Kind tags | Keep Put=1 and Delete=2; Batch returns 3. |
| Batch payload layout | Encode/decode count as big-endian uint32 and then exactly that many full framed records, including each frame's CRC. |
| Allowed inner record types and recursion | Version 2's recursive decoder/encoder supports Put, Delete, and Batch at any permitted depth; version 1 rejects Batch. |
| Frame cap at every level | Apply the existing 16 MiB cap to each frame's `len` before allocation or slicing, whether top-level or nested. |
| Nested frame cannot cross Batch boundary | Each recursive parse is passed the exact Batch payload slice; extent checks use that slice's length, so an inner frame cannot consume sibling/outer bytes. |
| Exact Batch consumption | Require the count-driven inner parse to end exactly at the payload boundary; missing counted frames and remaining trailing bytes are corrupt. |
| Depth convention and cap | Top-level is depth 0; each Batch enters depth + 1; reject a sub-stream depth greater than 8. Encoder uses the same rule, allowing at most eight nested Batches. |
| Version bump and compatibility | Encode always emits 2. Decode accepts 1 and 2, rejects all other/missing versions before reading frames. Version 1 permits only legacy tags. |
| Existing v1 decode behavior | Preserve ordered records, duplicate keys, CRC/truncation/oversize/unknown-type corruption handling, empty stream behavior, and Put/Delete payload semantics. Confirm compatibility using the fixed literal version-1 fixture, not Encode output. |
| Public v2 cases | Batch and nested Batch round trips exercise recursive encoding/decoding; mixed stream covers all top-level types; truncated Batch and depth-nine crafted stream exercise bounds; version 3 rejection checks early version validation; fixture tests cover legacy decoding, CRC flip, and truncation. |

## What stays

The wire framing remains big-endian `[len][type][payload][crc]`, with `len` counting type plus payload only and CRC32-IEEE covering those same bytes. Put and Delete payload layouts, record ordering, duplicate preservation, nil/empty value equivalence, one-byte empty stream, and `ErrCorrupt` semantics remain. Version 1 is retained as a decode-only compatibility format; only the version emitted by Encode changes.

## Verification plan

After implementation, run the v1 and v2 public suites together in the `walcodec` package. The v2 tests explicitly check empty/ordinary records where applicable, nested and mixed round trips, the fixed v1 fixture and its corrupt variants, truncation, version rejection, and depth overflow. Also review that frame-length rejection occurs before allocation and all recursive parsing remains bounded to its enclosing payload. No tests or implementation are run as part of this planning deliverable.
