# Plan: `walcodec` v2 — nested `Batch` records

This plan extends the converged v1 codec in `snapshot-v1/codec.go`. The worktree currently holds `go.mod` and the public tests only. Implementation starts by restoring that v1 file as `worktree/codec.go` and editing it in place.

Every v1 rule still holds except where the amendment changes it. The only wire and API changes are the `Batch` type, version 2 on encode, versions 1 and 2 on decode, and the depth cap.

Prior findings in `plan/plan_findings.md` are accepted and folded in below. The point-by-point response is in `plan/plan_rebuttal.md`.

## What stays

Frozen exported surface, names, and signatures:

```go
type Record interface { Kind() uint8 }

type Put    struct { Key string; Value []byte }
type Delete struct { Key string }

func Encode(recs []Record) ([]byte, error)
func Decode(b []byte) ([]Record, error)

var ErrCorrupt = errors.New("walcodec: corrupt stream")
```

Unchanged behavior:

- Frame shape stays `[len uint32][type uint8][payload][crc uint32]`, big-endian. `len` counts `type`+`payload` only. `crc` is CRC32-IEEE (`hash/crc32.ChecksumIEEE`) over `type`+`payload` and sits outside `len`.
- `Put` is kind 1, payload `[klen uint32][key][vlen uint32][value]`. `Delete` is kind 2, payload `[klen uint32][key]`. `Put{}.Kind()` is 1, `Delete{}.Kind()` is 2.
- A `nil` `Value` and an empty `Value` both encode as `vlen = 0` and both decode to a non-nil length-0 `[]byte`. They compare equal.
- Duplicate keys are kept, in stream order. The codec is a log, not a map.
- `Encode(nil)` and `Encode` of an empty slice return a one-byte stream. Decoding that stream returns a non-nil empty slice and a nil error.
- `Decode(nil)` and `Decode([]byte{})` return `ErrCorrupt` (missing version byte).
- Every wire failure returns the bare `ErrCorrupt` sentinel, not a wrapped error. On error the record slice is nil.
- `len` is capped at 16 MiB (`16 << 20`). A length above the cap is `ErrCorrupt`, checked before any allocation of that frame's body. A length equal to the cap is legal if the bytes are present. A length of 0 is `ErrCorrupt` (no type byte).
- Unknown type tags are `ErrCorrupt`. A CRC mismatch is `ErrCorrupt`. A truncated frame is `ErrCorrupt`.
- Payload inner lengths must consume the payload exactly. A hostile `klen`/`vlen` that would walk off the payload, or trailing bytes inside a `Put` or `Delete` payload, is `ErrCorrupt`. Comparisons use `uint64` so a hostile length cannot wrap a slice bound.
- Decoded keys and values are copies. The caller can mutate the input buffer after `Decode` without changing the result.
- `Encode` accepts only `Put`, `Delete`, and (new) `Batch` values. `*Put`, `*Delete`, `*Batch`, a nil interface, a typed-nil pointer, and any other concrete type return a non-`ErrCorrupt` error (`errUnknownRecord`, wrapped with `%T` or the word `nil`). `Kind()` is not consulted to accept a custom type.
- Encode errors that are not wire corruption stay non-`ErrCorrupt`: `errUnknownRecord`, `errFrameTooLarge`, and (new) `errTooDeep`. An oversize record, including a `Batch` whose type+payload is `maxFrameLen+1`, is rejected in the size pass before any of that record's bytes are written and before the output buffer is allocated.
- Frame sizing uses `int` comparisons against `maxFrameLen`, never `math.MaxUint32`, so the package builds on 386. Running totals use `addSize` so a huge stream cannot wrap `int` and panic in `make`.
- Version is validated before any frame. CRC is checked before the type byte is interpreted.
- v1 public tests stay as shipped. They are version-agnostic: they do not assert that `Encode` writes version 1. They must keep passing against the v2 codec.
- Doc comments that still say "version-1" or "Put and Delete only" are updated so they match the code. The wire rules they describe stay.

## What changes

### New type

```go
type Batch struct { Records []Record } // Kind 3
func (Batch) Kind() uint8 { return 3 }
```

`Batch` is a value type, same convention as `Put` and `Delete`. `Decode` returns `Batch` values, never pointers, so the v2 tests' type switch (`case Batch:`) matches.

A `Batch` may contain `Put`, `Delete`, or nested `Batch` values. An empty `Records` slice and a nil `Records` slice are the same on the wire: `n = 0` and no inner frames. `Decode` returns `Batch{Records: []Record{}}` (non-nil empty) for that case, matching the version-only stream rule one level down.

### Version byte

- `Encode` always writes `version = 2`, including for Put/Delete-only input and for an empty record slice. That is an allowed behavior change. The v1 public suite does not assert the written version.
- `Decode` accepts version 1 and version 2. Any other byte, including 0 and 3, is `ErrCorrupt`, checked before any frame is read.
- Version 1 and version 2 share the same frame parser. The only version-specific rule is which type tags are legal:
  - version 1: kinds 1 and 2 only. Kind 3, or any other tag, is `ErrCorrupt`.
  - version 2: kinds 1, 2, and 3. Any other tag is `ErrCorrupt`.
