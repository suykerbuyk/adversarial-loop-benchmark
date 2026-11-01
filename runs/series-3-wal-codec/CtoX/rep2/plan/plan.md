# Plan: `walcodec` v2 — nested `Batch` records

Status: phase-2 plan, round 3. The latest review (`plan_findings.md`) again reports no blocker or
major issue and lists no minor or nit findings, so the design is unchanged. Round 2 made three small
tightenings, marked **(r2)**: an early exit in encoder sizing, a redundant check removed from
`decodeBatch`, and a clearer statement of the version-1 + type-3 rule. A round-3 self-check against
`snapshot-v1/*.go`, `spec-v1.md`, the amendment, and both public test files found and fixed three
internal inconsistencies, marked **(r3)**:
a stale reference to the removed empty-`rest` check, an unreachable `math.MaxUint32` count check
(which needed an undeclared `math` import), and an unstated edge case (an empty Batch at the depth
limit). `plan_rebuttal.md` answers the finding point by point.

## 0. Baseline and a gap to fix first

- The converged v1 sources (`walcodec.go`, `encode.go`, `decode.go`) are in `snapshot-v1/`. The
  worktree currently holds only `go.mod`, `walcodec_public_test.go`, and `walcodec_v2_public_test.go`.
  **Step 1 of implementation:** copy the three v1 `.go` files from `snapshot-v1/` into `worktree/`
  unchanged. Then run `go test -run 'TestRoundTrip|TestCRC|TestUnknown|TestTruncatedFrame|TestOversize|TestEmpty|TestDecodeNil|TestDuplicate'`
  to confirm the v1 baseline before editing. Until `Batch` exists the v2 test file won't compile, so
  this check is done with the v2 test file temporarily moved aside, then put back.
- Both public test files in the worktree match `instrument/` byte for byte. They are **not modified**.
- `go.mod` (`module walcodec`, `go 1.25`) is unchanged. There are no new dependencies (stdlib only:
  `encoding/binary`, `hash/crc32`, `errors`, `fmt`).

## 1. What stays the same (v1 rules that still hold)

| v1 rule | How it is preserved |
|---|---|
| Exported surface `Record`, `Put`, `Delete`, `Encode`, `Decode`, `ErrCorrupt` frozen | No name or signature changes. The only new export is `type Batch` and its `Kind` method. No new exported errors, constants, or functions. |
| Frame = `len(4) type(1) payload(len-1) crc(4)`, BE, `len` counts type+payload, crc outside `len` | `decodeFrame` and `appendFrame` keep this layout. The same code handles frames at every depth. |
| CRC32-IEEE over type+payload | Unchanged; checked for every frame at every depth. |
| 16 MiB `len` cap, rejected before allocation | `decodeFrame` keeps the order: read `len`, check against `maxFrameLen`, check `len == 0`, then check truncation, before any slicing, CRC, or allocation. This applies to every frame at every depth. |
| Put/Delete payloads; nil/empty Value both `vlen=0`, decode to non-nil length-0 slice that doesn't alias input | `decodePayload` Put/Delete branches unchanged (`append([]byte{}, v...)`). |
| Unknown type tag, crc mismatch, truncation, oversize, len=0, trailing payload bytes → `ErrCorrupt` | Unchanged and applied at every depth. |
| Version byte validated before any frame; empty/nil input → `ErrCorrupt` | Unchanged order. Only the set of accepted versions grows (§3). |
| Empty `recs` → one-byte stream → decodes to empty non-nil slice | Unchanged; the one byte is now `2`. |
| Records in stream order; duplicates kept | Top-level and inner sequences are appended in wire order. |
| Errors wrap `ErrCorrupt` via `corrupt(...)`; on error `Decode` returns `nil` records | Unchanged. Inner errors propagate as-is (already wrapped), so `errors.Is(err, ErrCorrupt)` holds. |
| Encode accepts `Put`, `*Put`, `Delete`, `*Delete`; returns a plain (non-`ErrCorrupt`) error for nil element / nil pointer / unsupported type / over-cap frame | Kept, and extended to `Batch` / `*Batch` and to elements at any depth. |

