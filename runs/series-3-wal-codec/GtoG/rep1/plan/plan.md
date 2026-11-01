# Plan: `walcodec` v2 — nested `Batch` records

Amendment: `amendment.md`. v1 source of truth: `snapshot-v1/codec.go` (package `walcodec`). The worktree currently has `go.mod` and the two public test files and no codec; implementation starts by placing that file into the worktree and extending it. This revision answers `plan/plan_findings.md`. The point-by-point response is `plan/plan_rebuttal.md`.

v1 rules stay in force except where the amendment changes them. The frozen exported names and signatures are `Record`, `Put`, `Delete`, `Encode`, `Decode`, and `ErrCorrupt`. The only new exported type is `Batch`.

## What stays

- Package name `walcodec`, module `walcodec`, Go 1.25. `go.mod` does not set `GOARCH`. Every length that crosses a `uint32` boundary must be correct on both 32-bit and 64-bit `int`.
- `Record` interface: `Kind() uint8`.
- `Put` and `Delete` field sets, tags, and payload layouts:
  - `Put{Key string; Value []byte}`, `Kind() == 1`. Payload `[klen uint32][key][vlen uint32][value]`.
  - `Delete{Key string}`, `Kind() == 2`. Payload `[klen uint32][key]`.
  - A `nil` `Value` and an empty `Value` both encode as `vlen = 0` and both decode to a length-0 slice. They compare equal. Decode may return a nil slice or a non-nil empty slice; either satisfies v1 public `recEq` (`bytes.Equal`) and v2 public `recEq2` (string compare of the bytes).
- Frame shape, unchanged at every depth: `[len uint32][type uint8][payload][crc uint32]`, all integers big-endian.
  - `len` counts `type`+`payload` only. `crc` is outside `len`.
  - `crc` is CRC32-IEEE (`hash/crc32.ChecksumIEEE`) over `type`+`payload`.
  - Cap is `16 << 20` (`16777216`). A larger `len` is `ErrCorrupt`, checked before any allocation of that frame's body.
  - `len < 1` is `ErrCorrupt` (a frame must contain a type byte).
- `ErrCorrupt` text stays `walcodec: corrupt stream`. Callers use `errors.Is`. Encode does not return `ErrCorrupt`.
- Top-level stream is still `[version uint8][frame*]`.
- Empty `recs` (nil or empty slice) encodes to a one-byte version-only stream and decodes to an empty slice with no error.
- `Decode(nil)` and `Decode([]byte{})` stay `ErrCorrupt` (missing version byte). Version is still checked before any frame.
- Duplicate keys stay, in order. The codec remains a log.
- Truncation, CRC mismatch, unknown type, and payload that does not consume its frame exactly stay `ErrCorrupt`.
- No new exported functions, constants, or errors. Helpers stay unexported.

## What changes

All of this lives in the existing codec file (or a sibling file in the same package if the file grows). No new package.

### New type

```go
type Batch struct { Records []Record } // Kind 3
func (Batch) Kind() uint8 { return 3 }
```

`Batch` is a struct value, like `Put` and `Delete`. A nil `Records` and an empty `Records` both encode as inner count `0`. Decode of `n = 0` may return a nil `Records` or a non-nil length-0 `Records`. Both compare equal under the public helpers (`len` plus element equality). An equality helper that uses `==` on the slice header is not part of the contract and must not be added.

### Concrete types the encoder accepts

The single encode type switch accepts both the struct value and the pointer for each known type. A non-nil pointer is dereferenced once into a local struct. Both the `Kind()` check and the payload encoder see that dereferenced struct, not the pointer variable:

- `Put` and `*Put` → value `Put`
- `Delete` and `*Delete` → value `Delete`
- `Batch` and `*Batch` → value `Batch`

`Kind()` has a value receiver, so a non-nil `*Put` promotes and returns 1 today. Do not call `Kind()` on the pointer variable and then encode a separately copied value. Call `Kind()` on the dereferenced struct, then encode that same struct. A later edit that type-asserts the pointer and calls a different method set cannot drift from the payload it writes.