- A version-1 stream that contains a `Batch` is rejected even if the frame CRC is valid. The fixed v1 fixture has no `Batch`, so this does not affect the regression tests.

The version and the current depth live on one small value, so a later version is a table change, not a signature change across every helper:

```go
type decoder struct {
    version uint8
    depth   int
}
```

`decodeUntilEnd`, `decodeExactly`, `nextFrame`, and `decodeFrame` are methods on `decoder`. `parseBatch` builds `decoder{version: d.version, depth: d.depth + 1}` for the inner span. Encode keeps a depth `int` argument on `frameSize` and the append helpers; that path does not need a version field because it always writes 2.

### `Batch` payload

```
Batch payload = n(uint32) frame_1 frame_2 ... frame_n
frame         = len(4) type(1) payload(len-1) crc(4)
```

- `n` is the count of inner records. The `n` inner frames must consume the Batch payload exactly. Fewer well-formed frames than `n`, more bytes than `n` frames, or trailing bytes after `n` frames, is `ErrCorrupt`.
- `n = 0` requires a 4-byte payload and nothing after it. Extra bytes after a zero count are `ErrCorrupt`.
- Each inner frame is a full `[len][type][payload][crc]` frame, with the same 16 MiB `len` cap, the same CRC rule, and the same payload parsers as a top-level frame.
- An inner `len` that would read past the enclosing Batch payload is `ErrCorrupt` (truncated). The decoder never reads into a sibling frame or an outer frame. This is enforced by slicing the Batch payload into its own span and decoding that span only. The outer cursor does not move until the whole outer frame (length, body, CRC) has been accepted.

Short payload, before any read of `n`:

- `parseBatch` returns `ErrCorrupt` when `len(payload) < 4`. It does not slice `payload[0:4]` first. A Batch frame whose type+payload length is 1, 2, 3, or 4 (the type byte alone, or the type byte plus 0–3 payload bytes) with a valid CRC is `ErrCorrupt`, not a panic. Payload lengths 0, 1, 2, and 3 are this case, including `mkFrame2(3, nil)` and `mkFrame2(3, []byte{0, 0})`.

### Untrusted count `n`: no allocation sized from it

`n` is attacker-controlled. It is never passed to `make`, `make` capacity, or any other size argument. The result slice grows only by `append` of records that have already passed the length cap, the CRC check, and payload parsing. A hostile `n = 0xFFFFFFFF` inside a short, CRC-valid Batch frame must return `ErrCorrupt` and must not attempt a multi-gigabyte allocation (OOM on amd64, `makeslice` panic on 386).

Before any inner frame is decoded, reject an `n` the payload cannot hold. The smallest legal inner frame is 13 bytes: 4-byte length word, 1 type byte, 4-byte body (empty `Delete` is `klen = 0`; empty `Batch` is `n = 0`), and 4-byte CRC. After the 4-byte count, the remainder must be able to hold `n` such frames:

```text
if len(payload) < 4 {
    return nil, ErrCorrupt
}
n := binary.BigEndian.Uint32(payload[:4])
rest := payload[4:]
if uint64(n)*minInnerFrame > uint64(len(rest)) {
    return nil, ErrCorrupt
}
```

`minInnerFrame` is the constant 13. The multiply is `uint64`, so `n = 0xFFFFFFFF` cannot wrap. This check runs before `decodeExactly` and before any `append`. It is the same "reject before allocation" rule the v1 oversize-`len` path already uses. An empty `Delete` and an empty `Batch` are both 13 bytes on the wire, so a stream the encoder emits always passes this check.

`n = 0` passes the inequality (product is 0) and then requires `len(rest) == 0`.

On every error path, `parseBatch` and `decodeFrame` return a nil `Record` interface (`nil, ErrCorrupt`). They do not return a `Batch` zero value boxed into the interface. That conversion allocates nothing, so the huge-`n` and short-payload `AllocsPerRun` tests stay at 0.

### Exact consumption: two functions, no limit sentinel

Top-level streams stop at the end of the span. A Batch sub-stream stops after exactly `n` frames. Those are separate functions that share one `nextFrame` step. There is no `limit` argument and no sentinel value meaning "until the end".

A `limit int` of `-1` is not used. On `GOARCH=386`, `int(uint32(0xFFFFFFFF)) == -1`, so a sentinel would collide with a hostile count if the conversion ever ran. `decodeExactly` takes `n uint32` and counts with a `uint32`. It does not convert `n` to `int`.

