# walcodec v2 amendment implementation plan

## Baseline and scope

The v1 implementation is [snapshot-v1/walcodec.go](../snapshot-v1/walcodec.go); `worktree` currently contains only the module and the two supplied public suites, so the source will first be carried into `worktree`. Preserve the names and signatures of `Put`, `Delete`, `Record`, `Encode`, `Decode`, and `ErrCorrupt`. Add only exported `Batch{Records []Record}` with value-receiver `Kind() == 3`.

Encoding emits version 2 for all records, including empty and Put/Delete-only streams. Decoding accepts versions 1 and 2, selecting allowed tags from the version before parsing any frame. Version 1 accepts only Put/Delete; version 2 accepts Put/Delete/Batch. Every other version is rejected with bare `ErrCorrupt`.

## Wire format, depth, and API behavior

Keep v1 frame and Put/Delete layouts: big-endian uint32 body length, type plus payload, CRC32-IEEE over type plus payload; body length excludes CRC and is capped at 16 MiB at every level. A Batch payload is a uint32 count followed by exactly that many complete frames. Its inner frames are parsed only within the Batch payload slice. The payload must be consumed exactly; count mismatch, trailing bytes, truncated inner frame, bad CRC, unknown tag, and Batch in a v1 stream are corrupt.

Top-level sequence depth is 0. Entering a Batch payload increments depth; depth 8 is permitted and depth 9 is rejected, even for an empty Batch. Encode applies the same convention and returns an error satisfying `errors.Is(err, ErrCorrupt)` for an over-deep input (the internal `errBatchTooDeep` may remain distinguishable through multi-wrapping). A Batch comment will document this limit and that cyclic/self-referential Batch input is rejected by the depth guard rather than traversed indefinitely; test a self-referential value and ensure Encode returns the depth error without stack overflow.

Keep v1 Encode validation for nil, pointer, and unsupported Record implementations, extended recursively to Batch children. On any Encode error return nil bytes. Preserve the v1 Put/Delete payload grammar, order and duplicate behavior, nil/empty value equivalence, and 16 MiB per-frame rule. For Put/Delete, compute the leaf body size before appending/copying payload bytes, as v1 does. For Batch, append complete child frames recursively and stop the loop as soon as the running Batch body exceeds `maxBody`; retain a final body-size check as the authoritative cap check before converting to uint32 or writing the frame length.

## Encoding changes

Use one recursive frame append path to keep frame layout consistent. Reserve four bytes for frame length, append type/payload, compute size using `uint64`, enforce the 16 MiB cap before uint32 conversion, write length, then append CRC. Batch payload starts with a count placeholder; after all children encode, validate count fits uint32 and store it. The recursive path validates all nested record forms and checks entry depth before traversing a Batch, including empty Batches. Empty top-level input remains a one-byte version-2 stream; an empty Batch has a four-byte zero count payload.

## Decoding changes

Validate that a version byte exists and is 1 or 2 before parsing frames. A bounded sequence parser takes a slice, version policy, and depth; for each frame it validates length field availability, nonzero length, 16 MiB cap, body-plus-CRC availability, then CRC before decoding the body. Decode Put/Delete payloads with exact field consumption. Dispatch Batch only under version 2.

For Batch, require four count bytes, enter/check the child depth, then bound the count before allocation using overflow-safe arithmetic: compare `uint64(n)` against `uint64(len(payload)-4)/13` (13 bytes is the smallest complete frame), never convert the untrusted count to `int` for this bound. Allocate only after that check, parse exactly `n` frames from the bounded payload remainder, and require no leftover bytes. This prevents count-driven allocation and prevents an inner length from consuming sibling or outer bytes. Return non-nil empty Records for a zero-count Batch, consistent with v1's version-only decoded slice.

Every Decode failure at every nesting level returns the bare `ErrCorrupt` sentinel and nil records, including depth errors, count mismatches, inner overruns, and v1 tag 3. Do not return partial records or wrapped decode errors. Preserve this exact identity contract from v1 private tests and fuzzing.

## Documentation

Update package, Record, Encode, Decode, and Batch comments for v2 framing, both accepted versions, the allowed tags per version, Batch exact-consumption rule, per-frame cap, depth convention, nil/pointer/foreign-record rejection, Encode error behavior, and bare Decode error behavior. State that version 1 remains readable from fixed v1 bytes and rejects Batch.

## What stays

The v1 exported API signatures and Put/Delete wire encoding remain unchanged. CRC32-IEEE, big-endian integers, 16 MiB cap, ordering, duplicates, nil/empty Put value equivalence, Decode's bare `ErrCorrupt` with nil records on malformed input, and non-nil empty output for valid version-only streams remain. The v1 public suite remains unchanged; the v2 suite uses a fixed literal version-1 fixture rather than assuming Encode still writes 1.

## Verification and acceptance

Keep both supplied public suites unchanged. Restore the implementor-owned `walcodec_impl_test.go` and `walcodec_impl_fuzz_test.go` from the converged v1 implementation in `turns/c1_3_imp.json` (the `item_7` file-write heredocs; cross-check against `/tmp/wcp` when available), and verify the restored files match the converged c1_3 versions. Keep supplemental tests in `package walcodec_test`, matching the v1 private tests and avoiding helper collisions with the in-package public suites.

The old v1 malformed-stream table's `{2}` case is superseded: version-only `{2}` must decode successfully to a non-nil empty slice in v2. Replace that corrupt-version case with `{3}` (and test `{0}` remains corrupt). Audit all other v1 private assertions for the new version semantics; retain exact `err == ErrCorrupt` checks for malformed Decode results and Encode errors being distinct unless the amendment now requires `errors.Is(err, ErrCorrupt)` for excessive Encode depth.

Add private Batch coverage for: max legal depth 8 and rejected depth 9 on both Encode and Decode (including empty ninth-level Batch); Encode→Decode agreement for all accepted depths; self-referential Batch Encode rejection; Batch Put/Delete and nested round trips; version-1 Batch-tag rejection; truncated Batch; count short/long, trailing bytes, payload shorter than count field, inner CRC error, and inner length crossing its enclosing payload while still fitting the outer frame; huge count rejection without count-sized allocation; and exact/beyond frame-size boundaries. Extend Batch fuzzing with an encoded 8-deep seed and hand-built 9-deep seed, and for every accepted fuzz input assert `Encode(Decode(b))` succeeds. Preserve the v1 FuzzDecode properties: no panic, every malformed input returns bare ErrCorrupt and nil records, and accepted output can be re-encoded and decoded.

Acceptance commands in `worktree`: `go test ./...`, `go build ./...`, `go vet ./...`, `GOARCH=386 go test ./...`, and `GOARCH=386 go vet ./...`, alongside both supplied suites and the restored/extended private suites.