A nil pointer is an encode error (`errBadRecord`). Nested `Records` elements use the same switch, so a `*Put` or `*Batch` stored inside a `Batch` encodes. There is no second switch that accepts only values.

A method set that makes `Kind()` disagree with the concrete type is `errBadRecord`. An unknown concrete type, including a type that only implements `Record` and is not one of the six forms above, is `errBadRecord`.

Decode always returns the struct values `Put`, `Delete`, and `Batch`, never pointers.

### Encode errors (unexported, distinct)

Three unexported sentinels, none of them `ErrCorrupt`. Callers of `Encode` still see a non-nil error and a nil byte slice. Text is specific so a string match does not collapse the three failures:

- `errBadRecord` — unknown concrete type, nil pointer, `Kind()` disagrees with the concrete type, or a length that does not fit in `uint32` (Batch count, key length, value length). Text: `walcodec: record is not Put, Delete, or Batch`.
- `errDepth` — entering a sub-stream at depth greater than 8. Text: `walcodec: batch nesting exceeds 8`.
- `errOversize` — a frame body would exceed 16 MiB, including a Batch payload assembled from children. Text: `walcodec: frame exceeds 16MiB`.

`Encode` returns `(nil, err)` on any of these. It never returns a partial buffer.

### Version

- Add `versionV2 = 2` and `kindBatch = 3`. Keep `versionV1 = 1`.
- `Encode` writes `version = 2` for every successful stream, including Put/Delete-only and empty input. That is the allowed v1 behavior change. v1 public tests do not assert the version byte is `1`.
- `Decode` accepts version `1` and version `2` only. Any other byte, including `0` and `3`, is `ErrCorrupt` before any frame is read. Test 6 flips `b[0]` to `3` on an otherwise valid encode and requires `ErrCorrupt`.
- The version selects the allowed type set for **every** frame at **every** depth, not only depth 0.
  - Version `1`: types `1` and `2` only. Type `3` anywhere, including a hand-built version-1 stream that contains a Batch, is `ErrCorrupt` (unknown type). The decoder does not recurse into that payload and therefore cannot accept types `1` or `2` "inside" a version-1 Batch.
  - Version `2`: types `1`, `2`, and `3` at every depth up to the cap.

### Batch payload

```
Batch payload = n(uint32) frame_1 ... frame_n
```

Each inner frame is a full `[len][type][payload][crc]` frame, same rules as a top-level frame, including the 16 MiB `len` cap.

- `n` is the count of inner records. It is a `uint32` on the wire and a `uint64` in the walker. It is never narrowed to `int`.
- The `n` frames must consume the Batch payload exactly. Too few frames, too many frames, or trailing bytes is `ErrCorrupt`.
- An inner `len` that would read past the enclosing payload is `ErrCorrupt` (truncated). The decoder never reads a sibling frame or an outer frame to satisfy an inner `len`.
- A truncated count (payload shorter than 4 bytes) is `ErrCorrupt`.
- Inner types may be `Put`, `Delete`, or `Batch`, subject to the version's type set.
- An empty `Batch` is legal: payload is four zero bytes (`n = 0`) and nothing follows.

### Depth

Depth is an integer carried by both encode and decode.

- The top-level stream is depth 0. It is not a Batch sub-stream, so the cap is not applied to it.
- A `Batch` encountered at depth `d` has its inner sub-stream decoded and encoded at depth `d+1`.
- A sub-stream at depth greater than 8 is `ErrCorrupt` on decode, and is refused on encode (`errDepth`). The encoder does not emit a stream it would reject.
- Equivalently: 8 nested `Batch` levels are legal; a 9th is not.

Public test 7 builds a Put wrapped in 9 Batch frames under version 2 and requires `ErrCorrupt`. Under this convention that stream is:

| wrap | frame type | depth of that frame | depth of its inner sub-stream |
| --- | --- | --- | --- |
| outermost (top-level) | Batch | 0 | 1 |
| 2nd | Batch | 1 | 2 |
| ... | ... | ... | ... |
| 9th | Batch | 8 | 9 |
| innermost | Put | 9 | — |