```text
nextFrame(span, i) (Record, next int, error):
    existing cursor rule, inside this span only
    CRC verified before the type byte is interpreted
    oversize or zero len rejected before the body is sliced
    kind 3 only when version == 2; parseBatch enters depth+1
    kind 1 or 2: parsePut / parseDelete
    other kinds: ErrCorrupt

decodeUntilEnd(span) (recs []Record, error):
    if depth > 8: return nil, ErrCorrupt
    var out []Record
    i := 0
    for i < len(span):
        rec, next, err := nextFrame(span, i)
        if err != nil: return nil, err
        out = append(out, rec)
        i = next
    if out == nil: out = []Record{}
    return out, nil

decodeExactly(span, n uint32) (recs []Record, end int, error):
    if depth > 8: return nil, 0, ErrCorrupt
    // called only after uint64(n)*13 <= len(span)
    var out []Record
    i := 0
    for parsed := uint32(0); parsed < n; parsed++ {
        rec, next, err := nextFrame(span, i)
        if err != nil: return nil, 0, err
        out = append(out, rec)
        i = next
    }
    return out, i, nil

parseBatch(payload, d):
    if len(payload) < 4: return nil, ErrCorrupt
    n := Uint32(payload[:4])
    rest := payload[4:]
    if uint64(n)*13 > uint64(len(rest)): return nil, ErrCorrupt
    inner := decoder{version: d.version, depth: d.depth + 1}
    recs, end, err := inner.decodeExactly(rest, n)
    if err != nil: return nil, err
    if end != len(rest): return nil, ErrCorrupt
    if uint64(len(recs)) != uint64(n): return nil, ErrCorrupt
    if recs == nil: recs = []Record{}        // n == 0
    return Batch{Records: recs}, nil
```

`decodeExactly` parses at most `n` frames and returns the cursor. It does not keep parsing after `n` frames. The depth check runs before the `n == 0` success path, so an empty Batch whose sub-stream sits at depth 9 is `ErrCorrupt`.

- `n = 1` with a million trailing well-formed frames fails at `end != len(span)` after one frame. Those extra frames are not built into records.
- `n = 2` with one well-formed frame and no trailing bytes fails as truncation inside the second `nextFrame`, or as `len(recs) != n` if the span ends cleanly after one frame. Either path is `ErrCorrupt` and a nil slice.
- `n = 0` with trailing bytes fails the end check. No frames are parsed.
- A well-formed inner stream whose count matches and whose frames end exactly at `len(rest)` succeeds.

The top-level stream has no count word. `Decode` calls `decodeUntilEnd`. A version-only stream returns a non-nil empty slice.

Neither walker does `make([]Record, 0, n)`. The result slice is allocated by the first `append`, after that frame has passed the length cap, matching today's `decodeFramesV1`.

### Depth

Decoding and encoding carry a nesting depth.

- The top-level stream is depth 0.
- A `Batch` encountered at depth `d` has its inner sub-stream decoded (and encoded) at depth `d+1`.
- A sub-stream at depth greater than 8 is `ErrCorrupt`. Equivalently, 8 levels of `Batch` nesting are legal; a 9th is rejected.
- The public test wraps a `Put` in 9 `Batch` frames and prefixes version 2. That stream is depth 0 at the top, and the 9th wrap produces a sub-stream at depth 9. `Decode` must return `ErrCorrupt`.
- The encoder enforces the same cap and will not emit a stream it would reject. A caller-built `Batch` nested 9 deep returns `errTooDeep`. It is not `ErrCorrupt`: `ErrCorrupt` is for wire failures. The public test builds the over-deep stream by hand because the encoder refuses to emit it.
- Depth 8 is legal. A `Put` wrapped in exactly 8 `Batch` frames encodes and decodes. A hand-built version-2 stream of the same shape (the legal mirror of public test 7, using `mkFrame2`) must also decode, so the decoder cap is pinned independently of `Encode`.
- An empty `Batch` whose sub-stream sits at depth 8 is legal. That is an empty `Batch` frame encountered in a stream at depth 7. An empty `Batch` whose sub-stream would sit at depth 9 is rejected on encode (`errTooDeep`) and on decode (`ErrCorrupt`). The cap is on the sub-stream depth, not on whether the inner slice is empty.

`maxDepth` is documented as: top-level stream is depth 0; a Batch sub-stream is one deeper than the Batch frame; a sub-stream at depth greater than `maxDepth` (8) is rejected.

### Shared frame loop

Replace the version-1-only `decodeFramesV1` with the two walkers above:

```text
Decode(b):
  if len(b) < 1: ErrCorrupt
  version = b[0]
  if version is not 1 and not 2: ErrCorrupt
  return decoder{version, depth 0}.decodeUntilEnd(b[1:])
```

The cursor rule does not change. `i` is the next unread byte inside the current span. After a 4-byte length word is accepted, `i` advances by 4, then by `n+4` (type+payload and CRC). The span is the whole remainder of the outer buffer at depth 0, or exactly the bytes after the Batch count at depth > 0. An inner length that extends past that span fails the existing `len(span)-i < need` check and returns `ErrCorrupt`. No read crosses the span.

