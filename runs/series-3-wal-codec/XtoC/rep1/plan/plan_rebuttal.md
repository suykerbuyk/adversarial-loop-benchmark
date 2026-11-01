# Response to prior plan findings

## Major findings

1. **Batch count can trigger unbounded allocation.** Addressed in plan sections “Batch count and exact consumption” and the verification table. The decoder rejects `n > (len(payload)-4)/9` and never uses untrusted `n` as an allocation capacity. A huge-count, tiny-payload valid-CRC case is included in supplemental checks.
2. **Important v2 rules were not covered by tests.** Addressed in the verification table: add an eight-level Encode/Decode success case and nine-level Encode failure; inner-boundary crossing under a valid outer CRC; count mismatch in both directions, trailing bytes including `n=0`, and short count payload; version 0 and v1 type-3 rejection; and bad inner CRC under valid outer CRC. Existing public tests remain in the suite.
3. **Implementation target was unclear and absent.** Addressed in “Baseline and target”: create `worktree/walcodec.go` from the preserved reference as needed; `snapshot-v1/walcodec.go` is read-only reference, and `worktree/` is the build target.

## Minor findings

4. **Encoder over-depth error and frozen surface.** The encoder will return an error wrapping the existing `ErrUnsupportedRecord` or `ErrRecordTooLarge` only for those corresponding cases; depth overflow is an error wrapping `ErrCorrupt`, with a descriptive depth message. No exported sentinel is added. Callers can distinguish the depth cause from other `ErrCorrupt` cases through the message; `errors.Is(err, ErrCorrupt)` remains true.
5. **Depth check for empty Batch.** The plan pins the check to entry into every Batch child sub-stream, before inspecting its count or children, on both encode and decode. A depth-9 empty Batch is rejected.
6. **Sizing strategy and integer casts.** Use a recursive `frameBodySize(record, depth)` pre-pass, then emission; document bounded repeated subtree traversal (at most nine depth levels). Enforce frame/count size bounds before uint32 casts.
7. **Parallel encode switches.** The size pre-pass is the shared supported-kind and sizing authority; a single per-kind emitter appends payload, while a common framing function writes length and CRC. This avoids separate size and payload switches.
8. **Nested error offsets.** The shared parser receives each bounded slice's absolute base offset, so errors from nested frames retain absolute stream byte offsets.
9. **Empty decoded Batch representation.** `n=0` decodes to a non-nil empty `[]Record{}`, matching the top-level convention.
10. **Stale comments.** Update package documentation, Encode and Decode comments, and ErrUnsupportedRecord's comment for Batch and v1/v2 behavior.
11. **Existing extra sentinels.** Keep `ErrUnsupportedRecord` and `ErrRecordTooLarge` unchanged; they remain in the exported surface alongside the amendment's named v1 symbols.
