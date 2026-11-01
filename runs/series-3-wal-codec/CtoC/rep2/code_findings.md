# Code findings — walcodec v2, round 2 (adversarial re-review after code_rebuttal.md)

minor: The encode rules now live in two places. payloadLen (sizing pass) and appendPayload/appendFrame (write pass) each check the depth cap, the Put/Delete field caps and the 16 MiB frame cap. They already disagree on errors: the sizing pass reports an oversize field as "field exceeds 16777216-byte limit", while the write pass reports the same condition as errFrameTooLarge. The write-pass checks can never run with today's single-threaded input. The coverage profile shows codec.go:126, 132, 137, 144, 147 and 152 (appendPayload is 70% covered) and 104, 111 and 115 (appendFrame is 78.6%) never executed, so a future change to one copy of a rule could drift from the other without a test failing. Fix: keep one validator. Either drop the sizing pass and append into a growing buffer (back-patching already makes the lengths correct), or keep sizing as the only validator and remove the duplicate checks from the write pass. At the least, share one error value per rule.
nit: The appendFrame comment (codec.go:96-100) says input that changes between passes "(e.g. a *Batch mutated concurrently) yields an error, never a panic or an invalid frame". Concurrent mutation during Encode is a data race. The Go memory model gives no such guarantee (a torn slice header can cause an out-of-range panic), and a mutation that stays within the caps is silently encoded as a mix of old and new state. Change it to say that sequential drift between passes gives an error, and that Encode's input must not be mutated during the call.
nit: Decode allocation amplification for many tiny records. Reproduced: one 16 MiB Batch holding 1,290,554 empty Delete frames decodes successfully, with 105 MiB TotalAlloc (about 6.5x the input). The cause is boxing each record into a Record interface plus append doubling. The cost is linear and forced by the []Record API (v1's top-level stream behaves the same), so it is noted only.
nit: Every byte at depth d is CRC-checked d+1 times, because the outer frame CRC covers the inner frames. That is at most 9x CRC work. A 16 MiB stream nested 8 deep decodes in about 22 ms. This follows from the format, not the code, and is not worth changing.
nit: Test gaps (not defects; I probed each case and it behaves correctly). No test reaches the Put field-oversize branch in payloadLen (codec.go:41; Put{Key: 16 MiB+1} correctly gives "field exceeds"), the truncated Put/Delete field-header path in readField (codec.go:305), or the Put key-truncated path in decodePut (codec.go:315). All three give ErrCorrupt (payloads {0,0}, {0,0,0,5,'a'}, {0,0,0,0,0,0}). Add them to TestImplBatchCorruption / TestImplEncodeRejection so the uncovered lines are only the deliberately defensive ones.

Rebuttal evaluation (each claim re-checked against source and by running it):
- Item 1 (major, alloc before inner oversize-len check): VERIFIED FIXED. decodeBatch starts from `var recs []Record` and does not preallocate. TestImplAllocBounds has 1000-byte filler for n=1, 2 and 0xFFFFFFFF and requires 0 allocs, and it passes, including under -race and -count=3.
- Item 2 (minor, nested hostile-count amplification): VERIFIED FIXED. The 8-level nested n=0xFFFFFFFF case gives ErrCorrupt with 0 allocs.
- Item 3 (minor, O(depth×nodes) re-sizing): VERIFIED FIXED. appendFrame back-patches len, and the write pass never calls payloadLen. The leftover duplication is the minor finding above.
- Item 4 (nit, panics): VERIFIED FIXED. grep finds no panic call in the non-test source.
- Item 5 (nit, minFrameSize): VERIFIED removed.
- Item 6 (nit, allowBatch flag): VERIFIED. Version policy is centralized in kindAllowed(ver, kind).
- Item 7 (nit, Batch{} DeepEqual asymmetry): accepted as documented on the Batch type, and pinned by TestImplEmptyBatchNonNil.
- Item 8 (nit, no sentinel Encode errors): accepted. The frozen surface rules out new exports.
- Item 9 (nit, pointer records accepted): accepted per the plan (§2.3), and documented on Encode.

Verification performed: gofmt -l is clean. go vet passes (amd64 and GOARCH=386). go test -race -count=3 passes, and GOARCH=386 go test passes. Coverage is 90.7%. Both public test files are byte-identical to instrument/. go doc shows exactly the v1 surface plus Batch and its Kind method. A 15 s fuzz of FuzzImplDecode (5.8M execs) found nothing new. Re-derived from source:
- Depth convention: the top level is 0, decodeBatch(depth+1) rejects depth > 8, and the encoder rejects depth+1 > 8 before recursing. 8 nested levels are legal both ways; 9 are rejected both ways, including an empty Batch at the 9th level.
- Inner frames are read through a byteReader over the Batch payload sub-slice only, so they cannot read into a sibling or outer frame.
- Exact consumption: a loop of exactly n frames, then remaining()==0.
- Versions: 0, 3 and 255 are rejected before any frame, and a type-3 frame in a v1 stream is rejected.
- The v1 fixture decodes, and its crc flip and truncation are rejected.
- A Batch frame of exactly 16 MiB is accepted; one byte more is refused by Encode.
- Value and pointer cycles return an Encode error.
- Decode has no shared state, so concurrent calls are safe.

No blocker or major issue remains.