CRC is still verified before the type byte is interpreted. An oversize `len` is still rejected before the body is sliced for parsing and before any record slice is allocated for that frame. The first-frame oversize path stays allocation-free: the result slice is not allocated until a frame has passed the length cap. A version-only stream and an empty Batch (`n = 0`, 4-byte payload) both return a non-nil empty record slice.

CRC amplification is accepted and documented, not optimized away. A byte that sits inside a Batch at nesting depth `d` is covered by `d+1` CRC checks (its own frame, plus each enclosing Batch frame). At depth 8 that is 9 checks of the innermost body. Each check is bounded by the 16 MiB cap. Skipping an outer CRC would accept a corrupt enclosing frame. Do not skip it.

### Encode path

Keep a validating size pass, then a single allocation, then a write. The size pass exists to reject unknown types, oversize frames, and depth greater than 8 before the output buffer is allocated, and to pick `cap` so `append` does not grow. It is not a second layout algorithm that the writer must re-derive.

**`streamSize` is the size pass Encode calls.** Tests in package `walcodec` call the same helper. It returns the total byte count of the stream, including the version byte.

```text
streamSize(recs) (int, error):
  n := 1                                    // version byte
  for each r in recs:
    frameLen, err := frameSize(r, 0)
    if err != nil: return 0, err
    n, ok = addSize(n, 4+frameLen+4)        // len word + type+payload + crc
    if !ok: return 0, errFrameTooLarge
  return n, nil
```

**Early abort in `frameSize`.** Sums are checked as they grow. As soon as a partial Batch size exceeds `maxFrameLen`, or `addSize` fails, `frameSize` returns `errFrameTooLarge` and does not visit the remaining children. Depth greater than 8 returns `errTooDeep` before children of that illegal level are visited.

`total` is type+payload. That is the quantity the length word stores, and the same quantity `putFrameLen` and `deleteFrameLen` return (those helpers include the type byte). It starts at `1 + 4` (type byte + `n`). Each child adds that child's full frame: 4-byte length word + child type+payload + 4-byte CRC. The Batch's own length word and CRC are outside `total`; the caller adds them as `4+total+4`.

The cap check is `total > maxFrameLen`. A `total` equal to `maxFrameLen` is legal. The Batch's own length word and CRC are not subtracted before the comparison.

```text
frameSize(r, depth) (int, error):
  switch r {
  case Put, Delete: existing helpers, which return type+payload
  case Batch:
    if depth+1 > 8: return 0, errTooDeep
    total := 5                              // type + n; already type+payload
    for each child:
      childLen, err := frameSize(child, depth+1)
      if err != nil: return 0, err
      total, ok = addSize(total, 4+childLen+4)
      if !ok || total > maxFrameLen: return 0, errFrameTooLarge
    return total, nil
  case nil:
    return 0, fmt.Errorf("%w: nil", errUnknownRecord)
  default:
    return 0, fmt.Errorf("%w: %T", errUnknownRecord, r)
  }
```

Worked boundary, one `Delete` child. `maxFrameLen = 16 << 20 = 16777216`.

```text
delete type+payload     = 5 + keyLen
child frame inside Batch = 4 + (5+keyLen) + 4 = 13 + keyLen
Batch type+payload      = 5 + (13 + keyLen) = 18 + keyLen
```

- Key length `16<<20 - 18` (`16777198`): type+payload `16777216`, equal to the cap. `total > maxFrameLen` is false. Encode accepts it.
- Key length `16<<20 - 17` (`16777199`): type+payload `16777217`. `total > maxFrameLen` is true. `frameSize` returns `errFrameTooLarge` before `Encode` reaches `make`.

A check written as `total-1 > maxFrameLen` accepts the second input, because `total-1` is the payload only. That check is not used. Encode's top-level loop does not add a second cap test that would hide a wrong formula inside `frameSize`; `streamSize` trusts `frameSize`. The backfill check is a drift guard, described below, and is not the check these boundary tests measure.

A cyclic `Batch` is built so the slice and the value share one backing array. `Batch` is a value type; assigning the value into a freshly built slice stores a copy whose `Records` is nil, and that input is a legal depth-1 nest.

```go
recs := make([]Record, 1)
b := Batch{Records: recs}
recs[0] = b
```

`frameSize` on that value returns `errTooDeep` after 9 calls, before any size cap is hit. A width-aliased tree that shares one child slice still cannot recurse forever: the depth cap stops at 8, and the early cap stops a wide alias from being fully expanded once any root-to-leaf size sum passes 16 MiB. Work is bounded by about `maxFrameLen / 13` successful child visits per path, times at most 8 levels, and a failing path returns immediately. There is no separate "len(Records) must fit in uint32" check: that comparison does not compile on 386 if written as `len(x) > math.MaxUint32`, and it is unreachable because the 16 MiB cap already limits a Batch to about 1.29e6 children (each at least 13 bytes). `n` written on the wire is `uint32(len(Records))` only after the size pass has accepted the frame.