The 9th Batch sits at depth 8, which is still a legal frame depth, but opening it would decode a sub-stream at depth 9. That is rejected. Eight wraps of the same Put would put the innermost sub-stream at depth 8 and must succeed.

Decode order for a Batch frame is fixed. Do not put the depth check in a place that can run after an inner CRC, and do not let a depth failure skip the outer CRC:

1. Outer frame CRC is checked. A mismatch is `ErrCorrupt`. The payload is not parsed, so the depth check does not run.
2. Depth is checked on entry to the sub-stream, before the inner count is read and before any inner frame is parsed. `d+1 > 8` is `ErrCorrupt` for depth. The inner body is not parsed. A depth-9 Batch whose inner payload is truncated, has a bad inner CRC, or is otherwise malformed is still `ErrCorrupt` for depth. The inner CRC is not consulted.
3. The inner count is read.
4. Inner frames are parsed, each with the same order: that frame's CRC, then its depth (if it is a Batch), then its payload.

On encode, the depth check runs at the start of encoding a `Batch`, before any child is encoded and before the outer frame is appended. If any descendant fails (depth, `errBadRecord`, `errOversize`, or a length that does not fit in `uint32`), that error returns immediately. No outer frame is appended for a Batch that contains a failing descendant, including a Batch nested inside another Batch. `Encode` discards its destination on any error and returns `(nil, err)`.

### Shared frame walker

The current `Decode` loop and `appendFrame` stay the framing primitives. Decode is one function that reads a bounded number of frames from a byte slice and returns the unconsumed remainder. Both callers use that one function. There is no second loop that assumes a single record.

```
const drain uint64 = ^uint64(0) // caller asks the walker to read until the slice is empty

decodeFrames(b []byte, version uint8, depth int, n uint64) (recs []Record, rest []byte, err error)
```

`n` is the number of frames to read. It is a `uint64`. It is never an `int`, and it is never produced by `int(n)` from a `uint32` count.

- The function reads exactly `n` well-formed frames from the front of `b` when `n != drain`, or returns `ErrCorrupt`.
- When `n == drain`, it reads until `b` is empty. A top-level stream has no count word, so "extra frames" are additional records, which is legal.
- It returns the unconsumed suffix as `rest`. It does not drain `b` unless the caller asked for `drain`.
- It never reads past `len(b)`. An inner `len` that does not fit in the remaining suffix is `ErrCorrupt`.
- The loop counter is a `uint64`. The condition is `parsed < n` (or `len(rest) > 0` when draining). It does not compare against an `int` conversion of the wire count.

Two callers, one remainder rule:

- Top-level `Decode` checks the version byte, then calls `decodeFrames(rest, version, 0, drain)` **once**. The walker drains the slice. The caller requires `len(rest) == 0`, which holds because `drain` reads until empty. This is the only caller that passes `drain`.
- A Batch payload does **not** pass `drain`. After reading the `uint32` count, it calls `decodeFrames(payload[4:], version, depth+1, uint64(n))` **once**. Then:
  - error from the walker → `ErrCorrupt`;
  - `len(rest) != 0` → `ErrCorrupt` (more than `n` frames, or trailing bytes);
  - `rest` empty → the `n` records are the Batch body.

So "n says 1, payload holds two well-formed frames" is `ErrCorrupt` because the walker stops after one frame and the Batch caller rejects the leftover suffix. A walker that always drains the slice is not the contract. Both callers share `decodeFrames` and the remainder rule. A later edit cannot desync a per-frame append loop from the Batch caller, because there is no per-frame append loop.

`decodeFrames` rejects, in this order, for each frame it reads:

1. Remaining slice shorter than 4 bytes (truncated length word) → `ErrCorrupt`.
2. `len > 16 MiB` or `len < 1`, from the `uint32`, before any body allocation → `ErrCorrupt`.
3. `uint64(frameLen)+uint64(4)` extends past the remaining slice → `ErrCorrupt`. Do not add the `uint32` length to `4` in `uint32` and then widen.
4. CRC mismatch over `type`+`payload` → `ErrCorrupt`. The payload is not parsed. Depth is not consulted.
5. Type byte not in the version's allowed set → `ErrCorrupt`. The payload is not parsed. Version 1 never enters a Batch body.
6. Payload does not parse exactly. For a Batch, the first step inside this parse is the depth check (see "Depth"), then the inner count, then the inner frames. A depth failure returns before the inner count is read.

