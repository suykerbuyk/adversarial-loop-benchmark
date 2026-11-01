# Plan — `walcodec` v2 (nested `Batch` records)

Round 1. There is no `plan/plan_findings.md`, so there is no rebuttal. This plan also picks up the
open minor findings from the v1 review (`turns/p1_2_adv.json`, `turns/c1_1_adv.json`) where they
touch code that v2 changes anyway. See §7.

## 0. Starting state and baseline

- `worktree/` holds only `go.mod`, `walcodec_public_test.go` and `walcodec_v2_public_test.go`.
  Both test files are byte-identical to the copies in `instrument/`. The converged v1 source is
  **not** in the worktree. It is in `snapshot-v1/` (`walcodec.go`, `codec.go`; its `go.mod` is
  identical). The v1 impl test file was not snapshotted.
- **Step 0:** copy `snapshot-v1/walcodec.go` and `snapshot-v1/codec.go` into `worktree/` unchanged.
  This is the baseline that the edits below modify. Then confirm that the v1 public suite passes on
  the baseline. The v2 suite will not compile yet, because `Batch` is undefined. So run with the v2
  file temporarily excluded, e.g. `go test -run . ./...` on a scratch copy. Never edit either
  public test file.
- `go.mod` (`module walcodec`, `go 1.25`) stays as it is.

## 1. What stays (frozen)

- Exported surface keeps exactly these names and signatures: `Record`, `Put`, `Delete`, `Encode`,
  `Decode`, `ErrCorrupt`. `Put{}.Kind()==1` and `Delete{}.Kind()==2`. **The only new exported
  identifier is `Batch`** and its `Kind` method. No new exported errors, constants or helpers;
  everything else stays unexported.
- The frame format is `len(4) type(1) payload(len-1) crc(4)`, big-endian. `len` covers type+payload,
  and `crc` is CRC32-IEEE over type+payload. The Put/Delete payload layouts are unchanged.
- The 16 MiB cap is inclusive (`len == 16<<20` is legal). It is checked before any allocation or
  read. It now applies to **every frame at every depth**, encode and decode.
- The v1 decode rules all still apply, now at every depth:
  - `len == 0` is ErrCorrupt.
  - crc mismatch, unknown type, truncation and oversize `len` are each ErrCorrupt.
  - Put/Delete payloads must be consumed exactly.
  - 1–3 stray bytes after the last top-level frame are ErrCorrupt.
  - Decoded keys and values are copied, never aliasing the input.
  - Duplicates are kept in order.
  - A nil Value and an empty Value both decode to a length-0 `[]byte`.
- Error contract:
  - Every `Decode` failure returns the bare `ErrCorrupt` (not wrapped) and a nil slice, with no
    partial result.
  - A successful decode returns a non-nil slice. A version-only stream gives `[]Record{}`.
  - `Encode` errors are plain `error`s, never `ErrCorrupt`, and come with a nil slice.
- `Encode` stays two-pass: a validate-and-size pass, then a single exact-capacity write. The
  `math.MaxInt` total-size guard on the top-level sum stays.
- `byteReader` (`take`/`u32`/`remaining`), `readField`, `decodePut`, `decodeDelete`, `appendBytes`
  and `appendU32` are unchanged.

## 2. What changes

### 2.1 `walcodec.go`

- Update the package doc comment:
  - The stream writes version 2 and accepts versions 1 and 2.
  - Add the Batch payload layout `n(4) frame_1..frame_n` (type 3).
  - Add the exact-consumption rule, the per-depth 16 MiB cap, and the depth convention (top-level
    = depth 0, max sub-stream depth 8).
- Constants:
  - Add `version2 uint8 = 2`, `kindBatch uint8 = 3`, `maxDepth = 8`.
  - Add `minFrameSize = lenSize + 1 + crcSize` (9): the smallest possible frame, used only to bound
    preallocation.
  - `version1` stays; its comment changes to "legacy, decode-only".
- New type, with a doc comment:
  ```go
  // Batch groups records; its payload is a nested framed sub-stream (type 3).
  type Batch struct{ Records []Record }
  func (Batch) Kind() uint8 { return kindBatch }
  ```