## 2. What changes, file by file

### `walcodec.go`
- Add:
  ```go
  // Batch groups records into one frame whose payload is a nested framed
  // sub-stream: [n uint32] followed by n inner frames. Batches may nest up
  // to 8 levels deep.
  type Batch struct{ Records []Record }
  // Kind returns the Batch type tag, 3.
  func (Batch) Kind() uint8 { return kindBatch }
  ```
- Constants: rename the role of `version1` and add:
  `version1 uint8 = 1` (still accepted on decode), `version2 uint8 = 2` (written by Encode, accepted on
  decode), `kindBatch uint8 = 3`, `maxDepth = 8`, `countSize = 4` (the Batch `n` prefix), and
  `minFrameSize = lenSize + 1 + crcSize` (9; a conservative lower bound on any frame's wire size, used
  only to bound preallocation and fast-fail impossible counts).
- Update the package doc comment: add the Batch payload grammar
  (`Batch = [n uint32] frame{n}`), the depth convention, and the version rule (writes 2, reads 1|2).

### `decode.go`
Thread two pieces of context through the recursive decoder: the stream **version** (to decide whether
type 3 is legal) and the **depth** of the sub-stream being parsed.

```go
func Decode(b []byte) ([]Record, error)
    // len(b)==0 → corrupt("missing version byte")
    // v := b[0]; v != version1 && v != version2 → corrupt("unknown version %d")   (before any frame)
    // recs := []Record{}; loop decodeFrame(rest, v, 0) until rest is empty (top level is depth 0,
    //   ends at end of input, as in v1)

func decodeFrame(p []byte, ver uint8, depth int) (Record, int, error)
    // identical to v1 (cap → zero → truncation → crc), then decodePayload(typ, payload, ver, depth)

func decodePayload(typ uint8, payload []byte, ver uint8, depth int) (Record, error)
    // kindPut / kindDelete: unchanged
    // kindBatch: if ver == version1 → corrupt("batch record in version-1 stream")
    //            else decodeBatch(payload, ver, depth+1)
    // default: corrupt("unknown record type %d")

func decodeBatch(payload []byte, ver uint8, depth int) (Record, error)   // depth = sub-stream depth
    // 1. if depth > maxDepth → corrupt("batch nesting exceeds depth %d")       (before reading n)
    // 2. len(payload) < countSize → corrupt("truncated batch count")
    // 3. n := BE uint32; rest := payload[countSize:]
    // 4. if uint64(n)*minFrameSize > uint64(len(rest)) → corrupt("batch count %d exceeds payload")
    //    (fast fail; this also makes 5 safe)
    // 5. recs := make([]Record, 0, n)     // n is now ≤ len(rest)/9, so allocation is bounded by input size
    // 6. for i := uint32(0); i < n; i++ {
    //        rec, k, err := decodeFrame(rest, ver, depth); if err → return err
    //        (r2: no separate empty-`rest` check. decodeFrame already rejects len(rest) < 4 as
    //         "truncated frame header", which covers the fewer-than-n case.)
    //        recs = append(recs, rec); rest = rest[k:]
    //    }
    // 7. if len(rest) != 0 → corrupt("trailing bytes after %d batch records")
    // 8. return Batch{Records: recs}, nil   // non-nil (possibly empty) slice, value type
```

Why it's correct:
- **No read past the enclosing payload.** `decodeFrame` for an inner frame gets `rest`, a sub-slice
  of the enclosing Batch `payload`, which is itself `body[1:]` of the outer frame, already cut to the
  outer `len`. An inner `len` that claims more bytes than remain fails v1's existing
  `uint64(len(p)) < lenSize+n+crcSize` check → `ErrCorrupt` ("truncated frame"). Bytes of a sibling
  or outer frame (including the outer CRC) are never inside `p`, so they can't be read.
