# Rebuttal / response — phase 2, round 3

## Round-2 findings: none remaining

`plan_findings.md` reports no blocker, major, or minor issue. It also confirms that the round-1
minor finding (the ambiguous allocation contract) is resolved by §1a and the matching test plan.
**Nothing to dispute and nothing to fix. The design is unchanged.**

## Independent re-verification done this round (not prompted by the adversary)

I did not take the clean review on trust. I checked the plan again against the primary sources:
- **`snapshot-v1/*.go`:** the function names, error sentinels, constants, check order
  (`short header -> oversize -> len==0 -> truncated -> crc`), the lazy `recs` growth, and the
  two-pass Encode with `addSize`, as §1 and §2 describe them, all match the code.
- **The public tests:** `worktree/` copies equal `instrument/` (checked with `diff`). The v1
  `TestUnknownType` uses tag 99, and the v1 header says no case asserts that the version byte is 1.
  Each v2 case follows the path described in §4. For example, in public test 7 the 9th Batch sits
  at depth 8, its sub-stream would be at depth 9, and the result is `errTooDeep`.
- **The depth convention** was worked out again from the amendment text. The decoder and encoder
  both use `depth+1 > 8`, with top level = 0. So 8 levels are accepted and 9 are rejected.

That check led to two small additions, neither of which changes the design:
1. **§2.3, size-pass bound for shared `*Batch` subtrees.** Earlier rounds covered self-reference,
   which the depth cap stops. They did not cover DAG fan-out, where one `*Batch` is referenced
   many times. The new note shows the work is still bounded: each frame visit adds ≥13 bytes,
   and the batch sum stops at 16 MiB. So no cycle detection or memoization is needed.
2. **§2.4 test 11, canonical-bytes fuzz oracle.** Any accepted input must re-encode to exactly
   itself, with the version byte set to 2. This catches encoder or decoder asymmetries that a
   records-only round-trip would miss, such as a wrong back-filled `len` that the decoder happens
   to tolerate.
