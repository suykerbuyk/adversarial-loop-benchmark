# walcodec v2 amendment plan

## Baseline and target

`worktree/` contains `go.mod` and both public test files, but no implementation source. The preserved v1 implementation is `snapshot-v1/walcodec.go`; it is a read-only reference for wire and error behavior. The implementation target is `worktree/walcodec.go`, seeded from that snapshot and then extended for v2. The amendment and public tests are `amendment.md` and `instrument/walcodec_v2_public_test.go`. Prior adversary findings are addressed point by point in `plan_rebuttal.md`.

## Changes

1. **Record type and surface.** Add `Batch{Records []Record}` and `func (Batch) Kind() uint8 { return 3 }`. Preserve the names and signatures of `Put`, `Delete`, `Record`, `Encode`, `Decode`, and `ErrCorrupt`. Keep the existing `ErrUnsupportedRecord` and `ErrRecordTooLarge` exports as-is; they are present in the v1 implementation and are not removed or repurposed.
2. **Versioned encoding.** `Encode` writes version byte 2 for all inputs, including empty and Put/Delete-only streams. Preserve existing Put/Delete payloads and frame layout. Batch payload is a big-endian uint32 count followed by exactly that many standard frames, recursively encoded in order. Only the concrete `Put`, `Delete`, and `Batch` value types are accepted, consistent with v1's rejection of pointers and other `Record` implementations. Unsupported records wrap `ErrUnsupportedRecord`; oversized bodies wrap `ErrRecordTooLarge`.
3. **Bounded decode by version.** `Decode` checks the version before inspecting any frame, accepts only 1 and 2, and wraps `ErrCorrupt` for other values. A v1 stream permits only Put/Delete; a type-3 frame in v1 is corrupt. A v2 stream permits Put/Delete/Batch. Both versions retain v1 frame, CRC, payload-length, ordering, and duplicate behavior.
4. **One bounded frame parser.** Use a shared frame parser for top-level and nested streams, passing the bounded stream slice and its absolute base offset. It checks that the length field exists, body length is nonzero and no greater than 16 MiB, and the complete body plus CRC fits inside the current slice before slicing or allocating. CRC32-IEEE covers type plus payload. Errors at any nesting level wrap `ErrCorrupt` and report absolute stream byte offsets via the base offset.
5. **Batch count and exact consumption.** A Batch payload must have at least four bytes for `n`; after that, `n` complete inner frames must consume the payload exactly. Reject `n > (len(payload)-4)/9` early, since each frame requires at least 9 bytes (length, type, CRC). Do not allocate capacity from untrusted `n`; either append with zero initial capacity or cap any hint to the minimum-frame bound above. This makes huge counts fail as corrupt without a count-driven allocation. Reject undersupplied counts, excess well-formed frames, trailing bytes (including after `n=0`), and payloads shorter than four bytes.
6. **Depth convention shared by both directions.** Top-level is depth 0. When any Batch is encountered, check the child-substream depth `depth+1` at the point of entering its sub-stream, regardless of its record count; reject if it exceeds 8. Thus an empty Batch that would open depth 9 is rejected too. Eight nested Batches are valid; nine are invalid. Encoding applies the same check before descending, so it cannot emit a stream its decoder rejects.
7. **Encoder sizing and emission.** Use one recursive `frameBodySize(record, depth)` helper as the shared source of supported-kind, body-size, uint32-range, and depth decisions; it computes Batch body size as four count bytes plus the sizes of child frames (body plus eight framing bytes each). Validate the 16 MiB body cap before any uint32 cast or emission. The size pre-pass re-walks subtrees, with at most nine traversals per record along the depth bound. Emission then uses a single per-kind helper to append payload, length and CRC, avoiding independent size and payload type switches drifting apart. Batch count and body-length casts occur only after bounds have been checked.
8. **Decoded empty Batch representation and comments.** Initialize decoded record slices as non-nil empty slices, including `Batch{Records: []Record{}}` for `n=0`, matching the top-level empty-slice convention. Update package, `Encode`, `Decode`, and `ErrUnsupportedRecord` comments to describe v2, accepted record types, and v1/v2 decode support.

## What stays

- Put/Delete frame and payload formats, CRC algorithm, big-endian integers, duplicate retention, and record ordering stay unchanged.
- The 16 MiB limit remains on each frame's type-plus-payload body at every nesting level.
- Empty `Encode` remains a version-only stream; decoding nil or empty input remains corrupt. Zero-length and nil Put values continue to decode as empty values.
- Existing error sentinels and exported v1 signatures are retained. Malformed encoded streams, including unsupported versions and invalid nested structures, return errors matching `ErrCorrupt`.

## Rule-to-design mapping and verification plan

| Rule | Design and check |
| --- | --- |
| Batch Kind 3; Put/Delete/nested Batch members | Add Batch and recursive v2 encode/decode; public Batch and nested round trips. |
| Count followed by exactly n framed records | Bounded parser, early minimum-frame count bound, and exact end-position check; add malformed count smaller/larger than actual frames, trailing frame/bytes (including `n=0`), and under-four-byte payload cases. |
| Every frame capped at 16 MiB | Check length before slice/allocation at each depth; retain v1 oversize test and cover inner-frame length cap. |
| Inner frame cannot pass Batch boundary | Parse against only the Batch payload slice and preserve absolute base offset; add a hand-built Batch with valid outer CRC and inner length crossing its payload while bytes in the following outer stream could otherwise satisfy that length. Expect `ErrCorrupt`. |
| Eight nesting levels legal, ninth illegal | Add an 8-level Encode/Decode round trip and assert 9-level Encode rejection; keep the public hand-built 9-level Decode rejection. Include an empty Batch at the over-depth boundary to verify depth is checked on entry regardless of `n`. |
| Encode writes version 2 | Assert version byte 2 for empty and Put/Delete-only output, plus public v2 round trips. |
| Decode versions 1 and 2 only, validate version first | Public v1 literal fixture and v2 suites; add version 0 rejection and version-1 type-3 rejection. Retain version 3 rejection. |
| CRC/truncation and v1 decoding regressions | Use the fixed v1 fixture for expected records, flipped CRC, and truncation; add an inner-frame bad CRC with a valid outer CRC. Run the unchanged v1 suite as well. |
| No unbounded count allocation; no self-incompatible Encode output | Add a huge-count, tiny-payload valid-CRC Batch rejection case and verify all emitted Batches obey count, size, exact framing, and depth constraints. |

Run the v1 and v2 Go test suites from `worktree/` after implementation. The v2 tests listed above supplement the supplied public suite where it does not exercise amendment edge cases.
