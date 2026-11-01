# Plan: `walcodec` v2 — nested `Batch` records

Amendment only. v1 rules still hold except where the amendment changes them. This plan replaces the previous one and answers `plan/plan_findings.md` in `plan/plan_rebuttal.md`.

The phase-2 worktree currently contains `go.mod`, `walcodec_public_test.go`, and `walcodec_v2_public_test.go`, and does not contain `walcodec.go`. The existing v1 implementation is `snapshot-v1/walcodec.go`. Implementation starts by copying that file into `worktree/walcodec.go` and then applying the changes below. Do not rewrite the v1 codec from scratch.

## What stays

Frozen names and signatures. Do not add, rename, or re-sign these exported symbols.

```go
type Record interface { Kind() uint8 }

type Put    struct { Key string; Value []byte } // Kind 1
type Delete struct { Key string }               // Kind 2

func Encode(recs []Record) ([]byte, error)
func Decode(b []byte) ([]Record, error)

var ErrCorrupt = errors.New("walcodec: corrupt stream")
```

Unchanged wire rules for every frame at every depth:

- Big-endian integers.
- `frame = len(4) type(1) payload(len-1) crc(4)`.
- `len` counts `type`+`payload` only. CRC is outside `len`.
- CRC is CRC32-IEEE (`hash/crc32.ChecksumIEEE`) over `type`+`payload`.
- `len` cap is `16 << 20` (`16777216`), inclusive. A larger `len` is `ErrCorrupt` before any allocation sized from that `len`. A `len` of 0 is also `ErrCorrupt` (no type byte).
- `Put` payload = `[klen uint32][key][vlen uint32][value]`.
- `Delete` payload = `[klen uint32][key]`.
- `nil` and empty `Value` both encode as `vlen = 0` and both decode to a length-0 `[]byte`. They compare equal. Decode copies `Value` out of the input buffer (`make` + `copy`), so a zero-length value is non-nil.
- Duplicate keys stay, in order. The codec is a log, not a map.
- Empty `recs` encodes to a one-byte version-only stream and decodes to a non-nil empty slice, no error.
- `Decode(nil)` and `Decode([]byte{})` are `ErrCorrupt` (missing version byte).
- Truncation, CRC mismatch, unknown type tag, and a payload whose interior lengths do not consume the frame exactly are `ErrCorrupt`.
- `Decode` returns the `ErrCorrupt` sentinel directly so `errors.Is` matches. Encode failures stay non-`ErrCorrupt`, with a nil buffer. Error text is not a contract.
- Encode accepts only value forms (`Put`, `Delete`, and `Batch`). Pointer forms, a nil `Record`, and any other implementation are rejected without calling `Kind` (a nil `*Put`, `*Delete`, or `*Batch` would panic). Decode returns values, never pointers.
- Integer preflight, then one write into a buffer of exact capacity. Length arithmetic stays `int64` / `uint64` so a large field cannot wrap before the cap rejects it.
- `go.mod` stays `module walcodec`, `go 1.25`. The two public test files stay byte-for-byte as shipped. No new dependencies.

## What changes

### New type

```go
// Batch is a nested sequence of records. Kind is 3.
//
// A nil Records slice and an empty Records slice both encode as n = 0
// followed by no inner frames. Decode of that payload returns Records set
// to a non-nil empty slice. Nil and empty are equal for this codec: compare
// with len, or with a non-nil empty slice. reflect.DeepEqual treats nil and
// empty as different, so a Batch encoded with a nil Records does not
// DeepEqual the value Decode returns. This is the same equality rule as
// Put.Value (nil and empty both encode as length 0 and both decode as a
// non-nil length-0 slice) and the same shape as top-level Decode of a
// version-only stream, which returns []Record{}, not nil.
type Batch struct { Records []Record }

func (Batch) Kind() uint8 { return 3 }
```

`Batch` is the only addition to the exported surface. Its payload is itself a framed sub-stream:

```
Batch payload = n(4) frame_1 frame_2 ... frame_n
```

`n` is a big-endian `uint32` count of inner records. Each inner frame is a full `[len][type][payload][crc]` frame. Inner records may be `Put`, `Delete`, or nested `Batch`.

### Version byte

- `Encode` writes `version = 2` for every stream, including Put/Delete-only input and the empty stream. This is the allowed behavior change. v1 public tests do not assert that the written version is 1.
- `Decode` accepts version 1 and version 2. Any other version byte, including 0 and 3, is `ErrCorrupt`, checked before any frame is read.
- Version 1 may contain only `Put` and `Delete`. A type tag other than 1 or 2 in a version-1 stream, including `Batch` (3), is `ErrCorrupt`.
- Version 2 may contain `Put`, `Delete`, and `Batch`.