Inner frames use this same order. CRC of an inner frame is checked before that frame's payload is parsed. Depth of a Batch is checked after that Batch frame's own CRC and before its inner count. Depth is not a substitute for CRC, and an inner CRC is not a substitute for depth.

Oversize `len` is rejected from the `uint32` value before `make` of the body. The existing slice-window approach already avoids allocating the body; keep that. Do not `make([]byte, n)` for an `n` above the cap. Do not allocate a record slice of length `int(n)` from the wire count. Append records as they are parsed, or size the slice from a count only after it is known to fit in `int` and in the remaining bytes. A count in `2^31..2^32-1` on a 32-bit `int` must still be walked by comparing `uint64` values, not by `make([]Record, int(n))`.

Encode mirrors the bounded walk with an inner buffer so a failure cannot append a partial outer frame:

```
encodeRecords(recs []Record, depth int) (payload []byte, err error)
appendRecord(rec Record, depth int) ([]byte, error)
```

- `Encode` allocates a destination that starts as `[]byte{2}`. For each top-level record it calls `appendRecord` at depth 0. On any error it returns `(nil, err)` and drops `dst`. On success it returns `dst`.
- `Put` and `Delete` ignore depth (they are leaves). Their payload layout is unchanged. Their length prefixes are range-checked before the `uint32` conversion (see "Encode lengths must fit in `uint32`").
- `Batch` at depth `d`:
  1. If `d+1 > 8`, return `errDepth` immediately. Do not encode children. Do not append a frame.
  2. If `len(Records) > math.MaxUint32`, return `errBadRecord`. Do not convert the length.
  3. Encode each child with `appendRecord` at depth `d+1` into a fresh inner buffer. A child error returns that error at once. The inner buffer is discarded. The outer destination is not modified.
  4. Prefix `n = uint32(len(Records))` onto the inner buffer to form the Batch payload. The conversion is safe because step 2 passed.
  5. Call `appendFrame` with type `3` only after steps 1–4 succeed. `appendFrame` returns `errOversize` if the body would exceed 16 MiB, before writing the length word, using the comparison in "Frame body length must not wrap". On that error the caller returns `(nil, errOversize)` and does not keep the in-progress destination.
- One type switch owns both the tag and the payload, including the pointer forms listed above. The switch dereferences first. `Kind()` is checked on the dereferenced struct before the tag is written.
- Children are encoded first; the outer frame is written only if the assembled payload fits and no descendant failed. No partial Batch frame is appended, at any nesting level.

### Batch count is `uint64` through the walker

`int(n)` truncates when `n > math.MaxInt32` and `int` is 32 bits. The module does not set `GOARCH`, so that platform is in scope. A truncated count makes the walker stop early. The leftover-suffix check then reports `ErrCorrupt` for a payload the amendment defines as exactly `n` frames.

The encode guard `len(Records) > math.MaxUint32` does not close this. A count in `2^31..2^32-1` fits in `uint32`, encodes, and must decode.

Contract:

- Read `n` with `binary.BigEndian.Uint32`. Widen with `uint64(n)`. Do not write `int(n)`.
- Pass that `uint64` into `decodeFrames`.
- Compare the number of frames parsed to that `uint64`.
- A stream whose Batch count is in `2^31..2^32-1`, and whose payload holds exactly that many well-formed frames, decodes. It is not `ErrCorrupt` merely because `int` cannot hold the count.

### Encode lengths must fit in `uint32`

Three prefixes wrap if they are converted with a bare `uint32(len(...))`. A wrapped prefix emits a frame whose declared length is shorter than the bytes that follow. Decode then fails exact consumption (`ErrCorrupt`) on a stream the encoder produced. That breaks "the encoder will not emit a stream it would itself reject". The same wrap sits on every nested frame, not only at depth 0.

Before the conversion, refuse each of these with an encode error and no bytes:

- `len(Records) > math.MaxUint32` → `errBadRecord`. Checked on every Batch, including one nested inside another.
- `len(key) > math.MaxUint32` → `errBadRecord`. Checked on every `Put` and every `Delete`, including one nested inside a Batch.
- `len(value) > math.MaxUint32` → `errBadRecord`. Checked on every `Put`, including one nested inside a Batch.

Do not write the length word, and do not append the key or value bytes, when the check fails. A child failure returns immediately. The outer Batch frame is not appended.

### Frame body length must not wrap

`appendFrame` must refuse a body over 16 MiB before it writes the length word. The body length is `1 + len(payload)` in the abstract. That sum must not be computed as an `int` add.

`1+len(payload)` wraps when `len(payload) == math.MaxInt`. The wrapped sum is negative or small, the cap check passes, and `uint32(bodyLen)` then wraps the length word the same way a bare `uint32` count used to. A Batch whose children assemble a payload near the `int` limit can reach this. The 16 MiB check is supposed to refuse it.

Use one of these comparisons, and no other:

- `len(payload) > int(maxFrameLen)-1`, which is the v1 form. `maxFrameLen` is `16 << 20`, so `int(maxFrameLen)-1` does not overflow `int` on 32-bit or 64-bit.
- or `uint64(len(payload)) > uint64(maxFrameLen)-1`.

Do not write `1+len(payload)` and then compare. Do not write `uint32(1+len(payload))`. After the comparison succeeds, the length word is `uint32(len(payload))+1`, which is safe because `len(payload)` is at most `16<<20 - 1`.

### Bounds that must not wrap

Every length check uses `uint64` (or an equivalent that cannot wrap) before comparing to the remaining slice. The order is fixed. A recursive Batch payload uses the same three checks at every depth.

1. **Frame, after the 4-byte length word is consumed.** Require `len(rest) >= 4` first (truncated header). Then reject `frameLen > maxFrameLen` or `frameLen < 1`. Then require `uint64(frameLen)+uint64(4) <= uint64(len(rest))`. Do not write `uint32(frameLen)+4`.
2. **Key, after the 4-byte `klen` word is consumed.** Require `len(payload) >= 4` first. Then require `uint64(klen) <= uint64(len(payload))-4`, which is the same test as `uint64(4)+uint64(klen) <= uint64(len(payload))`. Do not write `4+klen` in `uint32`. The v1 form `uint64(len(payload)-4)` is safe only because `len(payload) >= 4` was checked first; keep that order. Do not subtract before the check.
3. **Value, after the key is sliced off.** Require `len(tail) >= 4` first. Then require `uint64(vlen) == uint64(len(tail))-4` (exact consumption). Same order: the `>= 4` check precedes the subtraction. Do not write `uint64(len(tail)-4)` unless `len(tail) >= 4` is already true. Do not add `vlen` to `4` in `uint32`.

Batch count is a fourth check, same style: require `len(payload) >= 4` before reading `n`. Then the walker bounds each inner frame by the payload remainder (`uint64(frameLen)+4`), never by the outer stream. `n` is not given a separate maximum other than the payload cap, the exact-consumption rule, the encode-side `MaxUint32` guard, and the rule that the walker count stays a `uint64`.

### Decode of Put and Delete

Keep the v1 exact-consumption rules inside a Batch as well as at the top level:

- Delete: key bytes consume the payload; any trailing byte is `ErrCorrupt`.
- Put: value bytes consume the tail after the key; a short `vlen` or extra trailing bytes is `ErrCorrupt`.
- Copy value bytes out of the frame (`append([]byte(nil), ...)`) when `vlen > 0`, so the caller does not alias the input buffer. A zero `vlen` yields a length-0 slice (nil or non-nil empty).

### Empty Batch vs missing count