**Reserve-and-backfill for the length word.** `appendBatch` does not call `frameSize` again.

```text
appendBatch(buf, b, depth) ([]byte, error):
  if depth+1 > 8: return nil, errTooDeep
  start = len(buf)
  buf = append 4 zero bytes                  // placeholder for len
  payloadStart = len(buf)
  buf = append type 3
  buf = append uint32(len(b.Records))
  for each child:
    switch child {
    case Put:    buf, err = appendPut(buf, child)
    case Delete: buf, err = appendDelete(buf, child)
    case Batch:  buf, err = appendBatch(buf, child, depth+1)
    case nil:    return nil, fmt.Errorf("%w: nil", errUnknownRecord)
    default:     return nil, fmt.Errorf("%w: %T", errUnknownRecord, child)
    }
    if err != nil: return nil, err
  frameLen = len(buf) - payloadStart         // type + payload, same quantity as frameSize's total
  if frameLen > maxFrameLen: return nil, errFrameTooLarge
  binary.BigEndian.PutUint32(buf[start:start+4], uint32(frameLen))
  buf = append CRC32-IEEE(buf[payloadStart:])
  return buf, nil
```

The nil and default arms return `errUnknownRecord`. They do not skip the child. `n` has already been written as `len(Records)`; skipping a child would emit a count that disagrees with the frames that follow. The size pass already rejects those children, so this arm is the write-path counterpart of `frameSize`'s default, matching the v1 top-level loop.

`appendPut` and `appendDelete` stay as they are: their length is a function of the key and value, already checked, and they write the length word before the body. Only `Batch` needs backfill, because its length depends on the children just written. One code path computes the bytes; the placeholder is overwritten with the length those bytes actually have. The size pass and the write pass cannot drift by a recomputed formula.

The backfill comparison is `frameLen > maxFrameLen`. Equality is legal, so a Batch whose type+payload equals the cap is written. This check stays in the writer so a size/write drift still cannot emit a length word above the cap. It is not the validator for the cap. The size pass is the validator. A boundary input of `maxFrameLen+1` must fail inside `frameSize`, before `make`. A failure that appears only after the buffer exists does not satisfy the encode contract.

`Encode`:

1. Calls `streamSize`. On any error, returns that error and does not allocate the output buffer.
2. Allocates `make([]byte, 1, n)` where `n` is the `streamSize` total.
3. Writes version byte `2`.
4. Appends frames with the helpers above. The switch has the same nil and default arms as `appendBatch`, returning `errUnknownRecord`.

Tests call `streamSize` and assert `len(b) == n && cap(b) == n` (which is also `cap(b) == len(b)`) for an empty Batch, a nested Batch, and a mixed stream. That equality does not catch an off-by-one in the cap comparison: a wrong `total-1` check is still self-consistent with the bytes `appendBatch` writes. The boundary tests below are the ones that catch it.

Empty `Batch{}` encodes as a frame whose payload is `n = 0` and nothing else. Nested empty batches are legal while the innermost sub-stream depth is at most 8.

`Encode` of Put/Delete-only input is byte-identical to a v1 encoding except the first byte is `2`. No padding, no optional fields. Re-encoding a successful decode is canonical for version-2 streams. A version-1 fixture re-encodes as version 2; the bytes match only after the version byte. Tests must not require `Encode(Decode(v1Fixture()))` to equal the fixture.

Child frames are written with the same `appendPut`, `appendDelete`, and `appendBatch` helpers, so a nested Batch is just another frame inside the payload. The outer CRC covers the count and the inner frames as opaque payload bytes. Inner frames have their own CRCs. That matches `mkFrame2(3, batchPL2(...))` in the public tests.

Which size error fires first: the check that fails is the first partial sum, or the first child `frameSize`, that exceeds the cap or the depth limit, in child order. An enclosing frame is strictly larger than any child, so an oversize child always makes the parent oversize too. The child check returns first because it runs before the parent's sum is updated with that child.

### Errors

| Condition | Error |
| --- | --- |
| Missing version, version other than 1 or 2 | `ErrCorrupt` |
| Truncated frame, CRC mismatch, unknown type, oversize `len`, bad inner lengths, Batch payload shorter than 4, `n` that cannot fit in the payload (`n*13 > len(rest)`), count mismatch, trailing bytes after `n` frames, inner `len` past enclosing payload, depth > 8 on decode | `ErrCorrupt` (bare sentinel, nil record interface) |
| `Encode` of nil, pointer, or other type, including a child the write-path switch does not recognize | `errUnknownRecord` (does not wrap `ErrCorrupt`) |
| `Encode` of a frame whose type+payload exceeds 16 MiB, or a stream whose size does not fit in `int` | `errFrameTooLarge` |
| `Encode` of a nest deeper than 8, including the cyclic `Batch` construction above | `errTooDeep` |

No new exported error. The public tests use `errors.Is(err, ErrCorrupt)`.

### Comments to update in `codec.go`