Keep two constants: `versionV1 = 1` (decode only) and `versionV2 = 2` (the byte `Encode` writes, and one of the two bytes `Decode` accepts).

### Depth

One named constant, used by both the encode check and the decode check so they cannot drift:

```go
// maxDepth is the deepest legal sub-stream. The top-level stream is depth 0.
// A sub-stream at depth greater than maxDepth is rejected. Eight wrapping
// Batches are legal; a ninth is not.
const maxDepth = 8
```

The top-level stream is depth 0. A `Batch`'s inner sub-stream is decoded and encoded one level deeper than the `Batch` itself. A sub-stream at depth greater than `maxDepth` is `ErrCorrupt`. Equivalently: 8 levels of `Batch` nesting are legal; a 9th is rejected.

Numbering used by the public test and this plan: the outermost `Batch` is a record in the depth-0 stream. Opening it creates a sub-stream at depth 1. The k-th wrapping `Batch` sits in a stream at depth k-1, and its sub-stream is depth k. Eight wraps put the innermost sub-stream at depth 8, which is legal. Nine wraps put a sub-stream at depth 9, which is rejected.

The check is on the sub-stream depth, before that sub-stream is opened: a `Batch` sitting at depth `d` is rejected when `d+1 > maxDepth`. A `Batch` frame sitting at depth 8 is therefore illegal to open, including when its payload is `n = 0` (an empty `Batch` at the depth-9 position). The check runs before inner frames are read and before any allocation for those frames.

The encoder enforces the same cap and does not emit a stream it would reject. `Encode` of a 9-deep `Batch` returns a non-`ErrCorrupt` error and a nil buffer, the same class of failure as an oversize frame or an unknown record type. The depth-8 public test builds the illegal stream by hand because the encoder refuses it.

### Exact consumption and bounds

The `n` inner frames must consume the enclosing `Batch` payload exactly. Fewer well-formed frames than `n`, more bytes than `n` frames, or trailing bytes after `n` frames is `ErrCorrupt`.

An inner `len` that would read past the enclosing payload is `ErrCorrupt` (truncated). The decoder passes the frame reader a sub-slice of the remaining enclosing payload. The reader decides truncation by comparing against `len` of that slice. It never receives an offset into a sibling frame or an outer frame, and it never reads past the slice it was given.

The same 16 MiB `len` cap applies to every frame at every depth, checked before any allocation sized from that `len`.

### Hostile `n`

`n` is untrusted. Nothing is allocated from the raw `n`.

- Decode builds the result by `append` of records that have already been decoded. It does not call `make([]Record, n)` or `make([]Record, 0, n)`, and it does not pass `n` to any other allocator.
- Before the walk, if `n` cannot fit in the remaining payload, return `ErrCorrupt` with no allocation. The bound is the format-invariant minimum wire size of a frame, not the minimum of today's kinds:

```go
// minFrameWire is a safe lower bound on any frame this format can hold:
// 4 (len) + 1 (type) + 4 (crc). A future kind with an empty payload is still
// at least this large. A kind that needs a smaller wire cannot exist under
// the current frame shape, so this bound does not need to be revisited when
// a kind is added. It is not the 13-byte minimum of today's Put, Delete, and
// Batch payloads. Using 9 instead of 13 only lets a few more impossible n
// values reach the append loop; the loop still allocates only for records
// that have already been decoded.
const minFrameWire = headerSize + 1 + crcSize // 9
```

  The check is `uint64(n) > uint64(len(payload)-4)/uint64(minFrameWire)`, using `uint64` so `n == 0xFFFFFFFF` cannot wrap. A `Batch` frame of about 20 bytes with `n == 0xFFFFFFFF` fails this check and returns `ErrCorrupt`.
- `append` then grows only with successful reads. Each successful read consumes at least `minFrameWire` bytes of a payload already capped at 16 MiB, so the result slice is bounded by the payload size.

Both the pre-check and the append-only walk are required. The pre-check is not permission to allocate `n` entries after it passes.

## Internal API (pinned)

No package-level depth variable, no per-call closure that hides depth, and no leftover v1 signature that cannot carry depth. `Encode` and `Decode` stay reentrant.

