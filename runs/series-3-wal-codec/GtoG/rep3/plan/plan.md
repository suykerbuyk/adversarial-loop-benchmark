# Plan: `walcodec` v2 — nested `Batch` records

Source of truth: `amendment.md`, `spec-v1.md` (every v1 rule still holds except where the amendment changes it), and the shipped suites `instrument/walcodec_public_test.go` and `instrument/walcodec_v2_public_test.go`.

Existing v1 implementation: `snapshot-v1/walcodec.go` (module `walcodec`, Go 1.25). The worktree currently holds `go.mod` plus both public test files and does **not** contain `walcodec.go`. Implementation copies `snapshot-v1/walcodec.go` into `worktree/walcodec.go` and edits that file only. Do not edit the public test files.

This revision answers `plan/plan_findings.md`. The point-by-point reply is `plan/plan_rebuttal.md`.

## What stays

Frozen exported surface, names, and signatures:

```go
type Record interface { Kind() uint8 }
type Put struct { Key string; Value []byte }
type Delete struct { Key string }
func Encode(recs []Record) ([]byte, error)
func Decode(b []byte) ([]Record, error)
var ErrCorrupt = errors.New("walcodec: corrupt stream")
```

Unchanged v1 wire and behavior:

- Frame shape `[len uint32][type uint8][payload len-1][crc uint32]`, big-endian. `len` counts `type`+`payload` only. CRC32-IEEE (`hash/crc32.ChecksumIEEE`) covers `type`+`payload` only.
- Inclusive frame cap `maxFrameLen = 16 << 20`. A `len` above the cap is `ErrCorrupt` before any allocation sized to that `len`, and before CRC or payload parse. `n == 0` stays `ErrCorrupt` (no type byte).
- Put payload `[klen uint32][key][vlen uint32][value]`. Delete payload `[klen uint32][key]`. Trailing payload bytes after a well-formed Put or Delete are `ErrCorrupt`.
- `nil` and empty `Value` both encode as `vlen = 0` and both decode to a non-nil length-0 `[]byte`. Non-empty values are copied. Keys are copied via `string(...)`.
- Duplicate keys stay, in order. Empty or nil `recs` encode to a one-byte version stream and decode to a non-nil empty `[]Record`. `Decode(nil)` and `Decode([]byte{})` are `ErrCorrupt`.
- Unknown type tags are `ErrCorrupt`. Truncated frames (short length field, short body, short CRC) are `ErrCorrupt`. CRC mismatch is `ErrCorrupt`.
- Encode type switch accepts `Put`, `*Put`, `Delete`, `*Delete`. A nil interface element, a typed nil pointer, and any other concrete type return `(nil, ErrCorrupt)`. Decode returns value records, never pointers.
- `errors.Is(err, ErrCorrupt)` is the only failure signal. Do not wrap `ErrCorrupt`.
- Package stays one file, standard library only (`encoding/binary`, `errors`, `hash/crc32`, `math`).

What does **not** stay verbatim from `snapshot-v1/walcodec.go`:

- `encodePut` (`snapshot-v1/walcodec.go:92`) and `encodeDelete` (`:107`) compare `len(...) > math.MaxUint32`. On `GOARCH=386`, `len` is `int` (32-bit) and that untyped constant does not compile. Rewrite both comparisons as `uint64(len(...)) > math.MaxUint32` before any `uint32(len(...))` conversion. Do not copy the snapshot comparisons.
- `takeBytes` (`snapshot-v1/walcodec.go:220`) narrows `n` with `int(n)` before the range check. On 32-bit, `int(0xFFFFFFFF)` is `-1`. The snapshot does not "pass a negative comparison and then slice"; the comparison `-1 > remaining` is false, then `end := start + int(n)` is `start-1`, and `payload[start:end]` panics. Replace that check with the order in the decode section below. Do not transcribe the snapshot line.

Those two rewrites are required so `go test` compiles and does not panic on 32-bit. They do not change any v1 rule.

## What changes

1. Add exported type `Batch` with Kind 3. Value receiver, matching Put and Delete:

```go
type Batch struct { Records []Record } // Kind 3

func (Batch) Kind() uint8 { return 3 }
```

   Encode also accepts `*Batch`, including a typed nil `*Batch` as `ErrCorrupt`. Decode returns `Batch` by value.

