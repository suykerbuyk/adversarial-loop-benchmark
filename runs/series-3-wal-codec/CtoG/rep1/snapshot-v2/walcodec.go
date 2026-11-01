package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	version1    byte  = 1 // Put/Delete only; still accepted by Decode
	version2    byte  = 2 // the version Encode writes; adds Batch
	kindPut     uint8 = 1
	kindDelete  uint8 = 2
	kindBatch   uint8 = 3
	maxFrameLen       = 16 << 20 // cap on a frame's len (type+payload), at every depth
	lenSize           = 4        // frame len field
	crcSize           = 4        // frame crc trailer
	u32Size           = 4        // klen / vlen / batch count prefixes
	maxDepth          = 8        // deepest legal sub-stream; the top level is depth 0

	// frameOverhead is the bytes a frame occupies outside its own len: the
	// len field itself and the trailing crc.
	frameOverhead = lenSize + crcSize

	// minRecordBody is the smallest len (type+payload) of any frame that can
	// decode successfully: every decodable payload starts with at least one
	// 4-byte length or count field (Delete: klen; Put: klen+vlen, so ≥ 9;
	// Batch: n). Used ONLY as a feasibility bound, never as a framing rule.
	minRecordBody = 1 + u32Size

	// minRecordFrame is the smallest full frame on the wire (= 13). Used ONLY
	// as a feasibility bound on a decoded count and an encoder child budget.
	minRecordFrame = frameOverhead + minRecordBody
)

// ErrCorrupt is wrapped by every error Decode returns. Use errors.Is to test
// for it; the returned errors carry additional detail and are not == to it.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a single WAL entry. Kind reports the record's wire tag.
//
// Kind is informational only: having a Kind of 1, 2 or 3 does not make a
// value encodable. Encode accepts only the concrete types Put, *Put, Delete,
// *Delete, Batch and *Batch (pointers non-nil), and dispatches on concrete
// type, never on Kind.
type Record interface{ Kind() uint8 }

// Put records that Key was set to Value.
type Put struct {
	Key   string
	Value []byte
}

// Delete records that Key was removed.
type Delete struct{ Key string }

// Batch groups records into one frame whose payload is a nested framed
// sub-stream. Records may hold Put, Delete and further Batch values, nested
// at most 8 deep.
type Batch struct{ Records []Record }

// Kind returns 1, the wire tag for Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire tag for Delete.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns 3, the wire tag for Batch.
func (Batch) Kind() uint8 { return kindBatch }

// Encode writes recs as a version-2 stream, frames in slice order. An empty
// recs yields the one-byte, version-only stream.
//
// Encode fails, returning a nil slice, if any element (at any depth) is not a
// Put, *Put, Delete, *Delete, Batch or *Batch (including nil elements and nil
// pointers), if Batches nest deeper than 8, or if any frame would exceed the
// 16 MiB cap. These errors start with "walcodec: " and do not wrap ErrCorrupt.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{version2}
	for i, r := range recs {
		// The per-child budgets in appendBatch keep every top-level frame within
		// maxFrameLen+frameOverhead bytes, so no write lands past ceil.
		e := encoder{ceil: len(out) + maxFrameLen + frameOverhead}
		var err error
		if out, err = e.appendFrame(out, r, 0, maxFrameLen); err != nil {
			// The single site that adds the "walcodec: " prefix.
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
	}
	return out, nil
}

// encoder carries per-top-level-frame encode state.
type encoder struct {
	// ceil is the largest len(out) the current top-level frame can reach. It
	// clamps buffer growth so a frame near the cap does not over-allocate.
	ceil int
}

// grow returns out with room for at least n more bytes. Capacity doubles, but
// is clamped to e.ceil so a frame near the cap does not over-allocate. The
// clamp never drops growth below a factor of 1.25: once the stream already
// exceeds the cap (many top-level frames), ceil is only 16 MiB past len(out),
// and an additive 16 MiB step would make encoding a large stream quadratic.
// Growth therefore stays geometric and the total copying stays linear.
func (e encoder) grow(out []byte, n int) []byte {
	need := len(out) + n
	if need <= cap(out) {
		return out
	}
	c := 2 * cap(out)
	if c > e.ceil {
		c = max(e.ceil, cap(out)+cap(out)/4)
	}
	if c < need {
		c = need
	}
	grown := make([]byte, len(out), c)
	copy(grown, out)
	return grown
}

// appendFrame appends r's full frame to out. depth is the depth of the stream
// holding r. limit is the largest len (type+payload) the frame may have:
// maxFrameLen at the top level and less inside a batch. On error it returns
// (nil, err); callers never touch out after an error.
func (e encoder) appendFrame(out []byte, r Record, depth, limit int) ([]byte, error) {
	switch x := r.(type) {
	case Put:
		return e.appendLeaf(out, kindPut, x.Key, x.Value, true, limit)
	case *Put:
		if x != nil {
			return e.appendLeaf(out, kindPut, x.Key, x.Value, true, limit)
		}
	case Delete:
		return e.appendLeaf(out, kindDelete, x.Key, nil, false, limit)
	case *Delete:
		if x != nil {
			return e.appendLeaf(out, kindDelete, x.Key, nil, false, limit)
		}
	case Batch:
		return e.appendBatch(out, x.Records, depth, limit)
	case *Batch:
		if x != nil {
			return e.appendBatch(out, x.Records, depth, limit)
		}
	}
	return nil, fmt.Errorf("unsupported record type %T", r)
}