- `Encode`:
  - Writes `version2`.
  - The sizing loop calls `frameLen(r, 0)` (depth 0 = the top-level stream) and wraps errors with
    the index: `record %d: ...`.
  - The write loop calls `appendFrame(out, r, 0)`.
  - Doc comment: the stream is version 2; Batch may nest up to 8 levels; pointer handling (§2.3);
    errors are never ErrCorrupt.
- `Decode`:
  ```go
  if len(b) == 0 { return nil, ErrCorrupt }
  switch b[0] {
  case version1: return decodeStream(b[1:], false) // Put/Delete only
  case version2: return decodeStream(b[1:], true)  // Put/Delete/Batch
  default:       return nil, ErrCorrupt             // 0, 3, ... rejected before any frame
  }
  ```

### 2.2 `codec.go`: encoding

The type switches are consolidated so that every encode-side switch fails loudly (not silently)
when a kind is missing:

- `normalize(r Record) (Record, error)` is the only place that resolves the dynamic type.
  - `Put`, `Delete` and `Batch` values are returned as they are.
  - Non-nil `*Put`, `*Delete` and `*Batch` are dereferenced.
  - A nil interface, a nil pointer or any other `Record` implementation is an error
    (`unsupported record type %T`).
- `payloadLen(r Record, depth int) (int, error)`. `r` is already normalized. `depth` is the depth
  of the stream that *contains* r's frame. This function now **enforces the field caps itself**,
  which closes the v1 minor finding that the sum could be computed before the caps were checked:
  - `Put`: reject if `len(Key) > maxFrameLen || len(Value) > maxFrameLen`, else return
    `8+len(Key)+len(Value)`.
  - `Delete`: same pattern.
  - `Batch`:
    - Reject first if `depth+1 > maxDepth` (`batch nesting exceeds depth 8`). This check comes
      **before** recursing, so a cyclic `Records` slice cannot overflow the stack (§4).
    - Start with `n := u32Size`. For each inner record, call `fl, err := frameLen(inner, depth+1)`
      (wrap the error with `batch record %d: `). Then `n += lenSize+fl+crcSize`.
    - **Exit as soon as `n > maxFrameLen`.** Each addend is ≤ 16 MiB+8, so `n` stays below 2^26
      and cannot overflow int, even on 32-bit.
  - `default`: `panic("walcodec: internal: unnormalized record")`. This is unreachable, because
    `normalize` filters input first. A missing case therefore crashes in tests instead of
    misencoding.
- `frameLen(r Record, depth int) (int, error)`:
  1. Call `normalize`.
  2. Call `payloadLen`.
  3. Compute `flen := 1+n` and reject if `flen > maxFrameLen`.

  The Batch's own frame is capped exactly like any other.
- `appendFrame(dst []byte, r Record, depth int) []byte`:
  1. `normalize`. The error was already ruled out by the sizing pass.
  2. Recompute `n` via `payloadLen` (or carry it). Write `len = 1+n`, the kind byte and the
     payload, then the crc over `dst[start:]`.
- `appendPayload(dst, r, depth)`:
  - Put/Delete: as in v1.
  - Batch: write `appendU32(dst, uint32(len(Records)))`, then call `appendFrame(dst, inner,
    depth+1)` for each inner record, in order. `len(Records)` fits in uint32 because each inner
    frame is ≥ 9 bytes and the payload is ≤ 16 MiB.
  - `default`: panic (unreachable).
- `fieldsWithinCap` and `recordFrameLen` are deleted; their logic moves into
  `payloadLen`/`frameLen`.
- The sizing pass and the write pass recurse over the same tree in the same order. The sizing pass
  has validated everything, so the write pass cannot fail, and the exact-capacity `make` holds.

### 2.3 Pointer records

This is a behavior change in an area v1 left unspecified. v1 rejected `&Put{}`. The v1 code review
flagged that as a minor risk: Go's method sets make pointers valid `Record`s, and the spec never
excludes them. v2 accepts non-nil pointers to all three kinds and rejects nil pointers. `Decode`
always returns values. This is a strict superset of v1 `Encode`'s accepted input, so no v1 test can
regress.