The codec tables are package-level variables filled in `init`, then never mutated. They are not composite literals that mention `batchCodec`. A literal would be an initialization cycle: `batchCodec` refers to `decodeBatch` and `batchBodyLen`, those functions close over the table variables, and the table variables would refer back to `batchCodec`. Go rejects that cycle (`initialization cycle for batchCodec`). Filling the slots in `init` is the chosen fix. Do not add a table parameter to `bodyLen` or `decode`; the signatures below stay as written. Do not use a lookup switch in place of the tables.

```go
// frameSize is one measured frame. Encode builds this tree once and the
// write pass replays it. children is nil for Put and Delete.
type frameSize struct {
	codec    *kindCodec
	kind     uint8
	bodyN    int64 // type + payload
	rec      Record
	children []frameSize
}

// codecTable is indexed by kind. A short table or a nil slot is an unknown tag.
type codecTable struct {
	byKind []*kindCodec
}

type kindCodec struct {
	// bodyLen measures rec at depth. The int is that record's stream depth.
	// Put and Delete ignore depth and return nil children. Batch returns the
	// child tree from this single walk. Signature: (Record, depth int) ->
	// (bodyN int64, children []frameSize, err error).
	bodyLen func(Record, int) (int64, []frameSize, error)
	// write fills a payload region whose length bodyLen already reserved.
	// It has no error return: every failure (unknown type, cap, depth, n
	// that does not fit in uint32) is returned by bodyLen during preflight.
	// It does not call bodyLen, does not check depth, and does not read the
	// caller's Records slice. Put and Delete ignore children.
	write func([]byte, Record, []frameSize)
	// decode parses a payload whose outer frame CRC has already been checked.
	// The int is this record's stream depth. Put and Delete ignore it.
	// Signature: (payload []byte, depth int) -> (Record, error).
	// The table is not a parameter. decodeBatch reads the version-2 table
	// variable, which init has already filled.
	decode func([]byte, int) (Record, error)
}

// readFrame reads one frame from b. b is the remaining enclosing slice, never
// an offset into a parent buffer. table selects legal kinds. depth is the
// stream depth of the record this frame contains.
func readFrame(b []byte, table *codecTable, depth int) (Record, int, error)
```

`Put` and `Delete` function bodies stay as they are, with depth and children added to the signatures and ignored. Their `bodyLen` and `decode` wrappers ignore the new `int` and return `nil` children.

### Tables and who indexes them

Three named table variables. There is no v1-sized global array on the encode path, so `kind 3` is never an out-of-range index into a `[kindDelete+1]` array.

`encodeTable` is the table the encoder indexes. `decodeV2Table` is the table version-2 decode indexes, including every nested `Batch`. They start as separate variables so a later version can add a kind to the encoder without changing the version-2 grammar. Today `init` points both at the same `byKind` slice (kinds 1, 2, and 3). Re-pointing `encodeTable` later is the extension. Re-pointing `decodeV2Table` is a version-2 grammar change and is not done when a new kind is added.

`decodeV1Table` is a separate slice with slots for kinds 1 and 2 only. Kind 3 is an unknown tag on a version-1 stream.

```go
// Declared empty so the composite literals do not mention batchCodec.
// init fills them once. Nothing mutates them after init.
var encodeTable codecTable
var decodeV2Table codecTable
var decodeV1Table codecTable

func init() {
	v2 := []*kindCodec{
		kindPut:    putCodec,
		kindDelete: deleteCodec,
		kindBatch:  batchCodec, // index 3
	}
	encodeTable = codecTable{byKind: v2}
	// Same slice today. A future version re-points encodeTable only.
	decodeV2Table = codecTable{byKind: v2}
	decodeV1Table = codecTable{byKind: []*kindCodec{
		kindPut:    putCodec,
		kindDelete: deleteCodec,
	}}
}
```

`putCodec`, `deleteCodec`, and `batchCodec` are `var` values of type `kindCodec`, also filled in that same `init` (or as literals that do not mention any table variable). `batchCodec.bodyLen` is `batchBodyLen`. `batchCodec.decode` is `decodeBatch`. `batchCodec.write` is `writeBatch`. `batchBodyLen` and `decodeBatch` read `encodeTable` and `decodeV2Table` only at call time, after `init` has returned. That is why the cycle is gone: no package-level initializer refers to a variable whose initializer refers back to it.

Who indexes which table:

- `codecOf` type-switches, then `measure` looks up `encodeTable` only.
- `appendFrame` does not index a table. `measure` stores the `*kindCodec` it got from `encodeTable` on the `frameSize`, and `appendFrame` calls `sized.codec.write`.
- Top-level `Decode` of version 1 passes `&decodeV1Table` to `readFrame`.
- Top-level `Decode` of version 2 passes `&decodeV2Table` to `readFrame`. It does not pass `&encodeTable`.
- `decodeBatch` passes `&decodeV2Table` into inner `readFrame` calls. It does not hard-code `&encodeTable`. A `Batch` exists only on the version-2 path, so its interior always uses the version-2 grammar, independent of whatever `encodeTable` points at after a later version re-points it.

`codecOf` type-switches `Put`, `Delete`, and `Batch` values. `*Put`, `*Delete`, `*Batch`, a nil `Record`, and any other implementation hit the default arm and return an encode error without calling `Kind`.

`measure` resolves every record at every depth through `codecOf` before it indexes a table. It does not call `rec.Kind()` and it does not index `encodeTable.byKind[rec.Kind()]`. A nil `Record`, a `*Put`, a `*Delete`, a `*Batch`, or a foreign `Record` nested inside a `Batch` (including inside a `Batch` nested in a `Batch`) therefore returns an encode error and a nil buffer. It does not panic.

### Measure once, then write

`Encode` calls `measure(rec, 0)` once per top-level record. `measure` is the only recursive walk. It calls `codecOf(rec)` (the type switch), then looks up `encodeTable`, then calls `bodyLen(rec, depth)`. That call returns the body length and, for a `Batch`, the child `[]frameSize`. The write pass receives that tree and does not call `bodyLen` again.

`measureBatch` / `batchBodyLen`:

1. If `depth+1 > maxDepth`, return an encode error. Do this before walking children, including when `Records` is nil or empty.
2. Do not copy `Records`. The previous copy stored `Batch{Records: copied}` on the tree and protected nothing: the write pass reads `frameSize.children`, and each child `frameSize` already holds its own `rec` (the interface value `measure` captured). A caller mutating the original `Records` backing array between measure and write cannot change those captured values. `Encode` still does not promise safety under a concurrent data race on key or value bytes; that is the same sharing rule v1 already has for `Put.Value`.
3. If `uint64(len(b.Records)) > math.MaxUint32`, return an encode error. The 16 MiB cap rejects a legal `Batch` before `n` can overflow `uint32` (each inner frame is at least `minFrameWire` bytes). The explicit check is what the write of `n` relies on.
4. Measure each child at `depth+1`, resolving it through `codecOf`. Keep a running `int64` sum of `headerSize + child.bodyN + crcSize`. If that running sum, plus the type byte and the `n` field (`1 + u32Size + sum`), would exceed `maxFrameLen`, or the running total would exceed `maxInt`, return an encode error immediately. Do not measure the remaining children. An oversize `Batch` therefore does not build the rest of its tree before it is rejected.
5. Return that body length and the child tree.

`writeBatch` writes `n = len(children)` as a big-endian `uint32`, then `appendFrame` for each child into the payload region `appendFrame` already reserved. It does not re-measure. It does not read `rec.(Batch).Records`.

`appendFrame` is the single place that writes `len`, type, payload, and CRC. Recursion is in `bodyLen` for `Batch`. Nested `appendFrame` calls run at write time on the child tree, at the depth already checked during measure.

The v1 `appendFrame` reslice `buf[start+4 : start+4+bodyN]` keeps the capacity of the whole stream buffer. A child `appendFrame` given `payload[4:4]` would then have capacity that runs past the `Batch` payload, into the outer CRC slot and the sibling frames. A measure/write mismatch would silently overwrite those bytes. That is not the intended loud failure. Require three-index slices so the reserved region cannot be written past:

```go
// body is exactly bodyN bytes. Cap equals length, so a child write that
// overruns panics instead of spilling into the outer CRC or the next frame.
body := buf[start+headerSize : start+headerSize+int(bodyN) : start+headerSize+int(bodyN)]
```

`writeBatch` hands each child the same shape. After the `n` field is written, the child region is `payload[4:4:len(payload)]` (length 0, cap equal to the reserved child bytes). Each nested `appendFrame` extends that slice within the cap. After the children are written, `writeBatch` checks `len(written) == len(payload)-4`. A mismatch is a bug inside `bodyLen`. The check panics with a distinct message (`walcodec: measure/write length mismatch`). That panic is not a user-input path: preflight already returned every input error, and write is only called with the tree preflight produced.

### Shared frame reader

`readFrame`:

1. Need 4 bytes for `len`, else `ErrCorrupt`.
2. If `len > 16 MiB` or `len < 1`, return `ErrCorrupt` before allocating.
3. If `len(b) < 4 + len + 4`, return `ErrCorrupt`. Do not read past `b`.
4. Check CRC over `type`+`payload`. Mismatch is `ErrCorrupt`.
5. Dispatch on `type` using the `table` argument. A kind past the table or a nil slot is `ErrCorrupt`.
6. Call `codec.decode(payload, depth)`.
7. Return the record and the bytes consumed (`4 + len + 4`).

Top-level `Decode` walks by passing `b[off:]` to `readFrame` until the slice is empty. `Batch` decode walks `payload[4:]` the same way and stops when `n` records have been consumed. Passing a sub-slice is what makes "inner `len` past the enclosing payload" identical to ordinary truncation.

### Decode control flow

```
Decode(b):
  if len(b) < 1 → ErrCorrupt
  version = b[0]
  if version == 1 → table = &decodeV1Table
  if version == 2 → table = &decodeV2Table
  else → ErrCorrupt                 // before any frame
  if len(b) == 1 → []Record{}, nil
  walk b[1:] with readFrame(slice, table, depth 0), appending each record
```

Version is selected once, before the loop. `decodeV1Table` has no kind-3 slot, so a hand-built version-1 stream that contains a `Batch` frame fails as an unknown type after that frame's CRC check. Version 2 never consults `encodeTable`, so adding a kind to the encoder later does not make a version-2 stream accept that kind.

`decodeBatch(payload, depth)`:

1. If `depth+1 > maxDepth`, return `ErrCorrupt` before reading the sub-stream.
2. If `len(payload) < 4`, return `ErrCorrupt`.
3. Read `n`. If `uint64(n) > uint64(len(payload)-4)/uint64(minFrameWire)`, return `ErrCorrupt` before any `make`.
4. `var recs []Record`. For `i` in `0..n-1`, `readFrame` on the remaining sub-slice with `&decodeV2Table` and `depth+1`, then `append`. Any inner error is `ErrCorrupt`.
5. If bytes remain after exactly `n` frames, return `ErrCorrupt`.
6. If `recs == nil`, set `recs = []Record{}`. Return `Batch{Records: recs}`.

### Encode control flow

1. `measure` each top-level record at depth 0 through `codecOf` and `encodeTable`. Failures return a non-`ErrCorrupt` error and a nil buffer.
2. If `1 + sum(frame wires)` exceeds `maxInt`, return an encode error and a nil buffer.
3. `make([]byte, 1, total)`, write version byte `2`, then `appendFrame` each top-level `frameSize`.

### Doc comments that must be updated

These v1 comments become false under v2. Update them in the same edit as the code.

| Location in `snapshot-v1/walcodec.go` | Replacement |
| --- | --- |
| `ErrCorrupt` comment, the Encode error list ("nil or unknown record, a frame that would exceed the 16 MiB cap, and a stream that would exceed the addressable size of an int") | Add nesting whose sub-stream depth would exceed `maxDepth`. Decode list stays the format failures, including a version other than 1 or 2, an inner frame that escapes its enclosing payload, an `n` that does not match the payload, and a sub-stream deeper than `maxDepth`. |
| `kindCodec` comment | Document the pinned signatures above: depth on `bodyLen` and `decode`, child tree returned by `bodyLen` and consumed by `write`, `write` has no error return. Tables are filled in `init` and not mutated after. |
| `codecs` var comment ("Only v1 kinds are registered") and the var | Delete. Replace with `encodeTable` ("encoder kinds; a later version re-points this"), `decodeV2Table` ("version-2 kinds only; nested Batch uses this, not encodeTable"), and `decodeV1Table` ("version-1 kinds only; kind 3 is unknown"). Comment that `init` fills them to avoid the `batchCodec` cycle. |
| `Encode` comment ("writes a version-1 stream"; "Only Put and Delete values") | Version-2 stream, including Put/Delete-only and empty input. Encodable values are `Put`, `Delete`, and `Batch`. Pointer forms include `*Batch`. Encode errors include nesting deeper than `maxDepth`, still with a nil buffer, still not `ErrCorrupt`. |
| `codecOf` comment | Value forms are `Put`, `Delete`, and `Batch`. Lookup is `encodeTable` only. Nested records go through this same switch. |
| `appendFrame` comment ("does not size the record again") | Still true, and stronger: it replays a `frameSize` and does not index a global array. The body slice is three-index, cap equal to length. Nested frames come from the child tree. |
| `Decode` comment ("parses a version-1 stream") | Accepts a version-1 stream (Put/Delete only) and a version-2 stream (Put, Delete, Batch). Any other version is `ErrCorrupt` before any frame. Version 2 uses `decodeV2Table`. |
| `frameParser` / `frameParserFor` comments ("v1 is the only version this decoder knows") | Remove those helpers. The version switch and `readFrame` carry the new comments. |
| `decodeFrameV1` comment | Becomes the `readFrame` comment: cap and zero `len` rejected before allocation; the slice bound is the enclosing payload. |

