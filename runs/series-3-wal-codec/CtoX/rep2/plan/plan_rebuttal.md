# Rebuttal: phase-2 plan, round 3

The current `plan_findings.md` has one finding:

> No blocker or major issue remains based on the amendment, v1 specification, and shipped public
> tests. The recursive decoder design bounds every inner frame to its enclosing payload, applies the
> same length cap and CRC rules at each level, enforces exact batch consumption and the specified
> depth convention, and retains version-1 decoding. The two-pass encoder validates nested frames and
> the depth cap before writing version 2.

## Point-by-point

1. **"No blocker or major issue remains."** Accepted. No blocker, major, minor, or nit items were
   raised, so there is nothing to dispute and no required fix.
2. **Each property the reviewer confirmed is kept as is:**
   - *Inner frames bounded by the enclosing payload:* `decodeFrame` gets a sub-slice of the outer
     frame's body, so it can never read a sibling or outer frame (§2, "No read past").
   - *Same cap and CRC at every level:* the same `decodeFrame` (cap → zero → truncation → crc) is used
     at every depth.
   - *Exact batch consumption:* `decodeBatch` steps 4, 6, 7.
   - *Depth convention:* top level = 0, sub-stream depth `d+1 > 8` is rejected before `n` is read, and
     the encoder uses the same predicate.
   - *Version-1 decoding retained:* versions 1 and 2 are both accepted; a Batch in a v1 stream is
     rejected, matching a v1 decoder.
   - *Two-pass encoder:* it validates and sizes first, checking depth and cap before any byte is
     written, then writes version 2.

## Self-review changes made anyway (r3, non-substantive)

I checked the plan again against `snapshot-v1/*.go`, `spec-v1.md`, the amendment, and both public
test files. The worktree copies are byte-identical to `instrument/`, and the worktree still has no v1
`.go` files, so §0 step 1 (restore from `snapshot-v1/`) still applies. I fixed three internal
inconsistencies, each marked (r3) in `plan.md`:

- **Stale reference fixed.** The "Exact consumption" rationale still pointed to "step 6 (empty
  `rest`)", a check r2 removed. It now says the fewer-than-`n` case is caught by `decodeFrame`'s
  truncated-header and truncated-frame checks (or earlier by step 4).
- **Unreachable encoder check removed.** The `len(Records) > math.MaxUint32` guard needed a `math`
  import that §0's stdlib list didn't include. It is also unreachable: the r2 early exit hits the
  16 MiB cap after at most ~1.86M children, each at least 9 bytes. The plan now relies on the cap,
  explains why `uint32(len(Records))` is lossless, and keeps the dependency list accurate. A test for
  a large child count was added.
- **Edge case stated.** An empty Batch at the 9th nesting level is `ErrCorrupt` on decode and an error
  on encode. The depth check depends on the sub-stream's depth, not on `n`, which follows the
  amendment's wording. An internal test case was added.

Rule coverage is unchanged. The rule-to-mechanism table (§3) and the public-test walkthrough (§4)
still hold.
