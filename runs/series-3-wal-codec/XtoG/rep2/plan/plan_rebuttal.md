# Response to plan findings

The revised plan addresses each finding in `plan_findings.md`:

1. **Missing implementation baseline (major):** It explicitly says `worktree/walcodec.go` is absent, identifies `snapshot-v1/walcodec.go` as the baseline, and directs carrying that implementation into the target package before extension.
2. **Depth off-by-one (major):** It pins the rule to child stream entry, `if d+1 > 8`, states eight nested Batches are accepted and nine rejected, and includes both boundaries in validation.
3. **Version-1 Kind 3 (major):** It states v1 permits only Kinds 1 and 2, with a well-formed Kind 3 rejected, and calls for a direct validation case.
4. **Exact consumption (major):** It specifies Put/Delete field bounds and no trailing payload bytes, plus exactly `count` Batch frames and no residual bytes; malformed exactness cases are included at top level and inside Batch.
5. **Batch frame cap (major):** It applies the 16 MiB cap to each frame's complete `type+payload` length, explicitly including the enclosing Batch frame, with encoder and decoder checks and an over-cap Batch validation case.
6. **Inner length crossing Batch boundary (major):** It requires passing only the Batch payload slice to recursion and adds a valid-CRC outer Batch fixture where an inner frame could reach later outer bytes if the parser escaped its slice.
7. **Encode over-depth error (minor):** It specifies `errEncode`, nil output, and that it does not match `ErrCorrupt`, with a nine-level Encode case.
8. **Empty Batch (minor):** It defines count zero as valid, returns an empty records slice, and clarifies that an empty Batch at child depth 8 is allowed while opening depth 9 is rejected.
9. **Pointer dispatch (minor):** It freezes the accepted Encode forms to values and non-nil pointers of Put, Delete, and Batch; nil pointers and other implementations return `errEncode` and nil bytes.
10. **Hostile count allocation (minor):** It requires wide-integer comparison against `(payloadLen-4)/9` before any count-dependent allocation and explicitly covers `0xffffffff` with no count-sized allocation.
11. **Inner CRC/tag/length invariants (minor):** It mandates the same frame reader and checks at every nesting depth and lists isolated inner CRC, unknown tag, zero length, and oversize length cases.
12. **Uncovered v1 invariants (minor):** It enumerates short headers, missing version, Put/Delete truncation and trailing bytes, zero/oversize frame lengths, CRC and unknown-tag rejection, and empty/nil input; it requires several payload defects to be tested inside Batch too.
13. **Unknown tag under v2 (nit):** It explicitly requires Kind 99 rejection under both version 1 and version 2.
14. **Source location and commands (nit):** It identifies `worktree/walcodec.go` in package `walcodec` and specifies `go test`, `go vet`, and `go build` from `worktree`.
15. **Version byte for Encode (nit):** It requires version 2 for empty and Put/Delete-only encodes and directly checks both first bytes.
16. **Exact cap boundary (nit):** It defines the uint64 comparison, rejects zero and values greater than `uint64(16<<20)`, accepts exactly `16<<20`, and requires boundary validation on both Encode and Decode.