### What not to do

- Do not bump `Record`, `Put`, `Delete`, `Encode`, `Decode`, or `ErrCorrupt` signatures, and do not wrap records in a new container type.
- Do not make `Encode` emit version 1 for Put/Delete-only input. The amendment requires version 2.
- Do not treat a sub-stream at depth 8 as illegal. The illegal case is a sub-stream at depth 9 (9 wrapping `Batch` frames, or an empty `Batch` whose sub-stream would sit at depth 9).
- Do not allocate a buffer of size `len` before the cap check.
- Do not allocate a record slice from `n` (`make([]Record, n)`, `make([]Record, 0, n)`, or any other size taken from the raw count).
- Do not satisfy an inner `len` by reading past the enclosing payload slice.
- Do not accept version 0 or 3, even if the rest of the stream is well formed.
- Do not return `ErrCorrupt` from `Encode`. Depth and cap failures on the way out are encode errors and a nil buffer.
- Do not keep a package-level mutable depth. Depth is an argument.
- Do not re-measure on the write path, and do not drop a write-time sizing error. `write` has no error return because it does not size.
- Do not leave `codecOf` or `appendFrame` indexing the old `[kindDelete+1]` array.
- Do not decode an empty `Batch` as `Records: nil`. The decoded slice is non-nil and empty.
- Do not delete `walcodec_v2_internal_test.go` before hand-off.
- Do not declare that test file as `package walcodec`.
- Do not initialize `encodeTable` or `decodeV2Table` with a composite literal that names `batchCodec`.
- Do not pass `&encodeTable` into `readFrame` or into `decodeBatch`. Version-2 decode uses `decodeV2Table`.
- Do not call `Kind` on a record that has not passed the `codecOf` type switch, at any depth.
- Do not copy `Records` into the measure tree.
- Do not hard-code the depth cap as the literal `8` in the checks. Use `maxDepth`.
- Do not hard-code `13` as the hostile-`n` divisor. Use `minFrameWire`.
- Do not reslice a payload with a two-index slice that keeps the outer buffer's capacity.

## How each v2 rule is satisfied

| Rule | How |
| --- | --- |
| `Batch{Records []Record}`, Kind 3 | New struct and value-receiver `Kind`. Only new exported name. Doc comment states the nil-versus-empty equality rule. |
| Payload is `[n uint32]` plus `n` full frames | `writeBatch` writes `n` then one `appendFrame` per child. `decodeBatch` reads `n` then `n` frames. |
| Inner records may be `Put`, `Delete`, or `Batch` | `bodyLen` for `Batch` recurses through `codecOf` and `encodeTable`. `decodeBatch` calls `readFrame` with `decodeV2Table`. |
| 16 MiB cap at every depth, before allocation | `sizedBody` on every measured frame, and `readFrame` on every decoded frame, before any allocation sized from that `len`. The `Batch` sum is checked inside the child loop, so an oversize nest stops before the rest of the tree is built. |
| Inner `len` past enclosing payload is truncated `ErrCorrupt` | `readFrame` sees only the remaining enclosing sub-slice. |
| `n` frames must consume the payload exactly | After `n` successful inner reads, remaining length must be 0. Early end of slice is `ErrCorrupt`. `n` that cannot fit is `ErrCorrupt` before the loop. |
| Nothing is allocated from hostile `n` | Append-only results, plus `n <= remaining/minFrameWire` before any `make`. `minFrameWire` is 9. |
| Depth: top-level 0, sub-stream is parent+1, depth > 8 is `ErrCorrupt` | `depth` argument on `bodyLen`, `decode`, and `readFrame`. Check `depth+1 > maxDepth` before opening the sub-stream, including an empty `Batch`. `maxDepth` is 8. |
| Encoder will not emit a stream it would reject | Same `maxDepth` check in `measure` / `batchBodyLen`. Over-deep input returns an encode error and a nil buffer. |
| `Encode` writes version 2, including Put/Delete-only | `buf[0] = versionV2`. |
| `Decode` accepts version 1 (Put/Delete only) and version 2 (all three) | Version switch before the frame loop. `decodeV1Table` has no kind-3 slot. Version 2 uses `decodeV2Table`. |
| Any other version, including 0 and 3, is `ErrCorrupt` before any frame | Version switch default, before `readFrame`. |
| Frozen v1 names and signatures | No edits to `Record`, `Put`, `Delete`, `Encode`, `Decode`, `ErrCorrupt`. |
| v1 decode behaviors hold on a fixed version-1 fixture | Version-1 path keeps CRC, truncation, unknown type, oversize, empty, nil, duplicate-key, and nil-versus-empty value behavior. Regression is the fixture in the public suite, not a replay of `Encode`'s first byte. |

