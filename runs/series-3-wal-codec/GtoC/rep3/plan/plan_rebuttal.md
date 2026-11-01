# Rebuttal to `plan/plan_findings.md`

Each finding is accepted. The replacement plan in `plan/plan.md` is what an implementor should follow.

## 1. major: the pinned table design does not compile

Accepted. `var encodeTable = codecTable{byKind: []*kindCodec{..., kindBatch: batchCodec}}` together with `batchCodec` referring to `decodeBatch` and `batchBodyLen`, which close over `encodeTable`, is a package-level initialization cycle. A Go build reports `initialization cycle for batchCodec`. The previous plan left the implementor to invent a fix on the first `go build`.

The plan now picks one architecture and does not leave the other open. The tables are declared as empty variables. `init` fills `putCodec`, `deleteCodec`, `batchCodec`, and the three tables. Nothing mutates them after `init`. `bodyLen` and `decode` do not gain a table parameter. `batchBodyLen` and `decodeBatch` read the table variables only at call time, after `init` has returned, so no initializer refers to a variable whose initializer refers back to it. A comment on the vars states why they are not composite literals. Implementation step 9 treats a clean `go build` as the check that the cycle stayed gone.

## 2. major: internal tests in package `walcodec` can collide with the grader

Accepted. The shipped v2 file says the public suites share package `walcodec` and suffixes its helpers so they do not collide. Hidden grader files are added to that same package. A top-level name such as `TestBatchDepth8RoundTrip`, `mkFrame3`, `be`, or `batchPL` in `package walcodec` would fail the package compile and every test, not one case. The file only needs the exported API, so it does not need to be an internal test package.

The plan now declares `worktree/walcodec_v2_internal_test.go` as `package walcodec_test`, imports `walcodec`, and prefixes every top-level identifier (`TestIntV2_...`, `intv2Frame`, `intv2BE`, `intv2BatchPL`). No unprefixed helper. A grader file in package `walcodec` cannot collide with it. `go test ./...` still runs the file.

## 3. minor: a measure/write mismatch would not panic

Accepted. The v1 `appendFrame` reslice `buf[start+4 : start+4+bodyN]` keeps the capacity of the whole stream buffer. `payload[4:4]` passed to child `appendFrame` calls would then have capacity past the `Batch` payload, into the outer CRC and the sibling frames. A mismatch would overwrite those bytes.

The plan now requires three-index slices. The body is `buf[start+headerSize : start+headerSize+int(bodyN) : start+headerSize+int(bodyN)]`, cap equal to length. The child region is `payload[4:4:len(payload)]`. After the children are written, `writeBatch` asserts `len(written) == len(payload)-4` and panics with `walcodec: measure/write length mismatch` if it fails. That panic is a `bodyLen` bug, not a user-input path.

## 4. minor: nested records could panic in `Kind`

Accepted. Indexing `encodeTable.byKind[rec.Kind()]` for a child would panic on a nil `Record` or a nil `*Put` inside a `Batch`. Encode must return an error and a nil buffer.

The plan now states that `measure` resolves every record at every depth through `codecOf` before any table index. `codecOf` type-switches `Put`, `Delete`, and `Batch` values and does not call `Kind` on the default arm. The internal tests add a nil `Record`, a `*Put`, and a foreign `Record` inside a `Batch`, and inside a `Batch` nested in a `Batch`.

## 5. minor: version-2 decode was coupled to the encoder table

Accepted. Passing `&encodeTable` into version-2 decode and into `decodeBatch` means a later kind added to the encoder (the plan's stated extension point) would be accepted inside version-2 streams and nested `Batch` payloads. That is the bug a separate `decodeV1Table` already prevents for version 1.

The plan now names `decodeV2Table`. `init` points it and `encodeTable` at the same `byKind` slice today. A later version re-points `encodeTable` only. Top-level version-2 decode and `decodeBatch` pass `&decodeV2Table`. They do not pass `&encodeTable`. Nested grammar is fixed by the version byte, the same way version 1 is fixed by `decodeV1Table`.

## 6. nit: the depth cap was a repeated literal

Accepted. Both checks now use `const maxDepth = 8`, with a comment that it is the deepest legal sub-stream and that the top-level stream is depth 0. The encode check and the decode check both read `depth+1 > maxDepth`. The doc-comment table and the "what not to do" list name the constant so a later edit does not put the literal back in one path only.

## 7. nit: the hostile-`n` divisor was today's minimum, not a format bound

Accepted. Dividing by 13 rejects a future kind whose payload is smaller than an empty `Delete` or an `n`-only `Batch`. Append-only decoding already bounds memory, so the pre-check only needs a safe lower bound.

The plan now uses `const minFrameWire = headerSize + 1 + crcSize` (9), with a comment that this is the frame shape's minimum, that it stays valid when a kind is added, and that it is not the 13-byte minimum of today's payloads. The compare stays `uint64`, so `n == 0xFFFFFFFF` cannot wrap. A ~20-byte `Batch` with that `n` still returns `ErrCorrupt` with no allocation.

## 8. nit: the `Records` copy protected nothing, and the cap ran too late

Accepted. Each child `frameSize` already holds the interface value `measure` captured. Copying `Records` and storing `Batch{Records: copied}` allocated at every level and was not what the write pass read. Checking the 16 MiB cap only after every child was measured also built the whole tree of an oversize `Batch` before rejecting it.

The plan now drops the copy. The write pass reads `children` and each child's `rec`. The running sum is checked inside the child loop, against `maxFrameLen` and `maxInt`, and the walk returns as soon as the sum would exceed either. The `uint64(len(Records)) > math.MaxUint32` check stays, because the write of `n` relies on it.

## 9. nit: earlier findings stay resolved

Accepted as confirmation. The plan keeps those resolutions and does not reopen them. Hostile `n` is append-only plus a `uint64` pre-check (now against `minFrameWire`). Depth is an argument (now compared with `maxDepth`). Encode measures once into a tree and write has no error return. An empty `Batch` decodes as a non-nil empty slice, with the doc comment. The stale doc comments stay in the touch list. `encodeTable` stays the encoder's table. The depth convention is unchanged: top-level is 0, eight wraps are legal, nine are rejected, and an empty `Batch` at the 9th level is rejected. `Encode` writes 2. `Decode` accepts 1 and 2, rejects 0 and 3 before any frame, and the version-1 table has no kind-3 slot. Inner `len` is bounded by the enclosing sub-slice.
