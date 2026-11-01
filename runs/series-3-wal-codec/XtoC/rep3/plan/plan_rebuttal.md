# Response to plan findings

## Major findings

1. **v1 private test conflict with version 2 — addressed.** The old malformed-input case `{2}` is explicitly superseded because it is a valid version-2 version-only stream. Replace it with `{3}`; `{2}` must decode to a non-nil empty slice. Keep `{0}` as corrupt and audit remaining v1 private assertions for changed version behavior.

2. **Encode depth error must satisfy ErrCorrupt — addressed.** Over-depth Encode errors will satisfy `errors.Is(err, ErrCorrupt)` while retaining the internal depth sentinel if useful via multi-wrapping. This matches the amendment's shared deeper-than-eight rule for encoding and decoding.

## Minor findings

1. **Bare Decode error contract — addressed.** Every malformed Decode result at all depths returns exactly bare `ErrCorrupt` and nil records, with no partial results; this includes depth, count, boundary, CRC, and version/tag failures.

2. **Overflow-safe count bound and 386 gate — addressed.** Check `uint64(n) > uint64(len(payload)-4)/13` before allocation and never cast untrusted `n` to `int` for the bound. Restore `GOARCH=386 go test ./...` and `GOARCH=386 go vet ./...` in acceptance.

3. **Avoid copying oversize Encode input — addressed.** Retain v1's cheap pre-append leaf size check for Put/Delete. Stop Batch child appends immediately when the running body exceeds `maxBody`, with a final body-length check before writing the frame length.

4. **Identify source of v1 private tests — addressed.** Restore `walcodec_impl_test.go` and `walcodec_impl_fuzz_test.go` from the item_7 heredocs in `turns/c1_3_imp.json` and compare with `/tmp/wcp` when available to ensure they match converged c1_3. The plan does not treat an absent or partial substitute as the regression source.

5. **Keep private tests external to package — addressed.** Supplemental Batch tests use `package walcodec_test`, like the v1 private tests, so they do not collide with public-suite helper names.

6. **Batch fuzz seeds and reverse property — addressed.** Seed with an Encode of an 8-deep nest and a hand-built 9-deep stream; for every accepted input, assert `Encode(Decode(b))` succeeds.

7. **Self-referential Batch — addressed.** Document that cyclic Batch input is rejected by the depth cap and add a test that Encode returns the over-depth error without overflowing the stack.