## Public tests (already shipped; do not edit)

`walcodec_v2_public_test.go` runs beside `walcodec_public_test.go`. Both are `package walcodec`.

1. `TestBatchRoundTrip` — `Batch` of `Put` and `Delete` round-trips.
2. `TestNestedBatchRoundTrip` — `Batch` containing a `Batch` and a `Delete` round-trips.
3. `TestV1FixtureDecodesUnderV2` — fixed bytes starting with `0x01` decode to `Put{"alpha","one"}` then `Delete{"beta"}`.
4. `TestMixedTopLevelRoundTrip` — top-level `Put`, `Delete`, `Batch`, `Put` with nil value round-trips. Nil value decodes as length 0; `recEq2` compares values with `string(...)`, so nil and empty compare equal.
5. `TestTruncatedBatch` — encode a two-record `Batch`, drop the last 2 bytes, expect `ErrCorrupt`.
6. `TestVersion3Rejected` — encode a `Put`, set byte 0 to 3, expect `ErrCorrupt`.
7. `TestBatchNestedPastDepth8` — hand-built version-2 stream, a `Put` wrapped in 9 `Batch` frames, expect `ErrCorrupt`. That is a sub-stream at depth 9.
8. `TestV1BehaviorsViaFixture` — the same fixture decodes; flipping its last byte is `ErrCorrupt`; dropping its last 3 bytes is `ErrCorrupt`.

v1 public tests must still pass. They never assert the written version byte. `TestUnknownType` builds a version-1 stream with type 99, which `decodeV1Table` still rejects. `TestOversizeLen` uses version 1 and a `len` of `16<<20+1`, still rejected before allocation. `TestEmptyRecsVersionOnly` only checks that empty `Encode` is one byte and decodes to length 0; that byte will be `2`.

The shipped v2 file says it runs alongside the v1 suite in the same `walcodec` package and gives its helpers a `2` suffix so they do not collide. Hidden grader files are added to that same package. An internal test in `package walcodec` can therefore fail the whole package if it redeclares a grader name (`TestBatchDepth8RoundTrip`, `mkFrame3`, `be`, `batchPL`, and similar). The internal file must not be in that package.

## Permanent internal tests

Add `worktree/walcodec_v2_internal_test.go` and keep it. The amendment forbids editing the shipped public tests. It does not forbid an additional test file. This file is the regression net for the rules the public suite does not pin. Do not delete it before hand-off.

The file is an external test package:

```go
package walcodec_test

import "walcodec"
```

Every top-level identifier uses the prefix `IntV2` for tests and `intv2` for helpers. Examples: `TestIntV2_Depth8RoundTrip`, `TestIntV2_HostileN`, `intv2Frame`, `intv2BE`, `intv2BatchPL`. No unprefixed name such as `be`, `mkFrame`, `batchPL`, `TestEmptyBatch`, or `TestBatchDepth8RoundTrip`. A grader file in package `walcodec` cannot collide with identifiers in package `walcodec_test`. The file uses only the exported API and hand-built bytes. It adds no exported helper that another package would import. `go test` still compiles and runs it with the package.

Cases it must contain:

- Eight nested `Batch` levels around a `Put` encode and decode equal to the input.
- Nine nested `Batch` levels: `Encode` returns a non-nil error, a nil buffer, and `errors.Is(err, ErrCorrupt)` is false. A hand-built 9-wrap stream decodes as `ErrCorrupt`.
- An empty `Batch` at the depth-9 position (nine wraps, innermost payload is `n = 0` and no frames) decodes as `ErrCorrupt`. `Encode` of that value returns an error and a nil buffer.
- `n` larger than the number of well-formed inner frames is `ErrCorrupt`. `n` smaller, with trailing inner frames, is `ErrCorrupt`.
- Hostile `n`: a `Batch` frame of a few dozen bytes whose `n` is `0xFFFFFFFF` decodes as `ErrCorrupt` and returns. This is the allocation crash case.
- Trailing bytes after `n` well-formed inner frames are `ErrCorrupt`.
- An inner `len` that extends past the enclosing `Batch` payload and covers exactly the following top-level sibling frame's bytes is `ErrCorrupt`. Those sibling bytes are present in the stream, so a reader that uses the outer buffer would see them. The decoder must not return the sibling as an inner record.
- An inner `len` that covers the next inner frame's bytes inside the same payload (CRC computed over that span, `n` still 2) is `ErrCorrupt`.
- A version-1 stream with a type-3 frame and a valid CRC is `ErrCorrupt`.
- Version byte 0 is `ErrCorrupt` (`[]byte{0}`, and an otherwise well-formed stream with byte 0 set to 0).
- An inner CRC flip is `ErrCorrupt`.
- An inner `len` of `16<<20+1`, with the huge body not present, is `ErrCorrupt`.
- Empty `Batch` (`Records` nil and `Records` empty) encodes as `n = 0` and decodes as `Batch{Records: non-nil empty slice}`.
- `Encode(nil)` is `[]byte{2}`. `Decode` of that byte returns a non-nil empty slice.
- `Encode` of a `*Batch`, a nil `Record`, and a foreign `Record` at the top level returns an error and a nil buffer.
- The same three rejections inside a `Batch`, and inside a `Batch` nested in a `Batch`: a nil `Record`, a `*Put`, and a foreign `Record`. Each returns an error and a nil buffer and does not panic.

## Test approach

Acceptance commands, run from `worktree` after the copy and the edits:

```
go test ./...
go vet ./...
go build ./...
```

`go test ./...` runs both shipped public files and `walcodec_v2_internal_test.go`. All three commands must be clean. Do not modify `walcodec_public_test.go` or `walcodec_v2_public_test.go`.

## File touch list

- Copy `snapshot-v1/walcodec.go` to `worktree/walcodec.go`.
- Edit `worktree/walcodec.go`: add `Batch` and its doc comment, kind 3, `maxDepth`, `minFrameWire`, `encodeTable` / `decodeV2Table` / `decodeV1Table` filled in `init`, pinned `kindCodec` signatures, measure-once `frameSize` tree, version-2 encode, version-1-or-2 decode, `readFrame`, recursive payload, depth cap, hostile-`n` check, append-only `Batch` decode, three-index body slices, and every doc comment in the table above.
- Add `worktree/walcodec_v2_internal_test.go` as `package walcodec_test` and keep it.
- Do not edit `go.mod` or either public test file.

## Implementation order

1. Copy `snapshot-v1/walcodec.go` into the worktree. The v2 public test file does not compile until `Batch` exists. That compile error is the expected first state.
2. Add `Batch`, `Kind`, and the equality doc comment so both public test files compile.
3. Replace the v1 `codecs` array with the three table variables. Fill `putCodec`, `deleteCodec`, `batchCodec`, and the three tables in `init`. Do not write a composite literal that names `batchCodec` at package level. Point `codecOf` and `measure` at `encodeTable` only. Point version-1 decode at `decodeV1Table` and version-2 decode, including `decodeBatch`, at `decodeV2Table`.
4. Change `kindCodec` to the pinned signatures. `Put` and `Delete` ignore depth and children. `measure` calls `codecOf` at every depth before any table index.
5. Implement `measure` / `batchBodyLen` so the child tree is built once, with the `maxDepth` check before the walk, no `Records` copy, and the 16 MiB check inside the child loop. `Encode` writes version `2` from that tree. `write` does not measure again. `appendFrame` uses a three-index body slice, and `writeBatch` asserts the written child length.
6. Implement `readFrame` and `decodeBatch`: sub-slice bounds, `maxDepth` check before the sub-stream, `n` pre-check against `remaining/minFrameWire`, append-only results, exact consumption, decoded empty `Batch` as `[]Record{}`. Inner reads use `&decodeV2Table`.
7. Update every doc comment in the table above.
8. Add `walcodec_v2_internal_test.go` as `package walcodec_test` with the `IntV2` / `intv2` prefixes and the cases listed above.
9. Run `go test ./...`, `go vet ./...`, and `go build ./...` from `worktree`. Fix until all three are clean. `go build` succeeding is the check that the `init` fill did not reintroduce the cycle.
