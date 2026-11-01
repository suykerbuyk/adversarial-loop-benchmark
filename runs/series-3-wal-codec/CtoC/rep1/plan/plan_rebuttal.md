# Rebuttal: response to plan_findings.md (phase 2, round 1)

All 10 findings are **accepted**, and the revised `plan.md` includes them. Each change is tagged
[F1] to [F10] in the plan. Numbering follows the findings file in order: F1 is the major finding,
F2 to F6 are the minors, and F7 to F10 are the nits.

**F1 (major): the Encode depth-cap error should wrap ErrCorrupt.** Accepted.
- The amendment's depth section says "Decoding (and encoding) carries a nesting depth … depth
  greater than 8 is ErrCorrupt". That reasonably applies to Encode too, and wrapping still passes
  any plain non-nil check.
- Plan §2.2: the unexported `encodeError` has a `tooDeep` flag, and `Unwrap()` returns
  `ErrCorrupt` only when that flag is set.
- The Encode doc comment is rewritten to say that only the depth-cap error wraps ErrCorrupt, and
  why.
- I kept the other Encode errors (oversize, nil, foreign or pointer type) **not** wrapping
  ErrCorrupt. That is v1's documented contract, and the amendment changes it only for depth.
- §5.1 asserts `errors.Is(err, ErrCorrupt)` for a 9-deep Encode. A new test, §5.2, pins the
  other side: the remaining Encode errors do not wrap ErrCorrupt. Both halves of the contract are
  now tested.

**F2 (minor): the error-layering contract was inconsistent.** Accepted, using the first option the
finding offered.
- Every internal decode function (`readFrame`, `decodeFrame`, `decodeStream`, `decodeCounted`,
  `decodePayload`) returns plain detail errors at every depth.
- Loops add context with `%w`, giving messages like "frame i at offset o: …".
- Only `Decode` calls `corrupt()`, exactly once, so the prefix appears once.
- This rule is written in §2.3 and will be a code comment. §2.3 also gives a sample message.

**F3 (minor): the sibling test might prove nothing.** Accepted. §5.6(b) now gives the exact byte
construction.
- The over-long inner frame has the correct `len`, a parseable Put payload, and a correct inner
  CRC when it is read *through* the Batch boundary. The inner CRC is computed over bytes that
  include the outer CRC and the trailing filler.
- A decoder that reads past the payload would therefore succeed on that frame.
- The test also asserts that the detail contains "truncated". That pins down that the sub-slice
  bound is what rejects the stream, so the result does not depend on how a buggy decoder fails
  later. I checked the arithmetic: Bbody is 18 bytes, the inner len is 18, the bounded region is
  13 bytes, and 18+8 > 13.

**F4 (minor): AllocsPerRun counts allocations, not bytes.** Accepted.
- §5.9 now uses `testing.Benchmark(...).AllocedBytesPerOp()` with a 4 KiB threshold.
- I added a second case, `n = 1<<24` with one real inner frame. It catches a moderate `make(n)`
  regression that would not OOM but would allocate hundreds of MiB.

**F5 (minor): there was no fuzz target.** Accepted. §5.15 adds `FuzzDecode`, seeded with the
public fixtures and the §5 corpora. It checks these properties:
- No panic.
- An error always wraps ErrCorrupt and comes with nil records.
- On success, Encode succeeds, the records round-trip, and the bytes are canonical:
  `Encode(Decode(x)) == x` for a version-2 `x`.
- I extended the canonical property to version-1 input: the output is `x` with the first byte
  changed to 2. This also exercises the v1 regression rule.
- The seeds run under plain `go test`. A 30-second fuzz run is added to the done criteria.

**F6 (minor): the dotted error path was built eagerly.** Accepted.
- `frameSize` no longer takes a path string. It returns a `*encodeError`, and each level appends
  its own index to `e.path` only while unwinding on failure.
- `Error()` reverses the path and formats it once.
- The success path does no string building, strconv calls, or allocation for paths.
- The concrete pointer return type avoids the typed-nil-in-interface pitfall.

**F7 (nit): the `count < 0` sentinel mode.** Accepted.
- There are now two entry points: `decodeStream(b, ver)` and
  `decodeCounted(b, ver, depth, n uint32)`.
- They share the per-frame step `decodeFrame`, so the frame logic still exists in only one place.

**F8 (nit): the sketch had type mismatches.** Accepted.
- `decodeCounted` keeps `n` as a `uint32` and loops with `i := uint32(0)`.
- Capacity is `int(min(uint64(n), uint64(len(b)/minFrameSize)))`.
- The full loop is written out in §2.3 with consistent types.

**F9 (nit): use a `kindAllowed(ver, kind)` helper.** Accepted.
- §2.1 defines it, and `decodeFrame` calls it right after `readFrame`.
- It is the only place the version-to-kinds mapping lives. It also covers unknown tags such as 99,
  which keeps v1 public test 3 failing the right way.

**F10 (nit): keep readFrame's v1 overflow-safe check verbatim.** Accepted. §2.3 now spells out
`uint64(n)+lenSize+crcSize > uint64(rem)` exactly, with a "verbatim v1 form" note.

**Verified items in the findings.** These are kept unchanged: the depth convention, the
bounded sub-slice, exact consumption, the running-sum cap, the tag-3 rejection in version 1, the
version check before any frame, an empty Batch decoding to non-nil `Records`, and the v1 fixture
analysis.

**Remaining disagreements:** none.
