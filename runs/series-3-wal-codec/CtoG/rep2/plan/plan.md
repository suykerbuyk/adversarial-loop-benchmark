# Plan: `walcodec` v2: nested `Batch` records

Status: phase 2, round 3. This revision answers `plan/plan_findings.md` (round 2). The point-by-point
response is in `plan/plan_rebuttal.md`. Changes from round 2, in short:
- **All pseudocode now shows every error return explicitly** (round 2 elided them in the decoder sketch,
  which the adversary correctly read as a missing `return`). In every loop, at every depth, an error from
  `nextFrame`, `decodeRecord` or `appendRecord` returns `nil, err` **before** anything is appended or
  any offset/buffer is advanced. This matches v1 `Decode`.
- Encoder loops no longer overwrite `out` with a child's return value before checking the error. The
  child result goes into a temporary (`grown`). `out` is replaced only after `err == nil`, and the cap
  check reads the length only after that. This works whether or not a child returns a nil buffer on error.
- The plan now states that both decoder `make` calls return a non-nil slice even at capacity 0, that
  `min` is the Go ≥1.21 builtin (go.mod says `go 1.25`; v1 already uses it), and §5 asserts `!= nil` on
  the empty top-level result and on an empty Batch's `Records`.
- The allocation test has separate ceilings for separate inputs. Each ceiling is derived from the input
  size, and a padded case checks that the 1024-entry top-level hint is a hard cap.
- `minPayload` is removed. `minFrameWire` is defined directly as `lenSize + typeSize + u32Size + crcSize`
  and documented as a lower bound on wire size for the count guard and capacity heuristic only. It is not
  a per-kind payload floor.

## 0. Starting point (read this first)

- The **worktree has no v1 source.** It holds only `go.mod`, `walcodec_public_test.go` (v1 suite) and
  `walcodec_v2_public_test.go` (v2 suite, byte-identical to `instrument/`). The converged v1 code is in
  `snapshot-v1/` (`walcodec.go`, `encode.go`, `decode.go`; its `go.mod` is identical). **Step 1 of the
  implementation is to copy those three files into `worktree/` unchanged.** Every later change is a diff
  against that baseline. The v1 extra/fuzz tests mentioned in the last v1 review are not in the snapshot.
  §5 replaces them.
- Right now the worktree does not compile, because `Batch` and the v1 code are missing. After step 1 only
  the v2 suite fails to compile. After the changes below, both suites must pass together in one package.
- I checked the v1 public suite: it never asserts the version byte that `Encode` writes. Its hand-built
  streams use version 1, which v2 still accepts. So it stays green when `Encode` switches to version 2.

## 1. What stays (v1 behaviour kept as is)

- Exported surface: `Record`, `Put`, `Delete`, `Encode`, `Decode`, `ErrCorrupt` keep their names and
  signatures. **The only new exported identifier is `Batch`.** There is no new exported error, constant or
  option. Encoder errors stay unexported `fmt.Errorf` values, as in v1.
- Frame shape `len(4) type(1) payload(len-1) crc(4)`, big-endian. `len` counts type+payload. CRC32-IEEE
  covers type+payload and sits outside `len`. 16 MiB cap on `len`.
- Put/Delete payload encoding and decoding: `reader`, `field`, nil/empty `Value` → length-0 non-nil slice,
  returned records never alias the input. Strictness stays: trailing bytes in a payload or a zero `len`
  are `ErrCorrupt`.
- Per-frame check order: header present → `len` ≤ cap (before any body read or allocation) → frame
  complete → `len` ≠ 0 → crc → type tag → payload.
- Every `Decode` failure wraps `ErrCorrupt` and returns a nil slice, **at every depth**: the first error
  stops decoding immediately. On success the slice is non-nil.
- `Decode(nil)` and `Decode([]byte{})` → `ErrCorrupt`.
- Encode input handling: `Put`, `*Put`, `Delete`, `*Delete`. A nil interface or nil pointer is an error.
  Each record is read exactly once. On error the result is `(nil, err)` and the error does not wrap
  `ErrCorrupt`.

## 2. What changes

