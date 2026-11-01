# walcodec v2 amendment plan

## Scope and baseline

The amendment extends the v1 implementation in `snapshot-v1/walcodec.go`. The `worktree` currently contains the v1 and v2 public suites and `go.mod`, but no implementation file; implementation work will port the existing v1 wire behavior into the worktree and extend it recursively. This plan makes no code changes.

Keep the exported v1 surface and signatures (`Put`, `Delete`, `Record`, `Encode`, `Decode`, `ErrCorrupt`) and add only `Batch{Records []Record}` with `Kind() uint8 == 3`. Preserve the v1 `ErrType` encoding error. The format uses big-endian uint32 fields and CRC32-IEEE.

## What changes

1. **Version and kind handling.** `Encode` emits version byte 2 for every stream, including empty and Put/Delete-only inputs. `Decode` checks the version before inspecting any frame, accepts exactly 1 and 2, allows only Put/Delete in v1, and allows Put/Delete/Batch in v2. Unsupported or missing versions and disallowed/unknown frame kinds return errors matching `ErrCorrupt`.

2. **Frame bounds and CRC validation.** At every stream depth, frame `len` is the byte count of type plus payload, excluding the four-byte CRC; the inclusive limit is `16 << 20`. Decode checks the cap and verifies that the complete body and CRC fit in the current enclosing slice before slicing. It computes and validates CRC32-IEEE over type plus payload immediately after establishing those bounds, before inspecting the type or parsing/recursing into payload. Any failure is `ErrCorrupt`. Encode rejects oversized field lengths, counts, arithmetic, or type-plus-payload frame bodies with `ErrType`; the cap applies to a Batch's complete count-prefixed payload and outer frame as well as every inner frame.

3. **Batch encoding.** Encode Batch as a payload containing a big-endian uint32 record count followed by exactly that many complete frames. Recursively support Put, Delete, and Batch, retaining order. Nil `Batch.Records` is an empty list and encodes as a zero count. Before conversion or allocation, check key/value lengths, record counts, and all length arithmetic for uint32 overflow and frame-cap violations. Accept only concrete value records `Put`, `Delete`, and `Batch`; reject nil interfaces, pointers (including nil pointers), and other implementations with `ErrType`. Apply the nesting-depth rule before encoding a Batch's child stream, even for an empty Batch. If any Encode operation fails at any recursive level, return `(nil, err)`, never a partial stream.

4. **Bounded recursive decoding and count safety.** Parse each frame wholly within the slice for its containing stream. For Batch, require at least four payload bytes for the count. Before allocating a result slice or doing work proportional to the declared count, prove the count is feasible: each complete inner frame has a minimum size of 9 bytes (four-byte length, one-byte type, four-byte CRC), so reject `count > remainingPayload/9` with `ErrCorrupt`. Do not preallocate `[]Record` from the count; append each record only after successfully parsing its bounded frame. Parse exactly the declared number of complete inner frames, and require exact payload consumption. This count feasibility check and incremental growth apply independently at every nesting level. An inner length that crosses its containing Batch payload is truncated and returns `ErrCorrupt`; it cannot be satisfied by sibling or outer bytes.

   Reject short headers, over-cap lengths, absent body/CRC bytes, bad CRCs, malformed fields, unknown or version-disallowed kinds, impossible counts, fewer frames than declared, and trailing bytes (including bytes after a zero count). Require exact payload exhaustion for Put and Delete too. Append concrete value records, never pointers. Detach decoded data from the caller's input: copy each key's bytes before converting to string, and copy every Put value into owned storage, including at nested depths; represent zero-length values as a length-zero slice.

5. **Depth limit.** Top-level stream depth is 0. Entering a Batch's inner stream increments depth; allow sub-stream depths 0 through 8, and reject an attempted depth 9 before parsing or encoding its children, even when the count is zero. Decode returns an error matching `ErrCorrupt`; Encode returns an error matching `ErrCorrupt` for depth violations, with any failure returning a nil output buffer. Other unsupported-type and size/overflow Encode failures return `ErrType`.

## What stays from v1

Keep Put and Delete wire layouts, frame order, duplicate-key behavior, CRC algorithm, exact payload validation, empty-stream semantics, and nil/empty value equivalence. Decode must accept the fixed version-1 fixture and preserve its records, CRC rejection, and truncation rejection. `Decode(nil)` and `Decode([]byte{})` remain corrupt. An empty Encode now returns exactly `{2}` as required by the version bump; it decodes to no records. A nil Put value encodes with value length zero, and a decoded zero-length value has length zero.

## Rule-to-change and public-test mapping

| Rule or acceptance case | How the plan satisfies it |
| --- | --- |
| Frozen v1 API plus new Batch type | Preserve all existing names/signatures; add `Batch{Records []Record}` and kind 3. |
| Nested framed Batch payload and recursion | Encode/decode shared complete frames recursively for Put, Delete, and Batch. |
| Exact count and no boundary escape | Enforce count feasibility, parse within the enclosing payload slice, parse exactly the count, and require exact consumption. |
| Count cannot trigger unchecked allocation | Reject count above remaining payload divided by 9 before allocation; grow decoded record slices only as inner frames are parsed. Repeat at every depth. |
| CRC before interpretation | Check CRC over type plus payload after length/bounds validation and before parsing any kind or nested payload. |
| Detached decoded fields | Copy key bytes before creating strings and copy value bytes for every record at every depth. |
| Frame cap at every level | Enforce inclusive 16 MiB type-plus-payload cap before decoder slicing; enforce it on every encoded frame, including outer Batch frames. |
| Depth convention | Check prospective inner depth before child work; depths 0–8 pass and depth 9 fails in both directions. |
| Encode error output | Every Encode error path, including recursive errors, returns a nil byte slice. |
| Version 2 writer, v1/v2 reader | Writer always writes 2; reader validates 1 or 2 before frames and gates allowed kinds by version. |
| Test 1: Put/Delete Batch round trip | Recursive Batch encoder/decoder preserves record values and order. |
| Test 2: nested Batch round trip | Recursive bounded parsing and encoding follow the shared depth convention. |
| Test 3: fixed v1 fixture decode | Decode the literal version-1 bytes through the v1 kind rules. |
| Test 4: mixed top-level round trip | Version 2 accepts all three kinds at top level in order. |
| Test 5: truncated Batch | Frame, CRC, field, and bounded-payload checks map truncation to `ErrCorrupt`. |
| Test 6: version 3 | Header validation rejects it before examining any frame. |
| Test 7: nine nested Batches | The decoder rejects the ninth inner-stream level with `ErrCorrupt`; encoder applies the same cap. |
| Test 8 and v1 suite | Fixed fixture checks round-trip records, CRC flip, and truncation; retain v1 checks for oversize length, unknown type, empty inputs, duplicates, and zero-length values. |
