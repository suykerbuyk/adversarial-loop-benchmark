# Rebuttal: `walcodec` v2 plan findings

Point-by-point response to `plan/plan_findings.md`. The revised plan is `plan/plan.md`. Nothing was implemented.

## Major: no golden-bytes test of the Batch wire format; negative tests have no positive controls

Accepted. Every public positive Batch test is Encode-then-Decode, and the previous private table had the same shape: the only hand-built streams expected `ErrCorrupt`. An encoder and decoder that share a layout bug (little-endian `n`, `n` omitted, `n` counting bytes, `len` including inner CRCs, CRC covering the `len` field) would pass all of them. A decoder that rejects every Batch would also pass every negative case, including `TestBatchNestedPastDepth8`.

Fix: `TestPrivBatchGoldenBytes` is mandatory. It hand-builds a version-2 stream with self-contained `priv` helpers: a top-level Put, a `Batch{Put, Delete, Batch{Put}}`, and an empty Batch. `Decode` must return those records, and `Encode` of those records must return exactly those bytes. `TestPrivDepth8AcceptsHandBuilt` hand-builds the 8-wrap stream in the same shape as the public 9-wrap stream and asserts `Decode` accepts it; it does not call `Encode`. Every negative private test first decodes its unmodified base stream successfully and only then applies the single defect.

## Minor: private and fuzz files do not stand alone, and `FuzzDecode` can collide

Accepted. The hidden grader may replace or remove the shipped public test files. A private file that calls `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `beU32`, `mkFrame`, `recEq`, or `recEq2` then fails to compile, and every test fails. `FuzzDecode` is a generic name; a grader file that defines its own `FuzzDecode` is a duplicate-symbol compile failure.

Fix: `walcodec_v2_internal_test.go` and `walcodec_fuzz_test.go` must not refer to any identifier defined in the shipped test files. They copy the bytes and helpers they need. Every top-level identifier in those files, including the fuzz target `FuzzPrivDecode`, carries the `priv` prefix. The fuzz seed list no longer says to reuse a stream from `TestBatchNestedPastDepth8`; the fuzz file builds that stream itself.

## Minor: encoder section weakens v1's size check for Put and Delete

Accepted. v1 `appendPut` and `appendDelete` reject `keyLen` / `valLen` / `bodyLen > maxFrameLen` before appending anything. The previous `appendFrame` pseudocode wrote the payload in place and only then checked `bodyLen`. A Put with a multi-GiB `Value` would be copied in full before `errTooLarge`, and a lost guard would let `uint32(keyLen)` wrap. The previous plan also said those functions stay, while `appendFrame` was taking over the envelope they currently write.

Fix: `appendPut` and `appendDelete` become payload-only writers. Put and Delete size their body up front and return `errTooLarge` before any append, then `appendClosedFrame` writes `len`, type, the payload, and the CRC. The running check after each child applies only to Batch.

## Minor: `minFrameLen = 9` sits next to `maxFrameLen` and counts a different unit

Accepted. `maxFrameLen` limits the `len` field (type+payload; the CRC is outside `len`). The smallest legal `len` field is 1. A constant named `minFrameLen` equal to 9 invites `frameLen < minFrameLen` against that field, which would reject legal frames.

Fix: the constant is `minWireFrameBytes = u32Size + 1 + crcSize`, with a comment that its unit is the full on-wire frame including CRC. It is used only in the Batch `n` bound. The plan forbids checking it against the `len` field.

## Nit: depth convention written three ways, and `Encode`'s starting depth unstated

Accepted. `if depth > 8` in `decodeStream` was dead: `decodeStream` is only entered at depth 0 or at a depth already accepted, and Batch payloads use their own loop. `depth+1 > 8` appeared separately in the decoder and the encoder, and the plan never said `Encode` starts at 0.

Fix: one helper, `childDepth(parent) (int, bool)`, using `maxBatchDepth`. Both encoder and decoder call it. The dead `decodeStream` check is deleted. `Encode` calls `appendFrame(dst, rec, 0)` for each top-level record.

## Nit: `appendFrame` close check was muddled

Accepted. `bodyLen` cannot be 0 because the type byte is always appended, and the depth error returns at `childDepth`, not at the size check.

Fix: the close check is only `bodyLen > maxFrameLen` → `errTooLarge`. Put and Delete never reach that line; they reject before writing.

## Nit: fuzz oracle compared records only

Accepted. The format is canonical: exact consumption, a fixed field order, and no optional encodings. A records-only comparison misses a decoder that accepts a non-canonical layout and an encoder that does not reproduce the bytes it decoded. The previous wording also never failed the case when `Encode` returned an error.

Fix: if `Decode(b)` succeeds, `Encode(recs)` must return `err == nil`, the first byte of the result must be `2`, and the result must equal `b` byte for byte when `b[0] == 2`. When `b[0] == 1`, the result must equal `b` with only the first byte changed to `2`. The fuzz target is `FuzzPrivDecode`.