| File | Change |
|---|---|
| `walcodec.go` | Add `type Batch struct{ Records []Record }` with `func (Batch) Kind() uint8 { return kindBatch }`. The value receiver means `Batch` and `*Batch` both satisfy `Record`. Add constants `version2 byte = 2`, `kindBatch uint8 = 3`, `maxDepth = 8` (deepest legal sub-stream), and `minFrameWire = lenSize + typeSize + u32Size + crcSize` (= 13). Its comment says: "lower bound on the wire size of any frame, because every payload kind starts with a u32 (klen or n). Used only to bound record counts and capacity hints. It is not a payload-validity floor; payload validity is checked per kind by the parsers." Rewrite the `version1` comment as "accepted for decode, never written". Update the package doc with the Batch payload grammar, the version rule and the depth convention. Remove `frameWireSize`: the v1 review flagged it as unused. Keep `payloadLen`/`frameLenField` for Put/Delete. |
| `encode.go` | Write `version2`. Replace the flat loop with recursive `appendRecord(out, r, depth)`. Add `appendBatch`. Fold the `fields` type switch into `appendRecord` and add `Batch` and `*Batch` (nil `*Batch` → error). Update the doc comment. |
| `decode.go` | Accept version 1 or 2. Move frame parsing into a shared `nextFrame` used at every depth. Add `decodeBatch`. Allow kind 3 only in version 2. Thread `depth` through. Update the doc comment. |
| `walcodec_v2_extra_test.go` (new) | Extra adversarial tests and fuzz targets (§5). |

### 2.1 Encoder design: single pass, `len`/`crc` written in afterwards

The pseudocode below is complete in its control flow. Every error return is written out.

```go
func Encode(recs []Record) ([]byte, error) {
    out := make([]byte, 1, 1+min(len(recs), 1<<16)*32)
    out[0] = version2
    for i, r := range recs {
        grown, err := appendRecord(out, r, 0)        // 0 = depth of the top-level stream
        if err != nil {
            return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
        }
        out = grown
    }
    return out, nil
}

// appendRecord appends r's full frame to out. depth is the depth of the stream r sits in (top level = 0).
// On error, callers discard the returned buffer and use only err.
func appendRecord(out []byte, r Record, depth int) ([]byte, error) {
    switch x := r.(type) {
    case Put:     return appendLeaf(out, kindPut, x.Key, x.Value)
    case *Put:    if x == nil { return nil, errors.New("nil *Put") };    return appendLeaf(out, kindPut, x.Key, x.Value)
    case Delete:  return appendLeaf(out, kindDelete, x.Key, nil)
    case *Delete: if x == nil { return nil, errors.New("nil *Delete") }; return appendLeaf(out, kindDelete, x.Key, nil)
    case Batch:   return appendBatch(out, x.Records, depth)
    case *Batch:  if x == nil { return nil, errors.New("nil *Batch") };  return appendBatch(out, x.Records, depth)
    case nil:     return nil, errors.New("nil record")
    default:      return nil, fmt.Errorf("unsupported record type %T", r)
    }
}

// appendLeaf is the v1 path: check the cap BEFORE appending, then appendFrame (unchanged from v1).
func appendLeaf(out []byte, kind uint8, key string, val []byte) ([]byte, error) {
    p := payloadLen(kind, uint64(len(key)), uint64(len(val)))
    if n := frameLenField(p); n > uint64(maxFrameLen) {
        return nil, fmt.Errorf("frame length %d exceeds cap %d", n, maxFrameLen)
    }
    return appendFrame(out, kind, key, val, p), nil
}

func appendBatch(out []byte, recs []Record, depth int) ([]byte, error) {
    sub := depth + 1                                       // depth of the sub-stream we are about to write
    if sub > maxDepth {
        return nil, errors.New("batch nesting exceeds depth 8")
    }
    // Pre-guard (necessary condition only): every child takes up at least minFrameWire bytes of this
    // Batch's payload, because a child's len, type, payload AND crc all sit inside the parent payload.
    if uint64(len(recs)) > (uint64(maxFrameLen)-typeSize-u32Size)/minFrameWire {
        return nil, fmt.Errorf("batch of %d records cannot fit in a 16 MiB frame", len(recs))
    }   // => len(recs) <= 1,290,554 < 2^32, so uint32(len(recs)) below never truncates
    lenAt := len(out)
    out = append(out, 0, 0, 0, 0)                          // placeholder len
    body := len(out)
    out = append(out, kindBatch)
    out = appendU32(out, uint32(len(recs)))
    for j, r := range recs {
        grown, err := appendRecord(out, r, sub)
        if err != nil {                                    // checked FIRST; out and body still valid
            return nil, fmt.Errorf("batch record %d: %w", j, err)
        }
        out = grown
        if l := uint64(len(out) - body); l > uint64(maxFrameLen) {   // authoritative cap check
            return nil, fmt.Errorf("batch frame length %d exceeds cap %d", l, maxFrameLen)
        }
    }
    binary.BigEndian.PutUint32(out[lenAt:], uint32(len(out)-body))   // index, not pointer: out may have moved
    return appendU32(out, crc32.ChecksumIEEE(out[body:])), nil      // this Batch's own crc, outside len
}
```