Replace the stale v1 wording when the file is restored:

- Package and `Record` comments: `Put`, `Delete`, and `Batch` are the concrete records. Encode accepts those values only and rejects `*Put`, `*Delete`, and `*Batch`.
- `Encode` comment: writes a version-2 stream. Empty input is still one byte, whose value is 2. An oversize frame, including a Batch whose type+payload exceeds 16 MiB, is rejected before the output buffer is allocated.
- `Decode` comment: accepts version 1 (Put and Delete only) and version 2 (Put, Delete, and Batch). Any other version is `ErrCorrupt` before any frame.
- `maxDepth` comment: top-level is 0; a Batch sub-stream is decoded one level deeper; depth greater than 8 is rejected. An empty Batch still opens a sub-stream.
- `frameSize` comment on the Batch arm: `total` is type+payload, starting at 5; the cap check is `total > maxFrameLen`; equality is legal.
- `ErrCorrupt` comment: also covers a short Batch payload, an `n` the payload cannot hold, a count mismatch, trailing inner bytes, an inner length past the enclosing payload, and a nest deeper than 8.

## How each v2 rule is satisfied

1. **Kind 3, `Batch{Records []Record}`.** New struct and `Kind()` method. `Encode` and `Decode` type-switch on `Batch` values. Pointers stay rejected.

2. **Payload is a framed sub-stream.** `appendBatch` writes `n` then `n` full frames, with the length word backfilled from the bytes written. `parseBatch` reads `n` only after a 4-byte guard, rejects an `n` the remainder cannot hold, and runs `decodeExactly` on that remainder. Inner frames use the same `[len][type][payload][crc]` helpers as the top level.

3. **Exact consumption.** `decodeExactly` parses at most `n` frames. The cursor must then equal `len(rest)`, and `len(recs)` must equal `n`. Trailing bytes fail the cursor check without building the extra frames. A short count fails as truncation or as a length mismatch. Both are `ErrCorrupt`.

4. **Inner `len` past the enclosing payload.** The sub-stream decoder is given only `payload[4:]`. The existing remaining-bytes check rejects a length that extends past that span before any read past it. Sibling and outer bytes are outside the span.

5. **16 MiB cap at every depth.** Decode uses the same `n > maxFrameLen` check on every frame, before slicing the body. Encode's `frameSize` applies `total > maxFrameLen` to every Batch it would emit, at every depth, where `total` is type+payload. A partial sum that exceeds the cap returns before the remaining children are visited and before `make`. A type+payload equal to the cap is emitted and accepted by `Decode`.

6. **Depth cap.** Top-level is depth 0. Each Batch sub-stream is `depth+1`. `depth > 8` is `ErrCorrupt` on decode and `errTooDeep` on encode. The encoder will not emit a 9-deep nest. Eight-deep is accepted, including a hand-built 8-wrap stream that never passed through `Encode`. An empty Batch is subject to the same sub-stream cap.

7. **Version.** `Encode` writes `2`. `Decode` accepts `1` and `2` only, before any frame. Version 1 rejects kind 3. Version 2 accepts kinds 1, 2, and 3.

8. **v1 regression.** v1 decode rules are unchanged and are applied to any stream whose first byte is 1. The fixed fixture in the v2 tests (version byte `0x01`, `Put{"alpha","one"}`, `Delete{"beta"}`) decodes to those two records. CRC flip and truncation of that fixture return `ErrCorrupt`. Regression is not checked by reading `Encode`'s first byte.

9. **Frozen surface.** No rename and no signature change of `Put`, `Delete`, `Record`, `Encode`, `Decode`, or `ErrCorrupt`. The only addition is `Batch`.

10. **No allocation from untrusted `n`.** The count is bounds-checked with `uint64(n)*13 <= len(rest)` and is never a `make` argument. Record slices grow only by `append` of validated frames. A huge `n` with a short payload returns `ErrCorrupt` with `testing.AllocsPerRun == 0` on that call. Error paths return a nil interface.

## Public tests, mapped to the plan

| Test | What it locks | Plan hook |
| --- | --- | --- |
| `TestBatchRoundTrip` | `Batch` of `Put` and `Delete` round-trips as a `Batch` value | `appendBatch` / `parseBatch`, kind 3, version 2 |
| `TestNestedBatchRoundTrip` | Batch-in-Batch, plus a sibling `Delete` inside the outer Batch | recursive encode and decode at depth 2 |
| `TestV1FixtureDecodesUnderV2` | fixed version-1 bytes decode to `Put{"alpha","one"}`, `Delete{"beta"}` | version 1 accepted; Put/Delete parsers unchanged |
| `TestMixedTopLevelRoundTrip` | `Put`, `Delete`, `Batch`, `Put` with nil value, in order | top-level loop still in stream order; nil value still decodes as length 0 |
| `TestTruncatedBatch` | `Encode` then drop the last 2 bytes → `ErrCorrupt` | existing truncation check; last 2 bytes are inside the outer CRC, so the outer frame is short |
| `TestVersion3Rejected` | flip `Encode` output's first byte to 3 → `ErrCorrupt` | version check before frames; also proves `Encode` wrote a version byte at index 0 |
| `TestBatchNestedPastDepth8` | hand-built version-2 stream, a `Put` wrapped in 9 Batches → `ErrCorrupt` | depth 9 sub-stream rejected; encoder is not used to build this input |
| `TestV1BehaviorsViaFixture` | fixture round-trip, last-byte CRC flip, drop last 3 bytes | same v1 checks, against the fixture, not against `Encode` |