2. Version constant written by Encode becomes `2`. Keep a named constant for version `1` so Decode can accept it. Reject every other version byte, including `0` and `3`, **before** any frame is read.

3. Payload of a Batch is itself a framed sub-stream:

```
Batch payload = n(uint32) frame_1 ... frame_n
frame         = len(4) type(1) payload(len-1) crc(4)
```

   `n` is the count of inner records. The `n` frames must consume the Batch payload exactly. Too few frames, an extra well-formed frame, or any trailing byte is `ErrCorrupt`.

4. Nesting depth. Top-level stream is depth 0. A Batch's inner sub-stream is decoded and encoded one level deeper than the Batch record itself. A sub-stream at depth greater than 8 is `ErrCorrupt`. Equivalently, 8 levels of Batch nesting are legal and a 9th is rejected. The encoder refuses to emit a stream it would reject.

5. Version-1 streams may contain only Put and Delete. A type tag other than 1 or 2 in a version-1 stream, including tag 3, is `ErrCorrupt`. Version-2 streams accept tags 1, 2, and 3.

## Depth convention (one rule)

Reject a sub-stream only when its depth at entry is greater than 8. A Batch frame decoded at depth 7 whose payload is another Batch is legal. Nine wraps fail. Eight wraps succeed.

`TestBatchNestedPastDepth8` wraps a Put in **9** Batch frames and expects `ErrCorrupt`. Walk that stream:

| Sub-stream depth | What is decoded there | Verdict |
|---:|---|---|
| 0 | outermost Batch frame (1 of 9) | legal |
| 1 | Batch 2 | legal |
| 2 | Batch 3 | legal |
| 3 | Batch 4 | legal |
| 4 | Batch 5 | legal |
| 5 | Batch 6 | legal |
| 6 | Batch 7 | legal |
| 7 | Batch 8 | legal; its inner payload is a Batch |
| 8 | Batch 9 | legal frame; recurse with depth 9 |
| 9 | the Put | `ErrCorrupt` on entry, before any frame read |

Eight wraps (the legal maximum the amendment and the public test both accept):

| Sub-stream depth | What is decoded there | Verdict |
|---:|---|---|
| 0 | outermost Batch (1 of 8) | legal |
| 1..6 | Batches 2..7 | legal |
| 7 | Batch 8, whose payload is the innermost records (Put, Delete, or empty) | legal |
| 8 | those innermost records | legal; `depth > 8` is false |

There is no second rule that rejects "a Batch frame at depth 8 whose payload is another Batch." That phrase described nine wraps and was wrong if read as a ban on eight wraps. Delete it. The only check is:

```
if depth > 8 { return ErrCorrupt }   // on entry, before reading any frame of this sub-stream
// a Batch frame decoded at this depth recurses with depth+1
```

A Batch frame whose own body is parsed at depth 8 is legal. Its inner sub-stream is depth 9 and is the thing rejected, whether that inner sub-stream holds a Put, a Delete, or another Batch. The encoder uses the same numbers: `encodeAt(recs, depth)` rejects `depth > 8` before writing a frame. `Encode` starts at depth 0, so eight nested `Batch` values encode, and nine return `(nil, ErrCorrupt)` and a nil buffer.

## Decode algorithm

Replace the version-1-only body of `Decode` with a version gate plus a shared frame walker. The walker is the only place that reads frames, so the 16 MiB cap, CRC, truncation, and "do not read past the enclosing buffer" rules apply at every depth.

