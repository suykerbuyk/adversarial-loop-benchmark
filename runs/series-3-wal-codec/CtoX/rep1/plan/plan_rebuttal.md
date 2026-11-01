# Response to plan_findings.md (round 2, phase 2)

## Summary of the findings
The round-2 adversary report raises **no findings**. It states: "No blocker or major issue
remains." It confirms three points:
1. Each nested parser receives only its enclosing payload, which closes the round-1 boundary concern.
2. Frame CRC validation comes before payload dispatch.
3. Three-index slices cap capacity, so an accidental over-slice panics instead of reading a sibling
   or outer frame.

It also confirms that the proposed implementation path covers the public v2 cases and the fixed
v1-fixture regression.

## Point-by-point
- **Boundary / no read into sibling or outer frame**: **accepted as resolved.** No change. The
  mechanisms are still in plan §2.2 (the `readFrame` contract, the `len`-only bounds checks, and
  `cap == len` capping). The mandatory overrun tests are still in §5.
- **CRC-before-dispatch**: **accepted as resolved.** No change. It stays step 5 of `readFrame` and
  is marked as an invariant, with a dedicated test in §5.
- **Three-index slice capping**: **accepted as resolved.** No change (§2.2 step 4 and `decodeBatch` step 2).
- **Public cases and the v1 fixed-fixture regression**: **accepted as resolved.** No change. I
  independently re-verified that the fixture parses as two frames: len 17 / type 1 / crc
  0x20745b79, and len 9 / type 2 / crc 0x93aa4197. Both CRCs match, and the frames consume the
  buffer exactly, so test cases 3 and 8 follow the v1 decode path in plan §4.

## Changes made in plan.md (revision 3)
- Header and status updated to revision 3. They record that round 2 found nothing and that there is no design change.
- §0 notes that its starting-point facts were re-checked. The worktree still has only `go.mod`
  and the two public test files, so step 0 (copy the v1 sources from `snapshot-v1/`) still applies.

There are no other edits. The design, the rule-to-mechanism table (§3), the test plan (§5), and the
verification checklist (§6) are unchanged from revision 2.