**Why the cap check cannot be reached with a bad buffer.** `len(out) - body` is evaluated only after
`err == nil` and `out = grown`. A child that succeeds returns `out` with one complete frame appended. It
never truncates, because every `append*` helper only appends. So `len(out) ≥ body` holds and the
subtraction cannot go negative. The child's return value on error is never looked at, so a later change
that returns the partly grown buffer with the error (as v1's `appendFrame` shape might suggest) cannot
cause a panic or skip the check. I will add a one-line comment saying this at the loop.

**Byte accounting for the cap check.** When the check runs, `out[body:]` holds exactly
`type(1) + n(4) + child frames so far`. Each child frame is complete (`len+type+payload+crc`). By the Batch
grammar (`payload = n frame_1 … frame_n`), those child bytes, *including the children's CRCs*, are this
Batch's payload. The only CRC outside this Batch's `len` is its own, and that is appended after the check
and after `len` is written. So `len(out)-body` at the check equals the `len` field this frame will carry.
That makes the check exact. It never accepts a frame the decoder rejects and never refuses a legal one. An
empty Batch has `len` = 5 and is trivially within the cap.

Why this design:
- **Each record is read exactly once.** This keeps the v1 doc contract. A two-pass size-then-write
  encoder would read nested records twice and could produce an inconsistent frame if the caller broke the
  no-concurrent-mutation rule.
- **The cap applies to every frame at every depth.** Leaf frames are checked before they are appended, as
  in v1. Batch frames are checked after each child, so the buffer goes past the cap by at most one child
  frame, and that child has already passed its own cap check. Memory stays bounded.
- **The pre-guard** only rejects early: if it passes, the running check still decides. Its real job is to
  bound `len(recs)` so the `uint32` count cast is exact.
- **Depth cap and termination.** A self-referential input such as `s := make([]Record,1); s[0] =
  Batch{Records: s}`, or a `*Batch` cycle, is cut off by the depth check when it tries to enter sub-stream
  9. That gives an error, not a stack overflow.
- **No partial output.** Every error path returns `nil, err`. Every caller checks `err` before using the
  buffer, and `Encode` returns `nil, err` on top of that. A partly built buffer cannot reach the caller.
- **Encoder/decoder agreement.** Both use the predicate "entering a sub-stream at depth `sub` is illegal
  iff `sub > 8`". Both check every frame's `len` against the same cap with the same accounting.

### 2.2 Decoder design

The pseudocode below is complete in its control flow. Every error return is written out. `corrupt(...)`
is shorthand for `fmt.Errorf("%w: ...", ErrCorrupt, ...)`.

```go
func Decode(b []byte) ([]Record, error) {
    if len(b) == 0 {
        return nil, corrupt("missing version byte")
    }
    ver := b[0]
    if ver != version1 && ver != version2 {                 // before any frame
        return nil, corrupt("unknown version %d", ver)
    }
    // Capacity hint: at most 1024 entries, and never more than the input could hold. make always
    // returns a NON-NIL slice, including at capacity 0 (e.g. Decode([]byte{2})), so success is non-nil.
    recs := make([]Record, 0, min((len(b)-1)/minFrameWire, 1024))   // builtin min (Go >= 1.21; go.mod 1.25)
    off := 1
    for off < len(b) {
        kind, payload, next, err := nextFrame(b, off)
        if err != nil {
            return nil, fmt.Errorf("%w at offset %d", err, off)
        }
        rec, err := decodeRecord(kind, payload, ver, 0)    // 0 = depth of the top-level stream
        if err != nil {
            return nil, fmt.Errorf("%w in frame at offset %d", err, off)
        }
        recs = append(recs, rec)                            // only reached with err == nil
        off = next                                          // only reached with err == nil; next > off
    }
    return recs, nil
}

// nextFrame validates the frame at buf[off:] against len(buf) ONLY: header, cap, completeness, len!=0,
// crc (the v1 checks in v1 order). On success, next = off + 4 + len + 4 > off. On error, kind/payload/next
// are zero values and callers must not use them.
func nextFrame(buf []byte, off int) (kind uint8, payload []byte, next int, err error)

// decodeRecord parses one frame's payload. depth = depth of the stream this frame sits in.
func decodeRecord(kind uint8, payload []byte, ver byte, depth int) (Record, error) {
    switch {
    case kind == kindPut || kind == kindDelete:
        return decodeLeaf(kind, payload)                    // v1 decodePayload body, unchanged
    case kind == kindBatch && ver == version2:
        return decodeBatch(payload, ver, depth)
    default:                                                // incl. kind 3 in a version-1 stream
        return nil, corrupt("unknown record type %d", kind)
    }
}

func decodeBatch(p []byte, ver byte, depth int) (Record, error) {
    sub := depth + 1                                        // depth of the sub-stream inside p
    if sub > maxDepth {                                     // before reading n or recursing
        return nil, corrupt("batch nesting exceeds depth 8")
    }
    if len(p) < u32Size {
        return nil, corrupt("truncated batch count")
    }
    n := binary.BigEndian.Uint32(p)
    s := p[u32Size:len(p):len(p)]
    if uint64(n) > uint64(len(s)/minFrameWire) {            // before make
        return nil, corrupt("batch count %d exceeds payload", n)
    }
    recs := make([]Record, 0, n)                            // non-nil even for n == 0
    off := 0
    for i := uint32(0); i < n; i++ {
        kind, pl, next, err := nextFrame(s, off)            // bounded by len(s): an overrun is "truncated"
        if err != nil {
            return nil, fmt.Errorf("%w in batch record %d at offset %d", err, i, off)
        }
        rec, err := decodeRecord(kind, pl, ver, sub)
        if err != nil {
            return nil, fmt.Errorf("%w in batch record %d at offset %d", err, i, off)
        }
        recs = append(recs, rec)
        off = next
    }
    if off != len(s) {
        return nil, corrupt("trailing bytes after %d batch records", n)
    }
    return Batch{Records: recs}, nil
}
```

- **First error wins, at every depth.** No loop ever appends a record or moves `off` after an error. An
  inner error comes back through `decodeRecord` → the parent `decodeBatch` loop → … → `Decode`, and each
  level returns `nil, err` straight away. So a corrupt inner frame can never show up as a nil `Record`
  inside a returned `Batch`, and a zero `next` can never be used as an offset. `off` strictly increases
  on every successful step (`next ≥ off + 9`), so the loops always terminate.
- **Never reading into a sibling or outer frame.** `nextFrame` measures completeness only against the
  slice it is given. For an inner frame that slice is `s`, the Batch's own payload after `n`, capped with
  a full-slice expression. An inner `len` that runs past the payload fails the completeness check and is
  reported as truncated. It cannot be satisfied by bytes from the outer CRC, a sibling or the parent.
- **The outer frame's CRC is checked before its payload is parsed**, at every depth.
- **Non-nil success.** Both `make` calls return a non-nil slice for any capacity, including 0. (Go's
  `make` never returns a nil slice. A zero-capacity slice points to the runtime's zero-size base.) So
  `Decode([]byte{2})` returns `[]Record{}` and an empty Batch decodes to `Batch{Records: []Record{}}`.
  §5 checks both with `!= nil`.
- **Allocation safety (bytes, not just counts).** Top-level hint: `16 · min((len(b)-1)/13, 1024)` bytes.
  That is at most 16 KiB, and for short inputs it is at most about `1.23 · len(b)`. A Batch's `make` runs
  only after `n ≤ len(s)/13`, so it is at most `16·len(s)/13` bytes. The sub-slices at successive depths
  are nested, so across all ≤ 8 Batch levels the total pre-allocation is ≤ `8·16/13·len(b)`. That is
  linear in the input the caller already holds. A claimed `n = 0xFFFFFFFF` with a short payload returns
  before `make`.
- **Recursion** is at most 9 frames deep (8 Batches plus a leaf), because the depth check runs before
  any recursion into a sub-stream. CRC work is at most 9× the input size.
- **Error context.** Messages keep the v1 top-level `at offset N`. Nested errors add `in batch record i
  at offset N` (relative to the Batch payload). Every error still wraps `ErrCorrupt`, because every leaf
  error is created with `%w` around `ErrCorrupt` and every wrapper uses `%w`.

## 3. How each v2 rule is met

| Amendment rule | Where / how |
|---|---|
| `Batch{Records []Record}`, Kind 3, only new exported symbol | `walcodec.go`: the type plus a value-receiver `Kind()` returning `kindBatch = 3`. Nothing else exported is added. |
| Batch payload = `n(4)` + `n` full inner frames | `appendBatch` writes `uint32(len(recs))` (exact, thanks to the pre-guard), then each child as a full frame via `appendRecord`. `decodeBatch` reads `n`, then exactly `n` frames via `nextFrame`. |
| Inner frames consume the payload exactly; fewer/more frames or trailing bytes → `ErrCorrupt` | Fewer: the `n` loop runs out of bytes → `nextFrame` truncation (or the `n > len/13` guard), returned at once. More or trailing: the `off != len(s)` check after the loop. |
| Same 16 MiB cap at every depth | Decode: the shared `nextFrame` checks the cap at every depth. Encode: leaves are checked before they are appended. Batches are checked exactly after each child (§2.1 byte accounting). |
| Inner `len` past the enclosing payload → `ErrCorrupt` (truncated), never reads the next outer frame | `nextFrame(s, off)` is bounded by `len(s)` (§2.2). Its error is returned immediately. |
| Depth: top level = 0, a Batch's sub-stream = its depth + 1, sub-stream depth > 8 → `ErrCorrupt` | `decodeBatch` and `appendBatch` compute `sub := depth+1` (depth = the stream holding the Batch) and reject iff `sub > 8`, before touching the sub-stream. The rejection travels up as an error through every level (§2.2 "first error wins"). See the trace below. |
| Encoder enforces the same caps and won't emit what it rejects | Same depth predicate, same cap accounting. Encoder errors do not wrap `ErrCorrupt`, as in v1 (they describe caller input, not corruption). |
| `Encode` writes version 2 (also for Put/Delete-only input) | `out[0] = version2`. Empty input → `[]byte{2}`. |
| `Decode` accepts v1 (Put/Delete only) and v2 (all three) | The version check accepts 1 and 2. `decodeRecord` allows `kindBatch` only when `ver == version2`, so tag 3 in a v1 stream is an unknown type → `ErrCorrupt`. Nested frames inherit the stream's version. |
| Any other version (0, 3, …) → `ErrCorrupt`, before any frame | The version check comes right after the empty check, before the frame loop. |
| v1 regression via a fixed v1-bytes fixture | The Put/Delete decode code is unchanged. The v2 suite's `v1Fixture` (version 1) decodes to the same records. The v1 public suite stays green as it is. §5 adds a golden test: v2 `Encode` of the fixture records equals the fixture with byte 0 set to 2. |

### Depth trace (for the predicate `sub := depth+1; reject iff sub > 8`)

Batch `B_k` is the k-th Batch counted from the outside. `B_1` is a top-level frame.

| Batch | stream holding its frame (`depth`) | its sub-stream (`sub`) | `sub > 8`? |
|---|---|---|---|
| B_1 | 0 | 1 | no |
| … | … | … | no |
| B_8 | 7 | 8 | **no: legal** |
| B_9 | 8 | 9 | **yes: ErrCorrupt** |

So 8 nested Batches (innermost sub-stream at depth 8) are legal, in both Encode and Decode. A 9th Batch is
rejected even if it is empty, because its empty sub-stream would still be at depth 9.

### Public v2 cases, traced
1. Batch round-trip → `appendBatch`/`decodeBatch`. 2. Nested → B_1 enters sub 1, B_2 enters sub 2.
3. v1 fixture → version-1 path. 4. Mixed top level → `appendRecord` dispatch. A nil `Value` decodes as
`[]byte{}`, and `recEq2` compares by string, so they match. 5. `b[:len-2]` → the outer frame is truncated
in `nextFrame`, and `Decode` returns `nil, err`. 6. `b[0]=3` → version check. 7. 9 hand-built wraps:
B_9's `decodeBatch` (depth 8) returns `ErrCorrupt`, and B_8 … B_1 and `Decode` each return it at once →
`(nil, ErrCorrupt)`. 8. Fixture CRC flip → crc check. Fixture `[:len-3]` → truncation.

## 4. Docs

- Package doc: add `Batch = n:u32be frame{n}`, the version rule (writes 2, reads 1|2) and the depth
  convention ("the top-level stream is depth 0; a Batch's sub-stream is one deeper; a sub-stream deeper
  than 8 is corrupt").
- `Encode` doc: writes version 2; accepts `Batch`/`*Batch`; returns errors for nesting deeper than 8 and
  for any frame at any depth over 16 MiB; read-once contract unchanged.
- `Decode` doc: versions 1 and 2 (Batch only in 2); check order, now with "depth, then n, then exactly n
  inner frames bounded by the Batch payload, then no trailing bytes"; the first error at any depth ends
  decoding with a nil result.

## 5. Tests to add (`walcodec_v2_extra_test.go`)

Helpers get an `x` suffix (`beX`, `frameX`, …) so they don't clash with names already in the two public
files (`beU32`, `mkFrame`, `recEq`, `recsEq`, `be2`, `mkFrame2`, `putPL2`, `batchPL2`, `recEq2`, `recsEq2`,
`v1Fixture`, `v1FixtureRecords`).

- **Depth boundary (both sides of the line, both directions).** 8 hand-built nested Batches decode
  successfully. `Encode` of 8 nested Batches succeeds, round-trips, and its bytes equal the hand-built
  stream. 9 nested: `Encode` returns `(nil, err)` with `!errors.Is(err, ErrCorrupt)`, and the hand-built
  form → `(nil, ErrCorrupt)`. An empty Batch as the 9th level is rejected by both. An empty Batch as the
  8th level is accepted by both.
- **Errors stop decoding, at every depth (finding r2-major).** Table test. Each case asserts
  `errors.Is(err, ErrCorrupt)` **and** `got == nil`. Each case also has a valid **sibling after** the bad
  inner frame (inside the same Batch) and a valid top-level frame after the Batch, so that a decoder that
  skipped the bad frame and carried on would return a non-nil result or no error. Cases, each placed at
  depth 1 and at depth 3, all with valid enclosing CRCs: inner truncated frame; inner CRC flip; inner
  unknown type 99; inner `len` = 0; inner `len` > 16 MiB; inner Put with trailing payload bytes; a 9th-level
  Batch with siblings after it. Run under a 1 s `t.Deadline`-style timeout guard (a goroutine plus
  `time.After`) so a spinning loop fails rather than hangs.
- **Self-referential and cyclic input.** `s[0] = Batch{Records: s}` and a `*Batch` cycle → encoder error
  and a nil result, no stack overflow.
- **Encoder child error with a nil buffer.** A Batch whose 2nd child is a nil interface, at depth 1 and at
  depth 5 → `(nil, err)`, no panic. This covers the `grown`/`err` ordering in §2.1.
- **Inner len past the enclosing payload, valid outer CRC.** The bytes that follow in the outer stream (a
  sibling top-level frame) would complete the inner frame if the decoder overran. The result must be
  `(nil, ErrCorrupt)`.
- **Count mismatches, each with a valid outer CRC.** `n=2` with 1 frame; `n=1` with 2 frames; `n=1` plus
  trailing junk; `n=0` with junk; `n=0xFFFFFFFF` with a tiny payload.
- **Allocation, measured in bytes, with separate ceilings.** Each case reads `runtime.MemStats.TotalAlloc`
  around 100 `Decode` calls (`runtime.GC()` first) and divides by 100. Each ceiling has a fixed 1 KiB
  overhead allowance (error formatting, the returned error value), plus the documented hint for that
  input:
  1. `n = 0xFFFFFFFF` Batch, one 13-byte top-level frame (14-byte stream). Hint = 1 entry → ceiling 1 KiB +
     16 B. A `make` sized from `n` would be about 64 GiB.
  2. Huge claimed top-level `len` (`16 MiB + 1`), 6-byte stream. Hint = 0 entries → ceiling 1 KiB.
  3. Same huge-`len` header followed by 1 MiB of zero padding. Hint = `min(1 MiB/13, 1024)` = 1024 entries
     → ceiling 1 KiB + 16 KiB. This checks that the hint is a hard cap and does not grow with input size
     (uncapped it would be about 1.2 MiB).
  4. Linear bound: a legal Batch of N = 10,000 empty-key Deletes. Ceiling = `c·len(b)` with c = 4 (covers
     the Batch `make`, the top-level hint and the per-record `Delete` boxing).
- **Encoder cap, exact boundary.** Build a Batch whose `len` field is exactly `maxFrameLen` (one Put
  child sized so that 1 + 4 + child frame = 16 MiB) → accepted, decodes, round-trips. Increase the Put's
  value by one byte → encoder error. Also: a Put at the max leaf size wrapped in a Batch → error, because
  the outer frame goes over the cap. Hand-build a Batch frame with `len` = `maxFrameLen` + 1 and a valid
  CRC → `ErrCorrupt`. These pin down that both sides use the same accounting (inner CRCs inside the parent
  `len`, the Batch's own CRC outside it). These cases can be skipped under `testing.Short`.
- **Pre-guard.** A Batch with 1,290,555 empty-key Deletes (one over the guard; 1,290,554 of them make a
  legal 16,777,207-byte `len`, so the guard is tight) → encoder error, returned before the buffer grows
  past the guard. This is skippable under short mode.
- **Version gating.** Tag 3 inside a version-1 stream → `ErrCorrupt`. Versions 0, 3, 255 → `ErrCorrupt`.
  `Decode([]byte{2})` and `Decode([]byte{1})` → `got != nil && len(got) == 0`, no error.
- **Golden bytes.** `Encode(v1 fixture records)` equals the fixture with byte 0 = `2`. A hand-assembled
  `Batch{Put, Delete}` stream equals `Encode` output exactly.
- **Misc.** `*Batch` is accepted and decodes as a `Batch` value. A nil `*Batch` → error, nil result. An
  empty Batch round-trips to `Batch` with `Records != nil && len(Records) == 0`, at the top level and nested.
  Decoded nested records do not alias `b` (mutate `b` after decode). `Batch{}.Kind()==3`.
- **Fuzz.** `FuzzDecodeV2`, seeded with v1/v2/nested/corrupt streams. For every input, one of these must
  hold:
  (a) `Decode` errors: the error wraps `ErrCorrupt` and the result is nil; or
  (b) `Decode` succeeds: the result is non-nil and contains no nil `Record` at any depth; `Encode(recs)`
  succeeds; `Decode(Encode(recs))` equals `recs` (stability); **and** `Encode(recs)` equals `b` with byte
  0 set to 2 (canonicality).
  Property (b) holds because Decode accepts only canonical streams: every `len` is determined by the
  payload, every CRC by its bytes, every count by the frames, and trailing bytes are rejected at every
  level. Nil and empty `Value` both decode to `[]byte{}` and re-encode identically, and the version is the
  only free byte. If (b)'s byte check ever fails, that is a strictness bug in Decode to fix.
  `FuzzRoundTripV2` builds random record trees with bounded depth and checks that they round-trip.

Verification gate before handing off: `gofmt -l` is empty, `go vet ./...` is clean, and
`go test ./...`, `go test -race ./...`, and short fuzz runs of both targets all pass. The v1 and v2
public suites must pass unmodified.

## 6. Out of scope / non-changes

- No streaming API, no exported depth or cap constants, no new error sentinels (the surface is frozen).
- No change to Put/Delete wire bytes. The only byte-level change for Put/Delete-only input is the
  version byte, which the amendment explicitly allows.