```
Decode(b):
  if len(b) < 1: return ErrCorrupt          // nil and empty
  version = b[0]
  if version != 1 && version != 2: return ErrCorrupt   // includes 0 and 3; no frame read
  return decodeStream(b[1:], version, depth=0)

decodeStream(buf, version, depth) -> ([]Record, error):
  if depth > 8: return ErrCorrupt
  out = []Record{}                          // non-nil empty when buf is empty
  off = 0
  while off < len(buf):
    rec, next, err = decodeFrame(buf, off, version, depth)
    if err != nil: return nil, err
    out = append(out, rec)
    off = next
  return out, nil

decodeFrame(buf, off, version, depth) -> (Record, nextOff, error):
  // bounds are against THIS buf, never against an outer buffer
  if len(buf)-off < 4: return ErrCorrupt
  n = uint32be(buf[off:off+4])
  if n > maxFrameLen || n < 1: return ErrCorrupt     // cap before any alloc sized to n
  remain = len(buf) - off
  if remain < 8 || uint64(n) > uint64(remain-8): return ErrCorrupt
  body = buf[off+4 : off+4+int(n)]                   // no copy; CRC and parse only
  crc  = uint32be(buf[off+4+int(n) : off+8+int(n)])
  if crc32.ChecksumIEEE(body) != crc: return ErrCorrupt
  typ, payload = body[0], body[1:]
  next = off + 8 + int(n)
  switch typ:
    1: Put, via existing decodePayloadPut (exact fill, copy value)
    2: Delete, via existing decodePayloadDelete (exact fill)
    3: if version != 2: return ErrCorrupt
       Batch, via decodeBatchPayload(payload, version, depth+1)
    default: return ErrCorrupt
  return rec, next, nil
```

The CRC slice is `buf[off+4+int(n) : off+8+int(n)]`. There is no other CRC expression. The `uint64` remaining check already proves `off+8+int(n) <= len(buf)`, so the slice cannot overflow `int` on 32-bit once `n` has passed the cap (cap is far below `MaxInt32`, and `off+8+n <= len(buf) <= MaxInt`).

`decodeBatchPayload(payload, version, childDepth)`:

```
if len(payload) < 4: return ErrCorrupt          // missing n
n = uint32be(payload[0:4])
rest = payload[4:]
// refuse a count that cannot fit before walking, and never make([]Record, n)
// a minimal frame is 9 bytes: len(4) + type(1) + crc(4), payload empty, n==1
if uint64(n) > uint64(len(rest))/9: return ErrCorrupt
inner, err = decodeStream(rest, version, childDepth)
if err != nil: return err
if uint64(len(inner)) != uint64(n): return ErrCorrupt   // too few or too many frames
return Batch{Records: inner}, nil
```

The pre-check is a bound, not a substitute for the walk. Exact consumption still falls out of `decodeStream`: it walks until `off == len(buf)`. A truncated inner frame (declared `len` extends past `payload[4:]`) fails the remaining-bytes check inside `decodeFrame` and never reads the parent buffer. Trailing bytes that are not a full extra frame also fail that check. An extra complete frame makes `len(inner) != n`.

Allocate the result only by appending frames the walker actually parsed (`out := []Record{}` then `append`). Do not `make([]Record, n)` or `make([]Record, 0, n)` from the attacker-controlled count. After the `/9` check, a legal `n` is at most `len(rest)/9`, which fits in `int`, so the later `uint64(len(inner)) != uint64(n)` compare cannot overflow.

Empty Batch: `n = 0` and payload length exactly 4. The `/9` check passes (`0 > 0` is false). `decodeStream` on an empty slice at a legal depth returns `[]Record{}`. That is the encoded form of `Batch{Records: nil}` and of `Batch{Records: []Record{}}`.

Version is passed down so a Batch that appears in a version-1 stream is rejected on the type tag even if some other bug let the frame through. Encode never writes a Batch into a version-1 stream; this is a decode-only guard.

Order of checks inside a frame stays: length field present, cap (and `n >= 1`), remaining bytes in **this** buffer, CRC, then type, then payload. Oversize `len` still allocates nothing proportional to `len`.

`takeBytes` (used by Put and Delete payload parse), required order:

```
if i < 0 || i > len(payload)-lenSize: return ErrCorrupt
n = uint32be(payload[i:i+4])
// remaining bytes after the length field; subtraction is safe because i <= len-4
avail = len(payload) - (i + lenSize)
if uint64(n) > uint64(avail): return ErrCorrupt    // reject 0xFFFFFFFF before narrowing
start = i + lenSize
end = start + int(n)                               // n already proven <= avail <= MaxInt
return payload[start:end], end, nil
```

Do not write `int(n) > remaining` and do not slice until the `uint64` compare has passed. On 32-bit the failure mode of the snapshot line is a panic in `payload[start:end]` because `int(n)` is `-1` and `end < start`, not a comparison that lets a negative length "pass" as a successful read.

## Encode algorithm