- **Exact consumption.** (r3: corrected to match the r2 step 6.) Fewer than `n` frames means
  `decodeFrame` is called on a `rest` that is empty or too short. It rejects that as "truncated frame
  header" (fewer than 4 bytes) or "truncated frame" (`len` overruns). In practice step 4 usually
  catches a too-large `n` first. More bytes than `n` frames, including garbage or extra well-formed
  frames, → step 7.
- **Depth.** The top level is parsed at depth 0. A Batch frame found in a stream at depth `d` has its
  sub-stream parsed at `d+1`, and `d+1 > 8` is rejected before its count is read. So 8 nested Batches
  (innermost sub-stream at depth 8) are accepted, and 9 nested Batches (sub-stream at depth 9) are
  rejected. Recursion is bounded at 9 stack frames regardless of input.
  (r3) The check depends on the sub-stream's depth, not on whether the sub-stream has any frames. So
  a 9th-level **empty** Batch (`n = 0`) is still `ErrCorrupt`, as the amendment says ("a sub-stream at
  depth greater than 8 is ErrCorrupt"). The encoder uses the same predicate, so it also refuses an
  empty `Batch{}` at depth 8. Encoder and decoder agree on every input.
- **Version gating.** A version-1 stream can never yield a Batch, at any depth (Batches can only be
  reached through a Batch, so checking at the type switch covers every level). This matches the
  amendment's "version = 1 (Put/Delete only)" and v1's own behavior, where tag 3 was an unknown type.
  A version-2 stream with only Put/Delete is fine. (r2) This is also exactly what the regression rule
  requires: v2 must "return the same records a v1 decoder would", and a v1 decoder rejects tag 3. So
  for every version-1 input, accepted or rejected, the v2 decoder behaves like v1.
- **Cost.** Each frame's CRC is checked before its payload is parsed, so an outer CRC covers its inner
  bytes once, and each inner frame is checked again. Each byte is hashed at most `maxDepth+1` times,
  which is linear in input size.
- **Allocation.** Top level: as in v1 (append-grown, ≤ one record per 9 input bytes). Batch: capacity
  ≤ `len(rest)/9` because of step 4, so a forged `n = 0xFFFFFFFF` in a tiny payload is rejected
  without allocating. Inner `len` uses the same 16 MiB cap before anything is sliced.

### `encode.go`
Replace the flat `frameFields` normalisation with a recursive two-pass design. Pass 1 validates and
sizes; pass 2 writes and can't fail.

```go
func Encode(recs []Record) ([]byte, error)
    // total := 1
    // for i, r := range recs { n, err := frameBodyLen(r, 0); err → fmt.Errorf("walcodec: record %d: %w", i, err)
    //                          total += lenSize + n + crcSize }
    // out := make([]byte, 0, total); out = append(out, version2)
    // for _, r := range recs { out = appendRecord(out, r) }
    // return out, nil

// frameBodyLen validates r, found in a stream at depth `depth`, and returns its len field
// (type+payload). It enforces the 16 MiB cap on THIS frame and, recursively, on every inner frame.
func frameBodyLen(r Record, depth int) (uint64, error)
    // switch on normalised r (deref *Put/*Delete/*Batch; nil pointer → error; nil/unsupported → error):
    //   Put:    1 + 4 + len(key) + 4 + len(value)
    //   Delete: 1 + 4 + len(key)
    //   Batch:  if depth+1 > maxDepth → fmt.Errorf("batch nesting exceeds depth %d", maxDepth)
    //           sum := 1 + countSize
    //           (r3: no separate `len(Records) > math.MaxUint32` check. The early exit below fails
    //           once sum > 16 MiB, and each child adds at least 9 bytes, so it trips after at most
    //           ~1.86M children. Any Batch that passes therefore has a count that fits in uint32,
    //           and pass 2's uint32(len(Records)) is lossless. No `math` import is needed.)
    //           for j, c := range Records { cn, err := frameBodyLen(c, depth+1)
    //                                       err → fmt.Errorf("batch record %d: %w", j, err)
    //                                       sum += lenSize + cn + crcSize
    //                                       (r2) if sum > maxFrameLen → cap error now (early exit:
    //                                       stops sizing a Batch that is already too big, and keeps
    //                                       sum far from uint64 overflow) }
    //   then: if n > maxFrameLen → fmt.Errorf("encoded frame length %d exceeds %d", ...)
    //
    // The cap check sits inside frameBodyLen, so an inner frame over the cap is reported at its own
    // index path, and an outer Batch whose combined inner frames push it over the cap is rejected too.
    // Each node is visited once, so the sizing pass is O(total records).

func appendRecord(out []byte, r Record) []byte
    // same back-patching pattern as v1 appendFrame: reserve 4-byte len, write type + payload,
    // patch len = len(body), append crc32(body).
    //   Put/Delete: payload as v1
    //   Batch: AppendUint32(len(Records)), then for each child: out = appendRecord(out, child)
    // Pointer forms are dereferenced (already proven non-nil in pass 1).
```

- A small shared helper, `normalize(r Record) (Record, error)`, turns `*Put`/`*Delete`/`*Batch` into
  value forms and rejects nil elements, nil pointers, and unsupported types. Both passes use it, so the
  switch logic isn't duplicated (pass 2 ignores the error because pass 1 already proved it nil).
- **Encoder enforces the decoder's limits.** Depth uses the same `depth+1 > maxDepth` predicate the
  decoder uses, and the cap uses the same `maxFrameLen`, so `Encode` never returns bytes `Decode` would
  reject. A `*Batch` that contains itself (a cycle) stops at the depth cap with an error instead of
  recursing forever.
- Encode errors stay plain errors, not `ErrCorrupt`, matching v1: they describe bad input, not a
  corrupt stream. The surface is frozen, so no new exported sentinel is added.
- `version2` is written even for Put/Delete-only input, which the amendment explicitly allows.

## 3. How each v2 rule is satisfied

| Amendment rule | Where / how |
|---|---|
| `Batch{Records []Record}`, `Batch{}.Kind() == 3` | `walcodec.go` type + `Kind` returning `kindBatch = 3`. |
| Batch payload = `n uint32` + `n` full inner frames | `appendRecord` Batch branch writes it; `decodeBatch` parses it with the same `decodeFrame`. |
| Recursion: Batch may contain Put/Delete/Batch | `decodeBatch` → `decodeFrame` → `decodePayload` → `decodeBatch`; same for the encoder. |
| 16 MiB `len` cap at every depth | `decodeFrame`'s cap check runs for inner frames too; the encoder checks every frame in `frameBodyLen`. |
| Inner `len` past the enclosing payload → `ErrCorrupt`, never reads the next outer frame | Inner parsing works on a sub-slice bounded by the outer frame's `len` (§2 decode, "no read past"). |
| Exactly `n` frames; fewer, more, or trailing bytes → `ErrCorrupt` | `decodeBatch` steps 4, 6, 7. |
| Depth: top level = 0, sub-stream at depth > 8 → `ErrCorrupt`; encoder uses the same cap | `decodeBatch` step 1; `frameBodyLen` Batch check, same predicate. |
| `Encode` writes version 2 | `out = append(out, version2)`. |
| `Decode` accepts 1 (Put/Delete only) and 2 (all types); any other version (0, 3, …) → `ErrCorrupt` before any frame | Version check at the top of `Decode`; Batch-in-v1 rejected in `decodePayload`. |
| v1 regression via fixed v1-bytes fixture | Decode paths for version 1 are unchanged except that the version gate accepts 1. The v1 suite and v2 cases 3 and 8 pass. |

## 4. Public test walkthrough (expected results)

v2 file:
1. `TestBatchRoundTrip`: one Batch[Put, Delete] → v2 bytes → equal records. ✔
2. `TestNestedBatchRoundTrip`: depth-2 nesting is well under the cap. ✔
3. `TestV1FixtureDecodesUnderV2`: version byte 1 accepted; frames are Put (len 0x11) and Delete
   (len 0x09) with valid CRCs. ✔
4. `TestMixedTopLevelRoundTrip`: includes `Put{Value: nil}`, which decodes to `[]byte{}`; the
   `string(...)` comparison treats them as equal. ✔
5. `TestTruncatedBatch`: cutting 2 bytes off the outer CRC fails the outer truncation check. ✔
6. `TestVersion3Rejected`: version gate. ✔
7. `TestBatchNestedPastDepth8`: 9 hand-built Batch wrappers, version 2, all CRCs valid. Sub-stream
   depths are 1..9; depth 9 > 8 → `ErrCorrupt`. ✔ (With 8 wrappers it would be accepted; see §5.)
8. `TestV1BehaviorsViaFixture`: the fixture decodes; flipping the last CRC byte gives a CRC mismatch;
   cutting 3 bytes gives truncation. ✔

v1 file (unchanged, run alongside): all 9 cases are version-agnostic or use hand-built version-1
streams (`TestUnknownType` with tag 99, `TestOversizeLen`), which still decode or reject as before.
`TestEmptyRecsVersionOnly` checks only the length (1), not the value. ✔

## 5. Verification plan (implementation phase)

- `gofmt -l .` is empty; `go build ./...`; `go vet ./...`; `go test ./...`; `go test -race -count=1 ./...`.
- Add a package-internal test file, `walcodec_v2_internal_test.go`. It keeps the public files untouched
  and uses its own helper names to avoid collisions with the `beU32`/`mkFrame`/`be2`/`mkFrame2`
  helpers. It covers edge cases the public suite doesn't pin:
  - exactly 8 nested Batches: `Encode` succeeds and `Decode` round-trips (the boundary is accepted);
  - (r3) 8 nested Batches whose innermost Batch contains an empty `Batch{}` (a 9th level): `Encode`
    errors, and the hand-built bytes (`n = 0` at sub-stream depth 9) → `ErrCorrupt`;
  - (r3) a Batch with a very large number of tiny children: `Encode` returns the cap error, with no
    panic and no truncated count;
  - 9 nested Batches: `Encode` returns a non-nil error and no bytes;
  - a self-referential `*Batch`: `Encode` returns an error and doesn't recurse forever;
  - Batch (type 3) inside a version-1 stream → `ErrCorrupt`;
  - version 0 and version 255 → `ErrCorrupt`;
  - hand-built Batch with valid outer CRC where `n` is greater than the frames present, `n` is less
    than the frames present, there are trailing junk bytes after `n` frames, or the payload is shorter
    than 4 bytes → `ErrCorrupt`;
  - an inner frame whose `len` overruns the Batch payload while the outer CRC is valid and a valid
    sibling frame follows the Batch at top level → `ErrCorrupt` (proves no read into the sibling);
  - inner `len` > 16 MiB → `ErrCorrupt`;
  - `n = 0xFFFFFFFF` with a tiny payload → `ErrCorrupt` with no large allocation (assert with
    `testing.AllocsPerRun` / a bound on bytes, or just that it returns quickly);
  - empty `Batch{}` and `Batch{Records: nil}` round-trip as a Batch with 0 records;
  - `*Batch` input encodes the same as `Batch`; a nil `*Batch` or a nil element inside a Batch is an
    Encode error, not a panic;
  - inner record over the cap, and an outer Batch whose children sum over the cap → Encode error;
  - a decoded inner Put `Value` doesn't alias the input buffer;
  - a fuzz target `FuzzDecode` (seeded with the fixture and some encoded Batches): `Decode` never
    panics; any error satisfies `errors.Is(err, ErrCorrupt)`; on success, `Encode(Decode(b))`
    re-decodes to equal records.

## 6. Out of scope / explicit non-changes
- No change to the public test files or `go.mod`.
- No new exported identifiers besides `Batch` (and its method).
- No option to write version-1 streams: the amendment says `Encode` writes 2.
- The decoder never returns `*Batch`; it always returns value records, consistent with v1 for
  Put/Delete.