| input | result |
| --- | --- |
| `Batch{Records: nil}` or `Batch{Records: []Record{}}` | payload `00 00 00 00`, decodes to `Batch` with length-0 `Records` (nil or non-nil empty; both legal) |
| Batch payload shorter than 4 bytes | `ErrCorrupt` |
| `n = 1` but payload ends after the count | `ErrCorrupt` |
| `n = 0` with leftover bytes after the count | `ErrCorrupt` (walker is asked for 0 frames; caller rejects non-empty `rest`) |
| `n = 2` with one well-formed frame and no more bytes | `ErrCorrupt` |
| one well-formed frame plus a second well-formed frame or trailing junk, with `n = 1` | `ErrCorrupt` (walker returns the leftover; caller rejects it) |
| `len(Records) > math.MaxUint32` on encode | encode error, no bytes |
| `len(key) > math.MaxUint32` or `len(value) > math.MaxUint32` on encode | encode error, no bytes |
| wire count in `2^31..2^32-1` with exactly that many frames | decodes; the walker count is `uint64`, not `int` |

## How each v2 rule is satisfied

1. **Kind 3, exported `Batch`.** New struct with `Records []Record` and `Kind() == 3`. No other exported addition. Existing signatures unchanged.
2. **Payload is a framed sub-stream.** `n uint32` then `n` full frames. Encode writes that layout. Decode parses it only inside the payload slice, via one `decodeFrames(..., uint64(n))` call.
3. **Inner records may be Put, Delete, or Batch.** The shared walker accepts all three when version is 2, at every depth, and recurses for Batch. Version 1 accepts only types 1 and 2 at every depth and does not recurse on type 3.
4. **16 MiB cap at every depth.** The same `maxFrameLen` check runs on every frame the walker reads, before allocation. Encode refuses an oversize body at every depth via `appendFrame` (`errOversize`), using `len(payload) > int(maxFrameLen)-1` or a `uint64` compare. It does not add `1` in `int` before the check.
5. **Inner `len` past the enclosing payload is truncated `ErrCorrupt`.** The walker is given only the payload bytes after the count. A `len` that does not fit in that remainder fails the truncated-frame check (`uint64(frameLen)+4`) and never advances into the parent remainder.
6. **Exact consumption.** `decodeFrames` reads exactly `n` frames and returns the remainder. The Batch caller requires `len(rest) == 0`. Fewer frames (walker error), more frames (non-empty rest), or trailing bytes (non-empty rest) are `ErrCorrupt`. The top-level stream is the only caller that passes `drain`. Both callers share the one walker and the remainder rule. The count is a `uint64`, so a count above `MaxInt32` is not truncated before the loop.
7. **Depth cap 8.** Top-level is depth 0. Each Batch adds one. Entering depth 9 is `ErrCorrupt` on decode and `errDepth` on encode, before children are written and before the outer frame is appended. Decode order is outer CRC, then depth, then inner count, then inner frames. A depth-9 Batch with a bad inner CRC is `ErrCorrupt` for depth; the inner body is not parsed. A bad outer CRC is `ErrCorrupt` for CRC; the depth check does not run. A failing descendant aborts the whole `Encode` with no bytes. Eight levels round-trip; nine do not. This matches test 7 (nine wraps).
8. **Encode writes version 2.** First byte is `2` for every successful `Encode`, including empty and Put/Delete-only input.
9. **Decode accepts 1 and 2 only.** Version checked first. `0` and `3` (and any other byte) are `ErrCorrupt`. The version's type set applies at every depth.
10. **v1 decode regression on a fixed fixture, not on Encode's first byte.** `Decode` of the literal `v1Fixture` bytes (`version = 1`, Put `alpha`/`one`, Delete `beta`) returns those records. CRC flip of the last byte and truncation of the last three bytes of that fixture are `ErrCorrupt`. The v1 public suite still runs unchanged against this decoder: version-1 streams it builds by hand (unknown type, oversize `len`) still decode, and streams this encoder writes (version 2, Put/Delete only) still round-trip because the decoder accepts version 2.
11. **Encoder will not emit a stream it would reject.** Depth is enforced on encode before any frame of that Batch is appended. A `Records` length, key length, or value length above `math.MaxUint32` is refused before the `uint32` conversion. The frame body cap is checked without an `int` add that can wrap. CRC, length, and type are produced by the same helpers the decoder checks. An input that cannot be represented (unknown type, nil pointer, `Kind()` mismatch, payload over 16 MiB, depth over 8, count or string length over `MaxUint32`) returns an encode error and no bytes. A child failure inside a nested Batch does not leave an outer frame in the returned buffer, because `Encode` returns `nil` for the buffer on any error and the outer `appendFrame` runs only after children succeed.

