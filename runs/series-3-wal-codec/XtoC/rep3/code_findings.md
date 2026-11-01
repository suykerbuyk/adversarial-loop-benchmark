# Code findings — walcodec v2 (XtoC/rep3, phase 2, round 3)

Rebuttal status: there is still no code_rebuttal.md. turns/c2_3_imp.json says the write was blocked ("writing outside of the project"), so there is no point-by-point response. Everything below was re-derived from worktree/walcodec.go and worktree/walcodec_impl_test.go.

Verified passing in worktree: go build, go vet, gofmt -l (clean), go test (85.9% coverage), go test -race, GOARCH=386 go vet, and GOARCH=386 go test. The implementor could not run the 386 checks, so I ran them here. Both public suites are byte-identical to instrument/.

I recreated the converged v1 private suite and v1 FuzzDecode from turns/c1_3_imp.json item_7. I applied that turn's own item_10 cap fix and swapped the superseded `{2}` corrupt case for `{3}`, as the plan requires. Run against the v2 code, it passes. Fuzzing found nothing: FuzzDecodeV1 ran 15s (6.4M execs), FuzzBatchDecode 20s, and FuzzDecode 10s.

Behavior probes on the real code all came out correct:
- Encode size cap: Put at exactly 16 MiB is accepted and 16 MiB+1 is rejected (errFrameTooLarge, nil bytes). Delete behaves the same. A Batch body at exactly 16 MiB is accepted and 16 MiB+1 is rejected.
- Decode size cap: a frame with len = 16 MiB+1 is rejected even when all its bytes and a valid CRC are present.
- Error paths: typed-nil `*Put`, and `&Delete{}` inside a Batch, return "unsupported record: %T" with nil bytes.
- Allocation guard: a 0xffffffff Batch count is rejected with about 64 B allocated.
- Malformed input: trailing bytes after a Put value and an inner len that overruns into a sibling frame are both ErrCorrupt.
- Depth: depths 0–8 are accepted and depth 9 is rejected on both Encode and Decode, including an empty 9th-level Batch. Cyclic Batches are rejected. Batch inside a v1 stream is rejected, as are versions 0 and 3.

## Round-2 items resolved (verified)
- Swapped Encode error labels: fixed. `default:` returns `errUnsupportedRecord` with %T, and the test covers `&Put{}`, `&Delete{}`, `&Batch{}` and a foreign type (mutation M6 is killed).
- Empty Put value decoding to nil: fixed (`make` + `copy`). The v1 private TestKindsAndDecodeContracts passes, and the Decode doc again states the empty-value and order/duplicate behavior.
- Test case 0 now really is "payload shorter than count" (`{0,0}`). The `len(body) < 5` guard is covered (mutation M1 is killed).
- The fuzz oracle now checks `reflect.DeepEqual` on the round trip.
- Tag literals were replaced by putTag/deleteTag/batchTag.
- The error vars are grouped, the v1 message "frame exceeds 16 MiB limit" is restored, `Kind() uint8` is used consistently, and the wire contract (len excludes CRC, payload grammar) is documented.