```
Encode(recs):
  return encodeAt(recs, depth=0)     // always writes version 2, even for Put/Delete-only input

encodeAt(recs, depth) -> ([]byte, error):
  if depth > 8: return nil, ErrCorrupt
  buf = empty
  if depth == 0: buf = []byte{2}     // one path for the version byte, including empty recs
  for rec in recs:
    buf, err = appendRecord(buf, rec, depth)
    if err != nil: return nil, err   // discard partial buf
  return buf, nil

appendRecord(dst, rec, depth):
  switch r := rec.(type):
    Put:          encodePut(dst, r)
    *Put:         nil -> ErrCorrupt; else encodePut(dst, *r)
    Delete:       encodeDelete(dst, r)
    *Delete:      nil -> ErrCorrupt; else encodeDelete(dst, *r)
    Batch:        encodeBatch(dst, r, depth)
    *Batch:       nil -> ErrCorrupt; else encodeBatch(dst, *r, depth)
    default:      return nil, ErrCorrupt
```

One version-byte path. `len(recs) == 0 && depth == 0` is not a separate return. An empty top-level call writes `[]byte{2}` and the loop does not run. A nested call with empty records writes no version byte and no frames. Do not add a second literal `[]byte{2}` anywhere else.

Do not call `Kind()` to dispatch. A nil interface panics on `Kind()`, and a typed nil pointer's value-receiver `Kind()` would return 3 and then panic on field access.

`encodePut` and `encodeDelete` keep the v1 payload layout and the call to `appendFrame`, with the length checks rewritten so they compile on 32-bit:

```
encodePut:
  if uint64(len(r.Key)) > math.MaxUint32 || uint64(len(r.Value)) > math.MaxUint32:
    return nil, ErrCorrupt
  // only then uint32(len(r.Key)) and uint32(len(r.Value))

encodeDelete:
  if uint64(len(r.Key)) > math.MaxUint32:
    return nil, ErrCorrupt
  // only then uint32(len(r.Key))
```

On amd64 the `uint64` compare is a real check (`MaxInt > MaxUint32`). On 386 it compiles and is vacuously false (`len` already fits in 32-bit `int`). Either way `uint32(len(...))` is reached only after the compare succeeds. `appendFrame` still rejects a payload longer than `maxFrameLen-1` before writing `len`.

`encodeBatch(dst, b Batch, depth)`:

```
// count check BEFORE recursion, and as uint64 so it compiles on amd64 and 386
if uint64(len(b.Records)) > math.MaxUint32:
  return nil, ErrCorrupt
// child sub-stream sits at depth+1; encodeAt rejects depth > 8 before writing
inner, err = encodeAt(b.Records, depth+1)   // NO version byte; frames only
if err != nil: return nil, err
payload = appendU32(nil, uint32(len(b.Records)))   // safe: compare already passed
payload = append(payload, inner...)
return appendFrame(dst, 3, payload)
```

The `uint64` compare is not optional and is not placed after `encodeAt`. On amd64, `len(b.Records) > math.MaxUint32` does not compile. On 386 that same form is vacuous, so it would not catch a wrapped `uint32` conversion if `len` could exceed `MaxUint32` — it cannot, on 386, but the check still belongs before the conversion and before the recursive call so a later port cannot write a truncated count. Do not write `uint32(len(b.Records))` on any path that skipped the compare.

`encodeAt` at `depth > 0` must **not** prepend a version byte. Only the top-level stream has a version. An empty nested Batch therefore encodes as a frame whose payload is the four bytes `00 00 00 00`.

Frame cap applies to the Batch payload too: `4 + len(inner)` must be `<= maxFrameLen-1` or `appendFrame` returns `ErrCorrupt`. A Batch whose inner frames would make the payload exceed 16 MiB − 1 is rejected by the encoder. The decoder independently rejects an inner `len` that claims more than 16 MiB or that extends past the enclosing payload.

`encodeAt` for a non-empty top-level slice and for every nested call builds frames with the same `appendFrame`. Put and Delete payload helpers are not duplicated.

## Version-1 fixture compatibility

`v1Fixture()` is the fixed bytes (not a replay of Encode):

```
01
00 00 00 11  01  00 00 00 05 61 6c 70 68 61  00 00 00 03 6f 6e 65  20 74 5b 79
00 00 00 09  02  00 00 00 04 62 65 74 61  93 aa 41 97
```