## Public tests, mapped to the plan

Shipped suite: `instrument/walcodec_v2_public_test.go`, already copied into the worktree. It runs beside the v1 suite in package `walcodec`.

| test | what the plan must make true |
| --- | --- |
| `TestBatchRoundTrip` | Encode/Decode of one Batch holding a Put and a Delete preserves order, keys, and values. |
| `TestNestedBatchRoundTrip` | A Batch whose first child is a Batch round-trips. Depth 2 is legal. |
| `TestV1FixtureDecodesUnderV2` | `Decode(v1Fixture())` equals Put `alpha`/`one` then Delete `beta`. Fixture is a literal; do not generate it with `Encode`. |
| `TestMixedTopLevelRoundTrip` | Top-level Put, Delete, Batch, Put (nil value) round-trips. Nil value decodes as length 0. |
| `TestTruncatedBatch` | `Decode` of an encoded Batch with the last 2 bytes removed is `ErrCorrupt`. |
| `TestVersion3Rejected` | Encoded stream with byte 0 set to 3 is `ErrCorrupt`. |
| `TestBatchNestedPastDepth8` | Hand-built version-2 stream of 9 nested Batch frames around a Put is `ErrCorrupt`. The encoder is not used to build this stream. Depth is rejected after the outer CRC and before the inner body is parsed. |
| `TestV1BehaviorsViaFixture` | Same fixture round-trips; last-byte CRC flip is `ErrCorrupt`; drop of the last 3 bytes is `ErrCorrupt`. |

v1 public tests stay as the regression suite. They must still pass after the version bump because none of them require the written version byte to be 1, and the hand-built version-1 streams (unknown type 99, oversize `len`) are still accepted as version 1 and rejected for the original reasons.

## Implementation order

1. Copy `snapshot-v1/codec.go` into the worktree as the v1 baseline. Confirm `go test` of the v1 public file passes before the v2 edits.
2. Add `Batch`, `kindBatch`, `versionV2`, `errDepth`, and `errOversize`. Point `Encode` at version 2. Widen `errBadRecord` text. Accept value and pointer forms in the one type switch. Dereference first, then check `Kind()` and encode that struct.
3. Replace the inline decode loop with `decodeFrames(b, version, depth, n uint64)` that returns the remainder. Top-level `Decode` only validates the version byte, then calls the walker once with `drain`.
4. Encode and decode Batch payloads through that walker. Pass `uint64(n)` into `decodeFrames`. Do not narrow `n` to `int`. Reject a non-empty remainder. Check depth after the outer CRC and before the inner count. On encode, check depth and `len(Records) <= math.MaxUint32` before children, and call `appendFrame` only after the inner buffer is complete.
5. Apply the version's type set at every depth inside `decodeFrames`, after CRC and before payload parse.
6. Fix the three `uint64` bounds (frame, key, value) in the order stated above, at every depth.
7. Range-check key length and value length before `uint32` conversion, at every depth. Change `appendFrame` to compare `len(payload) > int(maxFrameLen)-1` (or `uint64`) and do not add `1` in `int` before the check.
8. Run `go test ./...` and `go vet ./...` in the worktree. Both public files must pass. Do not add tests unless a rule above is not already covered and a private check is needed during implementation; the acceptance bar is the shipped suites.

## Out of scope

- No change to `Put`, `Delete`, `Record`, `Encode`, `Decode`, or `ErrCorrupt` signatures.
- No new version numbers, no optional fields, no compression, no length-prefix on the outer stream.
- No exported pointer API. Pointers are accepted on encode only as ordinary `Record` values already allowed by the interface.
- No rewriting of the public test files.
- Encode errors stay distinct from `ErrCorrupt`, and the three encode failures stay distinct from each other.
- No `GOARCH` pin. The walker count stays `uint64` so 32-bit `int` is not a special case that truncates a legal count.