### 2.4 `codec.go`: decoding

Each piece is shared by every depth:

- `readFrame(r *byteReader) (kind uint8, payload []byte, ok bool)` is extracted from the v1 loop:
  1. Read `u32` len.
  2. If `n == 0 || n > maxFrameLen`, return false **before** `take`. Zero allocations.
  3. `take(n)`, then `u32` crc, then the crc check.
  4. Return `body[0]` and `body[1:]` (sub-slices, no copy).

  Because `r` wraps only the enclosing slice, an inner `len` that reaches past the Batch payload
  fails `take` and returns ErrCorrupt. The bytes of a sibling or outer frame are **structurally
  unreachable**.
- `decodeRecord(kind uint8, payload []byte, depth int, allowBatch bool) (Record, bool)` is the
  **only** decode dispatch:
  - `kindPut` → `decodePut`
  - `kindDelete` → `decodeDelete`
  - `kindBatch`: if `!allowBatch`, reject; else `decodeBatch(payload, depth+1)`
  - `default` → reject
- `decodeStream(p []byte, allowBatch bool) ([]Record, error)` is the top level (depth 0):
  - Loop `while r.remaining() > 0` over `readFrame` and `decodeRecord(..., 0, allowBatch)`.
  - Any failure returns `nil, ErrCorrupt`.
  - If `recs == nil`, return `[]Record{}`.
- `decodeBatch(p []byte, depth int) (Record, bool)`, where `depth` is the depth of this
  sub-stream:
  1. `if depth > maxDepth { return nil, false }`. This check runs first, before reading `n`, so an
     empty `Batch{}` at depth 9 is also rejected. That matches the encoder (§3, rule 7).
  2. `n, ok := r.u32()`. A payload shorter than 4 bytes is rejected.
  3. Preallocate `min(n, r.remaining()/minFrameSize)`. **Never `make` from the untrusted `n`
     directly**: `n = 0xFFFFFFFF` must not allocate 64 GiB.
  4. Run `for i := uint32(0); i < n; i++` over `readFrame` and
     `decodeRecord(kind, pl, depth, true)`. If a frame is missing, the batch has fewer than `n`
     frames, which is truncation: reject.
  5. After the loop, `r.remaining() != 0` → reject. This covers both more than `n` frames and
     trailing bytes.
  6. Return `Batch{Records: recs}`. For `n == 0`, `recs` is `[]Record{}` (non-nil), matching
     top-level `Decode`.
- `decodeV1Frames` is removed and replaced by `decodeStream`.

The outer frame's crc is checked over the whole Batch body before descending. Each inner frame then
checks its own crc. Recursion depth is at most 9 calls, so stack use is bounded.

## 3. How each v2 rule is satisfied

| # | Amendment rule | Mechanism |
|---|---|---|
| 1 | `Batch{Records []Record}`, Kind 3 | §2.1 type and `Kind()` returning `kindBatch` |
| 2 | Payload = `n uint32` + `n` full inner frames | `appendPayload` Batch case; `decodeBatch` reads `n`, then `n` × `readFrame` (same frame shape and crc) |
| 3 | May contain Put/Delete/Batch (recursion) | `appendFrame` → `appendPayload` → `appendFrame`; `decodeBatch` → `decodeRecord` → `decodeBatch` |
| 4 | 16 MiB cap at every depth | `readFrame` (decode) and `frameLen` (encode) are the only frame paths and are used at every depth |
| 5 | Inner `len` past enclosing payload → ErrCorrupt, never reads outer | Inner `byteReader` wraps only the Batch payload sub-slice; `take` fails → ErrCorrupt |
| 6 | Inner frames consume payload exactly; fewer/more/trailing → ErrCorrupt | `decodeBatch` loop of exactly `n` plus a `remaining()==0` check |
| 7 | Depth: top-level = 0, sub-stream > 8 → ErrCorrupt; encoder enforces the same | Decode: `decodeBatch(p, depth+1)` with `depth > maxDepth` checked first. Encode: `payloadLen` rejects `depth+1 > maxDepth` before recursing. 8 nested Batches are legal both ways; 9 are rejected both ways (the public test 7 builds 9 wraps → innermost sub-stream at depth 9 → rejected) |
| 8 | `Encode` writes version 2 (also for Put/Delete-only) | `Encode` appends `version2`; empty input gives `[0x02]`, which still satisfies v1 test 6 (len 1, decodes empty) |
| 9 | Decode accepts v1 (Put/Delete only) and v2 (all) | `Decode` switch; `allowBatch=false` for v1, so a type-3 frame in a v1 stream is ErrCorrupt, as a v1 decoder would report |
| 10 | Any other version (0, 3, …) → ErrCorrupt before any frame | `default` branch of the version switch, before `decodeStream` |
| 11 | v1 regression via a fixed v1 fixture | The v1 path is the same frame/Put/Delete code. The fixture bytes decode to `Put{"alpha","one"}, Delete{"beta"}`, its crc flip fails the crc check, and its truncation fails `take`/`u32` |
| 12 | v1 surface frozen; only `Batch` added | §1; checked with `go doc walcodec` in verification (§6) |

