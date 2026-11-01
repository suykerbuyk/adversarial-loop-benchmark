# Point-by-point response to plan findings

## Major findings

1. **Implementation location was not pinned down.** Addressed in “Baseline and deliverable” and the rule-to-validation map: create `worktree/walcodec.go` by carrying forward `snapshot-v1/walcodec.go`. The worktree is the package compiled with both public suites. `snapshot-v2/` is not required by the current harness and remains untouched unless harness requirements change.

2. **Untrusted Batch count could drive huge allocation.** Addressed in “Batch decoder exactness and allocation safety”: validate the 4-byte count and reject `n > (len(payload)-4)/13` before allocation or iteration. The decoder will not allocate capacity from unchecked `n`; the bound follows from the minimum 13-byte complete frame.

3. **Boundary test strategy was incomplete.** Added explicit planned checks for exactly 8 and 9 nesting levels on encode and decode, valid-CRC outer frame with a past-payload inner length, counts above/below the actual frames, trailing bytes, Kind 3 under v1, versions 0 and 255 (alongside 3), and an oversized inner frame.

## Minor findings

4. **Depth check needed to cover empty Batch substreams.** Specified an entry check before count parsing or child iteration in both directions. The validation map includes an empty Batch that would enter depth 9.

5. **Encoder over-depth error and frozen surface.** Specified an ordinary unexported encoding error or wrapping `ErrCorrupt`; no exported sentinel or other API addition is allowed beyond `Batch`.

6. **Pointer and nil records in nested slices.** Specified preserving v1 pointer support, accepting non-nil `*Batch`, and rejecting nil interfaces and typed nil record pointers at every level without panic.

7. **Encoder recursion on self-referential slices.** Specified the depth check before recursion. The planned self-referential Batch test requires an error without panic.

8. **Build checks and test-helper collisions.** Added gofmt, `go build`, `go vet`, and running both public suites together in `worktree/`; implementation helpers must not collide with names from either suite.

9. **Empty Batch decode representation.** Specified that nil and empty Records encode as count zero and Decode may return a Batch whose Records is nil; both are semantically equivalent. Added a planned validation case.

10. **Named shared limits.** Specified named `maxNestDepth` and `maxFrameLen` constants shared by encoder and decoder, as well as version constants.

11. **Stale v1 comments and single version constant.** Specified updating Encode/Decode comments and replacing the single-version assumption with separate encode and accepted decode versions.

12. **Overflow-safe bounds arithmetic.** Specified uint64 comparisons for field/count/frame arithmetic and decoder body-plus-CRC checks before converting to `int` or slicing.

13. **Batch frame construction should avoid repeated temporary copies.** Specified writing frames in place and backpatching length and CRC, including recursive Batch frames.

14. **Decoded Batch values.** Specified explicitly that Decode returns `Batch` values, not pointers.