`TestTruncatedBatch` cuts two bytes off a complete encoding. Those two bytes are part of the outermost CRC. The failure is an outer truncated frame, which the v1 truncation path already returns as `ErrCorrupt`. The implementation still needs the inner-span rule for hostile payloads the public suite does not build: an inner `len` that claims bytes beyond the Batch payload must fail inside the payload span, not by borrowing the outer CRC or the next top-level frame.

`TestVersion3Rejected` depends on `Encode` succeeding for a single `Put` and on `b[0]` being the version. Writing version 2 satisfies it. The test does not require the rest of the stream to differ from v1.

`recEq2` compares `Batch` by nested record equality, `Put` by key and `string(Value)`, and `Delete` by key. Returning `Batch` values with the same nesting satisfies it. `string(nil)` and `string([]byte{})` are both `""`, so the nil-versus-empty value rule stays compatible with that helper.

## v1 public suite under v2

The v1 file is copied into the worktree beside the v2 file and must pass unchanged.

- Round-trip, duplicates, and nil-versus-empty value do not read the version byte. They pass if `Decode(Encode(recs))` preserves records.
- CRC flip, truncated frame, and unknown type are applied to `Encode` output or to a hand-built version-1 stream (`append([]byte{1}, mkFrame(99, nil)...)`). Version 2 output still has a CRC in the last bytes, so the flip test still hits a CRC. The unknown-type stream is version 1, which the v2 decoder must accept and then reject for tag 99.
- Oversize `len` is a version-1 stream with length `16<<20+1`. The cap check still runs before allocation, for both versions.
- Empty `Encode` is still one byte. The test checks `len(b) == 1`, not `b[0] == 1`. Decoding that byte yields an empty slice.
- `Decode(nil)` and `Decode([]byte{})` still lack a version byte.

## Extra tests to add in implementation