Mapping to the public v2 tests:

1. A Batch round-trips through rows 2–3.
2. A nested Batch round-trips through row 3 (depth 2 is legal).
3. The fixture follows row 9 with `allowBatch=false`.
4. A mixed stream: the top-level loop handles all three kinds, and `Put{last, nil}` decodes to a
   len-0 Value, which `recEq2` compares by string.
5. Truncating `b[:len-2]` leaves the outer Batch body intact, so `take(n)` succeeds. Only 2 of the
   4 crc bytes remain, so the crc `u32()` fails and the result is ErrCorrupt. Any deeper cut fails
   `take(n)` instead.
6. `b[0]=3` hits the version default.
7. 9 wraps fail the depth check.
8. Same as row 11.

All v1 public tests are version-agnostic and stay green. Test 3 hand-builds version 1 with type 99
and still gets ErrCorrupt. Test 5 hand-builds version 1 with an oversize len and still gets
ErrCorrupt.

## 4. Edge cases decided

- **Cyclic input** (`s := make([]Record,1); s[0] = Batch{Records: s}`, or via `*Batch`): `Encode`
  returns the depth error at depth 8, with no stack overflow, because the depth check comes before
  recursion.
- **Empty Batch** `Batch{}` or `Batch{Records: nil}`:
  - Encodes as payload `00 00 00 00`.
  - Decodes as `Batch{Records: []Record{}}`.
  - Legal at sub-stream depth ≤ 8, rejected at 9 on both sides.
- **Aggregate size:** a Batch whose inner frames each fit but whose total frame exceeds 16 MiB is an
  `Encode` error. A frame of exactly 16 MiB is accepted.
- **Batch inside a v1 stream:** ErrCorrupt.
- **Huge `n` with a short payload:** ErrCorrupt. Preallocation is bounded by remaining bytes ÷ 9.
- **Inner `len` that would fit in the *outer* stream but not the Batch payload:** ErrCorrupt (rule 5).
- **Inner crc wrong, outer crc right:** ErrCorrupt.
- **Errors inside batches:** `Encode` wraps them as `record i: batch record j: …` for diagnosis.
  They are never ErrCorrupt.

## 5. Tests (new `worktree/walcodec_impl_test.go`)

- The file uses package `walcodec_test`, so it cannot collide with the public helpers. Every test
  and helper name starts with `Impl`. It uses only the exported surface; constants appear as
  literals and are named locally (e.g. `implVersion2 = 0x02`, so a future version bump is a
  one-line edit).
- The public test files are not touched.

Tests:

1. **Golden bytes:** `Encode([]Record{Batch{[Put{"a","1"}, Delete{"b"}]}})` equals a hand-built
   byte slice. Inner and outer CRCs are computed independently in the test with
   `crc32.ChecksumIEEE`.
2. **Frame format unchanged:** `Encode(v1FixtureRecords)[1:]` equals the v1 fixture `[1:]` (the
   literal is copied into the impl test), and `[0] == 2`.
3. **Kinds:** `Batch{}.Kind() == 3`. A table round-trip covers every kind, as a value and as a
   pointer, at depths 0–8. This catches a missing switch case.
