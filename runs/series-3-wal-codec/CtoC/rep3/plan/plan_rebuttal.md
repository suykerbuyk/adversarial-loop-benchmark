# Rebuttal / response to plan_findings.md (p2_r1)

All nine findings are accepted. None are disputed. Section references are to the revised `plan.md`.

**F1 (major): Encode's over-depth error does not wrap ErrCorrupt. Accepted.**
The amendment's sentence "a deeper nest is ErrCorrupt … The encoder enforces the same cap" isn't
limited to Decode. The "Encode errors are not ErrCorrupt" policy was my own v1 choice, so it can't
override that sentence.
- The leaf error is now `fmt.Errorf("%w: %w", ErrCorrupt, errNestingTooDeep)`, with the index path
  wrapped around it using `%w`. Both `errors.Is` checks hold.
- Internal test 6.1 is flipped to assert both.
- The Encode doc comment states that only the depth error wraps ErrCorrupt, and why (§2 item 2, §3 [F1]).
- Size and unsupported-type errors keep the v1 behaviour, because no v2 text changes them.

**F2 (minor): the capacity hint is amplified across overlapping nested regions. Accepted, fixed
with option (b).**
- The hint is now `min(n, batchHintCap=16)`, and `append` grows the slice from there.
- Records are appended only after a child decodes, and each record corresponds to a distinct frame
  header. Total slice memory is therefore O(real frames), whatever `n` claims at any level.
- Option (a), a header pre-scan, was not chosen. It adds a second header walk whose checks would
  have to stay in sync with `readFrame`, which is the same two-sources-of-truth problem F3 raises.
- `minFrameSize` is removed.
- A new test (§6.6) builds the 8-level chain, each level claiming `n=0xFFFFFFFF`, around a 1 MiB Put.
  It bounds TotalAlloc to < 3× the input. Revision 1 would fail this test.

**F3 (minor): two independent definitions of the wire layout in the encoder. Accepted.**
- `finishFrame` now returns an error. It recomputes `len` from the bytes actually written and
  refuses (`errRecordTooLarge`) to backpatch anything above `maxFrameLen`. The cap is therefore
  enforced on real output, not just on pass-1 arithmetic.
- After pass 2, Encode checks `len(out) == total` and returns `errEncoderInvariant` with no bytes on
  a mismatch.
- Together these guarantee that the encoder never emits a frame over the cap, and they catch any
  layout drift.
- `finishFrame`'s new branch is covered directly by §6.11.

**F4 (nit): the claim that `total > MaxInt` removes the `make` panic. Accepted.**
- The check now runs while summing and returns as soon as the limit is crossed.
- The plan now says that only 32-bit is fully fixed. On 64-bit, totals between the runtime's maximum
  allocation size and MaxInt can still panic in `make`.
- I'm not adding a stream-size cap, because the spec has none. The residual is documented in code (§3 [F4], §8).

**F5 (nit): Batch region base is unspecified. Accepted.**
- The formula is pinned: `regionBase = absOff + lenSize + 1 + countSize` (= `absOff + 9`), and inner
  frames are at `regionBase + pos`.
- The offset each batch-level error reports is listed in §4.
- §6.5 asserts the exact `(offset N)` for an inner CRC error two levels deep, and for a
  "fewer frames" error.

**F6 (nit): the inner-len > 16 MiB test doesn't isolate the cap check. Accepted, and the test is
fixed instead of the claim being dropped.**
- `readFrame` checks `len <= cap` *before* "enough bytes", so a working cap check produces an
  "exceeds cap" message and a deleted one produces "truncated frame".
- §6.4 now asserts that the error text contains "exceeds cap". The test therefore fails if the
  inner cap check is removed.
- The test comment documents this dependency on check order.

**F7 (nit): stale v1 doc comments. Accepted.**
§2 now lists every comment to rewrite:
- the package doc;
- the Encode doc (both sentences);
- the `version1` const comment;
- the `decodeFramesV1`/`decodeRecordV1` names, which are renamed;
- the Decode doc;
- the comments for `beginFrame`, `grow` and `finishFrame`.

**F8 (nit): internal test names could collide with public `Test*` names. Accepted.**
- §0 lists every identifier the two public files declare: 12 helpers and 17 tests.
- The internal file uses a `TestInternal*` / `FuzzInternal*` / `it*` naming convention, which can't
  collide with them.

**F9 (nit): the 100% coverage goal conflicts with the unreachable `default` arm. Accepted.**
- `appendFrame`'s `default` arm is now covered by a direct internal call (§6.12).
- `finishFrame`'s cap branch is covered by §6.11.
- The one remaining uncovered statement is the `errEncoderInvariant` return, which can't be reached
  while pass 1 and pass 2 agree. It is named in §7 and gets an in-code comment. Covering it would
  need a test-only hook in production code, which I chose not to add.