That is version 1, then Put key `alpha` value `one`, then Delete key `beta`. Decode must return exactly those two value records. Flipping the last byte fails CRC. Dropping the last 3 bytes fails the remaining-bytes check. Both are `ErrCorrupt`.

v1 public tests never assert that Encode's first byte is 1. They round-trip through Encode/Decode, so they stay green when Encode writes 2. `TestUnknownType` builds its own version-1 stream with tag 99; the version-1 allow-list still rejects it. `TestOversizeLen` uses version 1 and `len = 16<<20+1`; the cap check is unchanged and still runs before allocation. `TestEmptyRecsVersionOnly` only checks `len(b) == 1`.

## Rule-by-rule satisfaction

| Amendment rule | How the plan meets it |
|---|---|
| `Batch{Records []Record}`, Kind 3 | Exported struct, value-receiver `Kind()` returns 3. Pointer form accepted only on encode, with nil rejected. |
| Payload is `[n uint32]` plus `n` frames of the same shape | `encodeBatch` writes the count then the version-less inner frames. `decodeBatchPayload` reads the count and calls the shared walker on `payload[4:]`. |
| Inner frames may be Put, Delete, or Batch | Type switch arms 1, 2, 3. Recursion passes `depth+1` and the same version. |
| Same 16 MiB cap at every depth | `decodeFrame` applies `n > maxFrameLen` against every enclosing buffer, including a Batch payload. `appendFrame` applies the same cap when writing. |
| Inner `len` past the enclosing payload is truncated `ErrCorrupt`, never a read into the next outer frame | Remaining-bytes check uses `len(buf)-off` of the sub-stream slice only. The parent buffer is not in scope. |
| `n` frames must consume the payload exactly | Walker requires `off == len(buf)`. Then `uint64(len(inner))` must equal `n`. Trailing bytes, short payloads, and extra frames are `ErrCorrupt`. The `/9` pre-check rejects an impossible count before the walk and does not replace the exact-fill check. |
| Depth: top-level is 0; Batch inner stream is one deeper; depth > 8 is `ErrCorrupt` | Single check at entry of `decodeStream` / `encodeAt`: `depth > 8`. Eight wraps succeed. Nine wraps put the innermost sub-stream at depth 9 and fail. |
| Encoder will not emit a stream it would reject | `encodeAt` rejects `depth > 8` before writing. Encode of nine nested Batches returns `ErrCorrupt` and a nil buffer. Encode of eight nested Batches succeeds. |
| Encode writes version 2, including Put/Delete-only input | The single `depth == 0` prefix writes `2`. Nested calls do not prefix a version. |
| Decode accepts version 1 (Put/Delete only) and version 2 (all three) | Version gate, then tag 3 allowed only when version is 2. |
| Any other version, including 0 and 3, is `ErrCorrupt` before any frame | Gate is the first check after the missing-version check. |
| Frozen v1 symbols | No signature or name changes. Only new exported type is `Batch`. |
| v1 decode behaviors hold on a fixed version-1 fixture | Fixture path is the version-1 arm of the same walker (CRC, truncation, records, unknown type, oversize). |

## Public tests (must pass; do not modify)

v2 suite, each mapped to the algorithm:

1. `TestBatchRoundTrip` — one Batch at depth 0 containing Put and Delete. Encode writes version 2 and one Kind-3 frame. Decode rebuilds `Batch` by value; `recEq2` compares Put by string(value), so the non-nil empty-value policy is fine. This case uses non-empty values.
2. `TestNestedBatchRoundTrip` — outer Batch at sub-stream depth 0, inner Batch payload decoded at depth 1. Both under the cap.
3. `TestV1FixtureDecodesUnderV2` — version byte 1, two frames, tags 1 and 2, exact records `Put{alpha,one}` and `Delete{beta}`.
4. `TestMixedTopLevelRoundTrip` — Put, Delete, Batch, Put with nil Value. Nil Value decodes to length 0; `string(x.Value) == string(y.Value)` holds.
5. `TestTruncatedBatch` — drop last 2 bytes of a successful Encode. The outer frame's CRC or remaining-bytes check fails (the cut lands inside the outer CRC for this small record). Either way `ErrCorrupt`. The same walker rejects a cut that lands inside the Batch payload.
6. `TestVersion3Rejected` — Encode then `b[0] = 3`. Version gate returns `ErrCorrupt` without reading the frame.
7. `TestBatchNestedPastDepth8` — hand-built version 2, Put wrapped in 9 Batch frames. Innermost sub-stream depth is 9. `decodeStream` returns `ErrCorrupt` on entry at depth 9. The ninth Batch frame itself is parsed at depth 8 and is legal. Encoder is not involved. A hand-built stream with 8 wraps must decode successfully; the encoder must also emit that shape for eight nested `Batch` values.
8. `TestV1BehaviorsViaFixture` — fixture records match; last-byte CRC flip fails CRC; drop of 3 bytes fails remaining-bytes. Neither path calls Encode.

