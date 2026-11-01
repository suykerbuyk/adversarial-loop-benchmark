# Rebuttal: `walcodec` v2 plan findings

Point-by-point response to `plan/plan_findings.md`. The latest findings state that no blocker or major issue remains. The two earlier defects stay accepted and stay in the revised plan. One wording gap in the previous encode budget is tightened so the check-before-copy rule is exact.

## No blocker or major issue remains

Accepted. The depth convention, frame bounds, exact `n` consumption, version check, version-1 Kind 3 rejection, and the 16 MiB cap are unchanged in substance. The plan still matches the amendment and the eight public tests. No new exported symbol is added beyond `Batch`. v1 names and signatures stay frozen. `Encode` still writes version 2, and `Decode` still accepts the fixed version-1 fixture.

## major — Version-1 streams must reject Kind 3

Accepted, and kept. `Decode` accepts a version-1 stream only as Put/Delete. The shared frame reader carries `allowBatch`, set from the version byte and passed unchanged into every nested sub-stream. Kind 3 under version 1 is `ErrCorrupt` at every depth, including a Batch that would have been nested inside another Batch. CRC and length checks still run first so a corrupt or oversize frame fails for those reasons. A version-2 stream with the same Kind 3 frame remains legal, subject to the depth cap and the exact-consumption rule.

The fixed v1 fixture is unchanged: it contains only Put and Delete, so the stricter type check does not disturb tests 3 and 8.

## minor — Batch encode must not copy an unbounded scratch before the 16 MiB check

Accepted, and tightened. The previous revision charged children against a running room counter, but it still said a failing encode may copy at most one legal frame past the last byte that still fit. That leftover copy is unnecessary and weakens the v1 check-before-copy rule.

The revised plan sizes each child before copying it. `frameSize` uses the existing `frameLen` arithmetic for Put and Delete, and the same room arithmetic for a nested Batch. If `childSize > remain`, encode returns `ErrCorrupt` before allocating a scratch for that child and before copying its key, value, or nested frames. Scratch capacity for a Batch that is entered is reserved as the remaining room, which is at most `maxFrameLen`. Depth greater than 8 is rejected before any child walk. A failed encode still returns `nil, ErrCorrupt` and does not emit a partial stream.

Put and Delete keep their existing `frameLen` check before any key or value copy. The budget is an additional bound so a Put that is legal alone is still rejected, without copying, when it cannot fit in the enclosing Batch.