// appendLeaf appends a Put (hasValue) or Delete frame. The size is checked
// against limit before any byte is appended.
func (e encoder) appendLeaf(out []byte, kind uint8, key string, val []byte, hasValue bool, limit int) ([]byte, error) {
	size := frameBodySize(key, val, hasValue)
	if size > uint64(limit) {
		return nil, fmt.Errorf("encoded size %d exceeds frame cap (%d bytes available)", size, limit)
	}
	// size ≤ limit ≤ maxFrameLen, so int(size) cannot overflow.
	out = e.grow(out, lenSize+int(size)+crcSize)
	start := len(out)
	// size ≤ limit ≤ maxFrameLen, so it and len(key), len(val) fit in uint32.
	out = binary.BigEndian.AppendUint32(out, uint32(size))
	out = append(out, kind)
	out = appendString(out, key)
	if hasValue {
		out = appendBytes(out, val)
	}
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start+lenSize:])), nil
}

// appendBatch appends a Batch frame holding recs. depth is the depth of the
// stream holding the Batch frame; its sub-stream sits at depth+1. Every child
// is given a budget derived from the bytes still free under limit, checked
// before recursing, so the frame never exceeds limit and work stays bounded
// even for cyclic or heavily aliased input.
func (e encoder) appendBatch(out []byte, recs []Record, depth, limit int) ([]byte, error) {
	// Same predicate Decode applies in decoder.batch.
	if depth+1 > maxDepth {
		return nil, fmt.Errorf("batch nesting exceeds depth %d", maxDepth)
	}
	used := minRecordBody // type + count
	if used > limit {
		return nil, errors.New("batch exceeds frame cap")
	}
	// Every child frame needs ≥ minRecordFrame bytes; fail fast if they cannot
	// fit. This also bounds len(recs) for the uint32 cast below.
	if uint64(len(recs)) > uint64(limit-used)/minRecordFrame {
		return nil, errors.New("batch exceeds frame cap")
	}
	out = e.grow(out, lenSize+minRecordBody)
	start := len(out)
	out = append(out, 0, 0, 0, 0, kindBatch)
	// len(recs) ≤ maxFrameLen/13 by the check above, so it fits in uint32.
	out = binary.BigEndian.AppendUint32(out, uint32(len(recs)))
	for j, c := range recs {
		childLimit := limit - used - frameOverhead // largest len the child's frame may have
		if childLimit < minRecordBody {
			// No record of any kind fits in less than minRecordBody of len.
			return nil, fmt.Errorf("batch record %d: batch exceeds frame cap", j)
		}
		before := len(out)
		var err error
		if out, err = e.appendFrame(out, c, depth+1, childLimit); err != nil {
			return nil, fmt.Errorf("batch record %d: %w", j, err)
		}
		used += len(out) - before // the child's FULL frame, ≥ minRecordFrame
	}
	// Invariant: used == len(out)-start-lenSize ≤ limit ≤ maxFrameLen, so the
	// cast is safe.
	binary.BigEndian.PutUint32(out[start:], uint32(used))
	out = e.grow(out, crcSize)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start+lenSize:])), nil
}

// frameBodySize returns the size of type+payload, computed in uint64 so that
// no length is narrowed before the cap check.
func frameBodySize(key string, val []byte, hasValue bool) uint64 {
	n := uint64(1) + u32Size + uint64(len(key))
	if hasValue {
		n += u32Size + uint64(len(val))
	}
	return n
}

// appendString appends s as [uint32 len][bytes]. The caller guarantees
// len(s) fits in uint32.
func appendString(dst []byte, s string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

// appendBytes appends p as [uint32 len][bytes]. The caller guarantees
// len(p) fits in uint32.
func appendBytes(dst []byte, p []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(p)))
	return append(dst, p...)
}

// Decode parses a version-1 or version-2 stream and returns its records in
// stream order. Decoded records never alias b; decoded Batches always have a
// non-nil Records slice.
//
// Every failure (missing or unknown version byte, truncated frame, oversize
// or empty len, crc mismatch, unknown type, malformed payload, a Batch in a
// version-1 stream, a Batch whose frames do not consume its payload exactly,
// nesting deeper than 8) returns a nil slice and an error wrapping ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt("missing version byte")
	}
	if b[0] != version1 && b[0] != version2 {
		return nil, corrupt("unknown version %d", b[0])
	}
	d := decoder{allowBatch: b[0] == version2}
	// Non-nil even when empty: a local choice so an empty stream reads as
	// "empty list" rather than "absent".
	recs := []Record{}
	for rest, off := b[1:], 1; len(rest) > 0; {
		rec, r, err := d.frame(rest, off, 0) // the top-level stream is depth 0
		if err != nil {
			return nil, err
		}
		off += len(rest) - len(r)
		recs, rest = append(recs, rec), r
	}
	return recs, nil
}