v1 suite stays as the regression suite. It does not assert version == 1 on Encode output. After the version bump it still round-trips, still rejects CRC flips, unknown tag 99 on a version-1 hand-built stream, truncation, oversize `len`, nil/empty Decode, duplicates, and nil-vs-empty Value.

## Implementation steps (when implementation is allowed)

1. Copy `snapshot-v1/walcodec.go` to `worktree/walcodec.go`.
2. Add `kindBatch = 3`, `versionV2 = 2`, `maxBatchDepth = 8`. Keep `versionV1 = 1`. Encode writes `versionV2`.
3. Add `Batch` and `(Batch) Kind()`.
4. Rewrite `encodePut` and `encodeDelete` length checks to `uint64(len(...)) > math.MaxUint32` before any `uint32` conversion. Do not leave the snapshot comparisons.
5. Split the current `Decode` loop into `decodeStream` / `decodeFrame` parameterized by version and depth. Keep `decodePayloadPut`, `decodePayloadDelete`, and `copyValue`. Rewrite `takeBytes` to the `uint64` order above before any `int(n)` slice.
6. Add `decodeBatchPayload` with the `/9` count bound before the walk, and allocate only by appending parsed frames.
7. Thread depth through encode. Add `*Batch` / `Batch` arms. `encodeAt` writes the version byte only in the `depth == 0` arm, including the empty-input case. `encodeBatch` does the `uint64` count check before `encodeAt`.
8. `go test ./...` in `worktree` (both public files). `go vet ./...`. No new dependencies. A 32-bit compile check (`GOARCH=386 go test -c` or `GOARCH=386 go vet`) confirms the length comparisons compile; running the tests on 386 is optional if the host cannot execute 386 binaries.

## Edges the implementation must not get wrong

- Nested `encodeAt` must not write a version byte. A version byte inside a Batch payload would desynchronize `n` and fail the exact-fill check on decode.
- Depth is the sub-stream depth, checked on entry with `depth > 8`. Eight nested Batches encode and decode. Nine fail. Checking `depth >= 8` when seeing a Batch tag would reject the legal eighth wrap. Do not add a second rejection for "Batch frame at depth 8."
- The count `n` is not the depth and is not the frame `len`. Do not treat `n > 8` as a depth error. Do not size a slice from `n`.
- An inner `len` is bounded by the Batch payload slice, not by the outer stream. Slicing the parent with an inner `len` is a spec violation even if the CRC later fails.
- Version check happens before the frame loop. A version-3 stream whose first frame is also truncated is still `ErrCorrupt`, and the implementation must not depend on reaching that frame.
- `Batch{Records: nil}` and `Batch{Records: []Record{}}` both encode as `n = 0` and both decode as `Batch{Records: []Record{}}` (non-nil empty). Public tests always use non-empty `Records` or compare via `recEq2`, which uses `len`. Keep the non-nil empty policy so a successful decode of `n = 0` matches a successful decode of an empty top-level stream.
- Foreign record types, nil interface elements, and typed nil pointers remain `ErrCorrupt` on encode, including inside a Batch. A Batch that fails mid-way returns `(nil, ErrCorrupt)` for the whole `Encode`, not a partial version-2 prefix.
- Do not export helpers. Do not change `ErrCorrupt`'s text. Do not add a version argument to `Encode` or `Decode`.

## Out of scope

No new record kinds, no compression, no length-prefix on the outer stream, no change to Put/Delete payload layout, no rewrite of the public tests, no dependency beyond the standard library.