4. **Depth boundary:**
   - 8 nested Batches round-trip.
   - 9 nested Batches: `Encode` returns an error (not ErrCorrupt) and a nil slice.
   - Hand-built 9-deep streams are rejected by `Decode`: one wrapping a Put, and one with an empty
     innermost Batch.
   - A hand-built 8-deep stream decodes.
5. **Cycle:** a self-referential Batch makes `Encode` return an error, not hang or overflow.
6. **Versions:**
   - Version bytes 0, 3 and 255 on an otherwise valid stream are ErrCorrupt.
   - `[0x01]` and `[0x02]` each decode to an empty non-nil slice.
   - A type-3 frame under version 1 is ErrCorrupt.
7. **Batch structure corruption table**, all hand-built with valid outer CRCs so that the
   inner-structure check is the one exercised:
   - `n` larger than the frames present
   - `n` smaller (an extra frame)
   - `n=0` with trailing bytes
   - 1–3 trailing bytes after `n` frames
   - payload shorter than 4 bytes
   - inner `len=0`
   - inner `len > 16 MiB`
   - inner unknown type
   - inner crc flip
   - inner `len` overrunning the Batch payload, with an outer sibling frame after it that would
     satisfy the overrun
   - a Put with trailing bytes inside a Batch
8. **Every prefix** of a mixed nested v2 stream:
   - `[:0]` → ErrCorrupt.
   - `[:1]` → empty, no error.
   - A prefix ending on a top-level frame boundary → the matching record prefix.
   - Any other prefix → ErrCorrupt.
9. **Allocation bounds:** `testing.AllocsPerRun` shows zero allocations for an inner `len > 16 MiB`
   inside a Batch. A Batch with `n = 0xFFFFFFFF` and no frames is ErrCorrupt with bounded
   allocations. The test has a comment saying it must not use `t.Parallel()`.
10. **Encode rejection:**
    - A nil element at top level and inside a Batch.
    - A nil `*Batch`.
    - A foreign `Record` type.
    - An aggregate Batch over 16 MiB (inner records each under the cap).
    - A key over 16 MiB.

    Each must return an error, never `errors.Is(err, ErrCorrupt)`, and a nil slice. A Batch frame
    of exactly 16 MiB encodes and round-trips.
11. **No aliasing:** mutating the input after `Decode` does not change decoded inner Put
    keys/values.
12. **Fuzz `FuzzImplDecode`:**
    - Seeds: the v1 fixture, a nested v2 stream, the depth-8 stream, and the depth-9 stream.
    - Property: if `Decode` succeeds, `Encode(decoded)` succeeds and `out[1:] == in[1:]` with
      `out[0] == 2`. A v1 input re-encodes as v2 with the same frames.
    - On failure, the error must be exactly `ErrCorrupt` with a nil slice.

## 6. Verification (implementation phase)

- `gofmt -l .` must print nothing.
- `go vet ./...`, `go build ./...` and `GOARCH=386 go build ./...` must succeed.
- `go test ./...` (both public suites plus the impl tests), `go test -race ./...` and
  `GOARCH=386 go test ./...` (if the toolchain allows) must pass.
- `go test -fuzz=FuzzImplDecode -fuzztime=30s` must pass.
- `go doc -all walcodec` must show exactly the v1 symbols plus `Batch`.
- Confirm with `diff` that the public test files are byte-identical to `instrument/`.

## 7. v1 review leftovers handled here

- **Too many parallel switches; silent misses (v1 plan and code minor findings):** there is now one
  `normalize` (type resolution), one decode dispatch, and encode switches that panic on unknown
  input. The impl test 3 table covers every kind.
- **Sum before cap check on 32-bit:** `payloadLen` checks caps before summing, and the Batch sum
  exits early.
- **Pointer records rejected:** accepted now (§2.3).
- **Untested oversize-field `Encode` branch:** covered by impl test 10.
- **The alloc test needs a no-parallel comment:** covered by impl test 9.
- **Not changed:** an offset or reason inside `ErrCorrupt`, and exported `Encode` errors. Both
  would widen the frozen surface, which the amendment forbids.