## Findings
[major] worktree/walcodec_impl_test.go — No test covers the 16 MiB frame cap, in either direction, and the plan's acceptance items for it ("exact/beyond frame-size boundaries", restore the v1 private suite) are still unmet. Mutation testing against the delivered suite (public + impl tests):
- Removing the Put cap and the final post-switch cap together (walcodec.go:100 + :141) SURVIVES, so Encode would emit Put frames larger than 16 MiB.
- Removing the Delete cap (:110 + :141) SURVIVES.
- Removing the Batch running cap (:132 + :141) SURVIVES.
- Removing the decode `n > maxBody` check (:244) SURVIVES, because the public TestOversizeLen frame is rejected for truncation anyway.
The 16 MiB rule is a pinned spec rule ("rejected before any allocation"), and right now it is enforced only by code no test exercises. The converged v1 private TestEncodeErrorsAndFrameLimit killed the Put mutant, but it was not carried forward. Fix: restore the v1 private suite (with the `{2}`→`{3}` and item_10 adjustments), and add exact/+1 encode tests for Put, Delete and Batch. Also add a decode test where a len = maxBody+1 frame is fully present with a valid CRC.
[minor] worktree/walcodec_impl_test.go:162 — The anti-aliasing check in TestDecodeContractsAndCopy flips the wrong byte, so it can never fail. For `Put{"k","v"}` the stream is 20 bytes: version(0), len(1-4), tag(5), klen(6-9), 'k'(10), vlen(11-14), 'v'(15), crc(16-19). `stream[len-8]` is index 12, a vlen byte, not the value byte. Verified: changing the decoder to `value := rest[4:]` (aliasing) survives the whole suite (M5). Fix: flip `stream[len(stream)-5]`, or locate the byte with `bytes.LastIndexByte`.
[minor] worktree/walcodec_impl_test.go:86 (case 2) — The 0xffffffff-count case cannot detect losing the allocation bound at walcodec.go:218. With the guard removed, `make([]Record, 0, 4294967295)` asks for a 64 GiB block. Linux overcommit maps it lazily, so the test still passes (M2 survives). Under `ulimit -v 8000000` the same mutant crashes with "fatal error: out of memory". Fix: assert a small allocation, e.g. `testing.AllocsPerRun` together with a `runtime.MemStats` TotalAlloc delta under 1 KiB.
[minor] worktree/walcodec_impl_test.go — Some v1 strictness and contract checks are covered only by the v1 private suite, which was not restored. These mutants survive the delivered suite:
- Accepting trailing bytes after a Put value (`!=`→`>` at walcodec.go:200, M15). This one also survives the v1 suite.
- Accepting trailing bytes after a Delete key (:208, M12b).
- Encode returning a partial buffer instead of nil bytes on error (:81, M20). The Encode doc promises "Any error returns nil bytes".
Add a Put-trailing-byte malformed case, a Delete-trailing case, and `b != nil` assertions on the Encode error paths.
[nit] walcodec.go:88-90, :123-125, :140-143, :171-173 — Defensive checks that no input can reach:
- appendFrame's top depth check: the Batch case already stops at depth ≥ 8 (M18 survives).
- The uint32 count check: it would need over 4G records, far beyond the 16 MiB cap (M24 survives).
- The post-switch `bodyLen == 0 || > maxBody` check: every case already enforces the cap, and no body is empty (M17 survives).
- parseSequence's depth check and `depth` parameter: it is only ever called with depth 0 (M23 survives).
Remove them, or label them as defensive. The duplicated cap checks are also why the major above needs combined mutants to show the gap.
[nit] walcodec.go:196-204, :213-217, :262-269 — Width literals remain next to named constants: `len(rest) < 4`, `rest[:4]`, `len(rest)-4`, `rest[4:]`, `body[1:5]`, `body[5:]`, and the `4`s in takeString. uint32Len, plus a batchCountEnd = 1+uint32Len, would make the layout self-describing.
[nit] worktree/walcodec_impl_test.go:168-219 — FuzzDecode and FuzzBatchDecode are near-duplicates with the same oracle. Their seed corpora are thin: there is no seed for a v1 Put/Delete stream, a mixed Batch, an empty Batch, or the v1 fixture. The plan called for a separate walcodec_impl_fuzz_test.go in package walcodec_test with the v1 seeds; the tests are in-package instead.

## Concurrency and resources
No package-level mutable state; -race is clean. Decode allocation is bounded by the input:
- A Batch count is limited to len(payload)/13 before `make`.
- Every frame's len is capped at 16 MiB and checked against the enclosing slice before it is sliced, so an inner frame cannot read into a sibling or outer frame.
- Recursion goes at most 9 levels deep.
Encode buffers at most about 32 MiB per oversize Batch before rejecting it, and cycles stop at depth 8. CRC work grows at most 9× from re-hashing nested bodies. On 386, every `int(n)` conversion is bounded first. No issues found.

## Summary
The production code (walcodec.go) has no correctness defect that I could find. It meets the amendment and v1 behavior, including the restored v1 private suite. No blocker remains. One major remains: there are no tests for the 16 MiB frame cap at encode or decode, which leaves a planned acceptance criterion unmet; the cap could be removed without any test failing. Fixing that, plus the broken aliasing check (minor), is mostly test work. This is NOT ready to converge until the major is fixed.