// decoder carries per-stream decode settings.
type decoder struct {
	allowBatch bool // false for version-1 streams
}

// frame reads one frame from the front of region. region is a stream at the
// given depth; off is the absolute offset of region[0] in the caller's buffer.
// It returns the record and the bytes after the frame.
func (d decoder) frame(region []byte, off, depth int) (Record, []byte, error) {
	if len(region) < lenSize {
		return nil, nil, corruptAt(off, depth, "truncated frame header")
	}
	n := binary.BigEndian.Uint32(region)
	if n > maxFrameLen {
		return nil, nil, corruptAt(off, depth, "frame len %d exceeds 16 MiB cap", n)
	}
	if n == 0 {
		return nil, nil, corruptAt(off, depth, "empty frame")
	}
	// At depth ≥ 1, region is exactly the unread tail of the enclosing Batch
	// payload, so this is also the amendment's "inner len past enclosing
	// payload" case: it never consults bytes of a sibling or outer frame.
	if rem := len(region) - lenSize; uint64(rem) < uint64(n)+crcSize {
		return nil, nil, corruptAt(off, depth,
			"truncated frame: len %d needs %d bytes, %d remain in region", n, uint64(n)+crcSize, rem)
	}
	end := lenSize + int(n)
	body := region[lenSize:end]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(region[end:]) {
		return nil, nil, corruptAt(off, depth, "crc mismatch")
	}
	rec, err := d.payload(body[0], body[1:], off, depth)
	if err != nil {
		return nil, nil, err
	}
	return rec, region[end+crcSize:], nil
}

// payload parses a crc-verified payload of the given type. off is the
// absolute offset of the holding frame. Parsing is strict: trailing bytes are
// an error, so a successful Decode is canonical.
func (d decoder) payload(kind uint8, p []byte, off, depth int) (Record, error) {
	switch kind {
	case kindPut:
		key, p, ok := readBytes(p)
		if !ok {
			return nil, corruptAt(off, depth, "malformed put payload")
		}
		val, p, ok := readBytes(p)
		if !ok || len(p) != 0 {
			return nil, corruptAt(off, depth, "malformed put payload")
		}
		v := make([]byte, len(val))
		copy(v, val)
		return Put{Key: string(key), Value: v}, nil
	case kindDelete:
		key, p, ok := readBytes(p)
		if !ok || len(p) != 0 {
			return nil, corruptAt(off, depth, "malformed delete payload")
		}
		return Delete{Key: string(key)}, nil
	case kindBatch:
		if !d.allowBatch {
			return nil, corruptAt(off, depth, "batch record in version-1 stream")
		}
		return d.batch(p, off, depth)
	default:
		return nil, corruptAt(off, depth, "unknown record type %d", kind)
	}
}

// batch decodes the payload p of a Batch frame. The frame is at absolute
// offset off in a stream at the given depth; its sub-stream is at depth+1.
func (d decoder) batch(p []byte, off, depth int) (Record, error) {
	// Same predicate Encode applies in appendBatch.
	if depth+1 > maxDepth {
		return nil, corruptAt(off, depth, "batch sub-stream depth %d exceeds %d", depth+1, maxDepth)
	}
	if len(p) < u32Size {
		return nil, corruptAt(off, depth, "truncated batch count")
	}
	n := binary.BigEndian.Uint32(p)
	p = p[u32Size:]
	sub := off + lenSize + 1 + u32Size // absolute offset of inner frame 1
	// Every inner frame takes ≥ minRecordFrame bytes; this bounds the hint.
	if uint64(n) > uint64(len(p))/minRecordFrame {
		return nil, corruptAt(off, depth, "batch count %d cannot fit in %d bytes", n, len(p))
	}
	recs := make([]Record, 0, n)
	for i := uint32(0); i < n; i++ {
		rec, r, err := d.frame(p, sub, depth+1)
		if err != nil {
			return nil, err
		}
		sub += len(p) - len(r)
		p = r
		recs = append(recs, rec)
	}
	if len(p) != 0 {
		return nil, corruptAt(sub, depth+1, "%d trailing bytes after %d batch records", len(p), n)
	}
	return Batch{Records: recs}, nil
}

// readBytes reads a bounds-checked [uint32 len][bytes] field from p.
func readBytes(p []byte) (field, rest []byte, ok bool) {
	if len(p) < u32Size {
		return nil, nil, false
	}
	l := binary.BigEndian.Uint32(p)
	if uint64(l) > uint64(len(p)-u32Size) {
		return nil, nil, false
	}
	end := u32Size + int(l)
	return p[u32Size:end], p[end:], true
}

// corrupt returns an error wrapping ErrCorrupt with the given detail. It is
// used only for the stream-level checks that precede any frame.
func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// corruptAt reports a failure at absolute offset off in a stream at the given
// depth: "walcodec: corrupt stream: <detail> at offset <off> (depth <depth>)".
func corruptAt(off, depth int, format string, args ...any) error {
	return fmt.Errorf("%w: %s at offset %d (depth %d)", ErrCorrupt, fmt.Sprintf(format, args...), off, depth)
}