These are not part of the public suite. They belong in a new `codec_test.go` in package `walcodec`, with helper names that do not collide with `beU32`, `mkFrame`, `recEq`, `recsEq`, `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `recEq2`, or `recsEq2`.

- Depth 8 round-trip succeeds, both through `Encode` and through a hand-built version-2 stream that wraps a `Put` in exactly 8 `mkFrame2(3, batchPL2(1, ...))` frames. Depth 9 `Encode` returns `errTooDeep` and does not panic.
- Empty-Batch depth boundary. Seven `mkFrame2` wraps of an empty Batch frame (`batchPL2(0)`), prefixed with version 2, decode successfully: the empty Batch is encountered at depth 7 and its sub-stream is at depth 8. Eight wraps of that same empty frame return `ErrCorrupt` (sub-stream at depth 9). Encode of a chain of 8 empty `Batch` values succeeds. Encode of a chain of 9 empty `Batch` values returns `errTooDeep`.
- Version 0 and version 2 with a valid Put frame: 0 is `ErrCorrupt`; 2 decodes. Version 1 with a valid Batch frame is `ErrCorrupt`.
- Empty `Batch{}` round-trips. `Batch` with a nil `Records` and `Batch` with an empty slice encode to the same bytes.
- Batch payload with `n = 1` but two well-formed inner frames is `ErrCorrupt`. `n = 2` with one inner frame is `ErrCorrupt`. `n = 0` with trailing bytes is `ErrCorrupt`.
- Batch payload shorter than 4 bytes, with a valid outer CRC, is `ErrCorrupt` and does not panic. Cover payload lengths 0, 1, 2, and 3 (`mkFrame2(3, nil)`, one byte, `mkFrame2(3, []byte{0, 0})`, and three bytes), each prefixed with version 2.
- Huge `n` (`0xFFFFFFFF`) with a short CRC-valid Batch payload returns `ErrCorrupt`. `testing.AllocsPerRun` of that `Decode` call is 0. The payload is long enough for a valid CRC and the 4-byte count, and far shorter than `n*13`. Build the input outside the measured function.
- Inner `len` larger than the remaining Batch payload, while the outer frame's CRC and length are consistent, is `ErrCorrupt` and does not consume the next top-level frame. Build this by hand: a Batch frame whose payload length word is small, whose declared inner `len` is larger than that payload, and whose outer CRC matches the short payload; follow it with another top-level frame. `Decode` must fail. A sibling test with a legal inner frame plus a following top-level frame must return both records, proving the outer cursor resumes after the Batch frame.
- Inner oversize `len` (`16<<20+1`) inside a Batch payload is `ErrCorrupt` with no large allocation (`testing.AllocsPerRun` on that path stays 0).
- Nested CRC flip: flip a byte inside an inner frame, recompute nothing, expect `ErrCorrupt` and a nil slice (no partial outer success).
- Put/Delete payload rules still hold inside a Batch: hostile `klen`, trailing payload bytes, empty key, binary key.
- `Encode` rejects `*Batch`, a nil `Record` element inside a Batch, and a custom `Record` implementation. The error wraps `errUnknownRecord`.
- Mixed stream re-encoded after decode is byte-identical (version 2 canonical). The v1 fixture re-encodes with version 2 and the same frames.
- `streamSize` and `Encode` agree. For an empty Batch, a nested Batch, and a mixed stream, `n, err := streamSize(recs)` and `b, err := Encode(recs)` satisfy `len(b) == n && cap(b) == n`.
- Batch cap boundary, top-level, one `Delete` child. Build the records before measuring.
  - Key length `16<<20 - 18`: `Encode` succeeds, `b[0] == 2`, the length word at `b[1:5]` is `maxFrameLen`, `len(b) == maxFrameLen+9`, `cap(b) == len(b)`, and `Decode` returns the same `Batch`.
  - Key length `16<<20 - 17`: `Encode` returns a nil buffer and `errFrameTooLarge`. `errors.Is(err, ErrCorrupt)` is false. `testing.AllocsPerRun` of `Encode` alone is 0, which places the rejection in `frameSize` before `make`.
- The same pair one level nested. `inner` is the Batch from the top-level pair. Call `frameSize(inner, 1)`.
  - Key length `16<<20 - 18`: `frameSize` returns `(maxFrameLen, nil)`.
  - Key length `16<<20 - 17`: `frameSize` returns `errFrameTooLarge`.
  - `Encode` of `Batch{Records: []Record{inner}}` for either key length returns `errFrameTooLarge`, a nil buffer, and `AllocsPerRun == 0`. The legal inner frame is `maxFrameLen`; the outer type+payload is `maxFrameLen+13`, so the outer frame cannot be emitted. The illegal inner frame fails in the child `frameSize` call, before the outer sum is updated.
- Cyclic Batch, this construction and no other:

```go
recs := make([]Record, 1)
b := Batch{Records: recs}
recs[0] = b
_, err := Encode([]Record{b})
```

  `errors.Is(err, errTooDeep)` is true. The walk returns after 9 calls, before a size cap is hit. The test does not build the cycle by `b := Batch{}; b.Records = []Record{b}`: that stores a copy whose `Records` is nil and is a legal depth-1 nest.
- `go test` race is unnecessary; the codec has no package-level mutable state after `crc32` init. `go build`, `go vet`, and `GOARCH=386 go test` (or `go test` compile) must pass.

## Files

- Restore `snapshot-v1/codec.go` into `worktree/codec.go`, then edit that file only for the codec change.
- Do not edit `walcodec_public_test.go` or `walcodec_v2_public_test.go`.
- Add `worktree/codec_test.go` for the extra cases above.
- Leave `go.mod` as `module walcodec` / `go 1.25`.

## Implementation order

1. Restore `snapshot-v1/codec.go` to `worktree/codec.go`.
2. Add `Batch`, `kindBatch = 3`, `versionV2 = 2`, `maxDepth = 8`, `minInnerFrame = 13`, `errTooDeep`, and `decoder`. Update the stale v1 comments, including the `frameSize` comment that `total` is type+payload.
3. Generalize `frameSize` to take a depth, handle `Batch`, and return as soon as `total > maxFrameLen` or depth exceeds 8. Extract `streamSize`. Change the version byte written by `Encode` from 1 to 2. `Encode` allocates only after `streamSize` returns.
4. Implement `appendBatch` with reserve-and-backfill and a nil/default arm that returns `errUnknownRecord`. Do not recompute `frameSize` on the write path. Keep `frameLen > maxFrameLen` as the drift guard.
5. Generalize `Decode` to accept versions 1 and 2. Replace the frame walk with `nextFrame`, `decodeUntilEnd`, and `decodeExactly`. `parseBatch` guards `len < 4`, rejects `uint64(n)*13 > len(rest)`, and never sizes a slice from `n`. Error returns are a nil interface.
6. Run `go test ./...`, `go vet`, and a 386 compile. Then add `codec_test.go` and run the tests again, including the cap-boundary allocation measurement and the cyclic `errTooDeep` assertion.

## Out of scope

- No streaming `io.Reader` API, no length-prefix change, no new exported error, no compression, no encryption.
- No change to Put or Delete payload layout.
- No attempt to make `Encode` keep writing version 1 for Put/Delete-only input. The amendment requires version 2.
- No skip of enclosing CRCs to avoid the 9× check at depth 8.
- No `limit int` sentinel on the frame walker.
