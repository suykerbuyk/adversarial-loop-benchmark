package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Decode parses a stream produced by Encode. It accepts version 1 (Put and
// Delete only) and version 2 (Put, Delete and Batch); any other version byte
// is rejected before any frame is read. Every failure wraps ErrCorrupt (test
// with errors.Is) and returns a nil slice; the first error at any depth ends
// decoding. On success the result is non-nil (empty for a version-only
// stream).
//
// Decode is strict: it accepts only canonical streams. Each frame, at every
// depth, is checked in this order: header present, len within the 16 MiB cap
// (before any read of the body or any allocation), frame complete within its
// enclosing stream or Batch payload, len non-zero, crc, then type tag and
// payload. A Batch payload is then checked for: nesting depth (a sub-stream
// deeper than 8 is corrupt), count n present, then exactly n inner frames
// bounded by the Batch payload, then no trailing bytes. Returned records never
// alias b.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: missing version byte", ErrCorrupt)
	}
	ver := b[0]
	if ver != version1 && ver != version2 {
		return nil, fmt.Errorf("%w: unknown version %d", ErrCorrupt, ver)
	}

	// make never returns a nil slice, so a version-only stream yields []Record{}.
	recs := make([]Record, 0, min((len(b)-1)/minFrameWire, 1024))
	off := 1
	for off < len(b) {
		kind, payload, next, err := nextFrame(b, off)
		if err != nil {
			return nil, fmt.Errorf("%w at offset %d", err, off)
		}
		rec, err := decodeRecord(kind, payload, ver, 0) // 0 = top-level depth
		if err != nil {
			return nil, fmt.Errorf("%w in frame at offset %d", err, off)
		}
		recs = append(recs, rec)
		off = next
	}
	return recs, nil
}

// nextFrame validates the frame starting at buf[off:], measuring completeness
// against len(buf) only, so a frame can never extend into bytes outside buf.
// On success it returns the type tag, the payload (aliasing buf) and the
// offset just past the frame's crc. Every error wraps ErrCorrupt; on error the
// other results are zero values and must not be used.
func nextFrame(buf []byte, off int) (kind uint8, payload []byte, next int, err error) {
	if len(buf)-off < lenSize {
		return 0, nil, 0, fmt.Errorf("%w: truncated frame header", ErrCorrupt)
	}
	n := binary.BigEndian.Uint32(buf[off:])
	if n > maxFrameLen {
		return 0, nil, 0, fmt.Errorf("%w: frame length %d exceeds cap", ErrCorrupt, n)
	}
	if uint64(len(buf)-off) < lenSize+uint64(n)+crcSize {
		return 0, nil, 0, fmt.Errorf("%w: truncated frame", ErrCorrupt)
	}
	if n == 0 {
		return 0, nil, 0, fmt.Errorf("%w: zero-length frame", ErrCorrupt)
	}
	end := off + lenSize + int(n)
	tp := buf[off+lenSize : end : end]
	if crc32.ChecksumIEEE(tp) != binary.BigEndian.Uint32(buf[end:]) {
		return 0, nil, 0, fmt.Errorf("%w: crc mismatch", ErrCorrupt)
	}
	return tp[0], tp[typeSize:], end + crcSize, nil
}

// decodeRecord parses one frame's payload. depth is the depth of the stream
// the frame sits in. Every error it returns wraps ErrCorrupt.
func decodeRecord(kind uint8, payload []byte, ver byte, depth int) (Record, error) {
	switch {
	case kind == kindPut || kind == kindDelete:
		return decodeLeaf(kind, payload)
	case kind == kindBatch && ver == version2:
		return decodeBatch(payload, ver, depth)
	default: // includes a Batch tag in a version-1 stream
		return nil, fmt.Errorf("%w: unknown record type %d", ErrCorrupt, kind)
	}
}

// decodeBatch parses a Batch payload. depth is the depth of the stream that
// holds the Batch frame; its sub-stream is decoded at depth+1.
func decodeBatch(p []byte, ver byte, depth int) (Record, error) {
	sub := depth + 1
	if sub > maxDepth {
		return nil, fmt.Errorf("%w: batch nesting exceeds depth %d", ErrCorrupt, maxDepth)
	}
	if len(p) < u32Size {
		return nil, fmt.Errorf("%w: truncated batch count", ErrCorrupt)
	}
	n := binary.BigEndian.Uint32(p)
	s := p[u32Size:len(p):len(p)]
	// Each inner frame needs at least minFrameWire bytes; checking this
	// before make bounds the allocation by the payload size.
	if uint64(n) > uint64(len(s)/minFrameWire) {
		return nil, fmt.Errorf("%w: batch count %d exceeds payload", ErrCorrupt, n)
	}
	recs := make([]Record, 0, n) // non-nil even when n == 0
	off := 0
	for i := uint32(0); i < n; i++ {
		kind, pl, next, err := nextFrame(s, off)
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
		return nil, fmt.Errorf("%w: trailing bytes after %d batch records", ErrCorrupt, n)
	}
	return Batch{Records: recs}, nil
}

// decodeLeaf parses a Put or Delete payload. Every error it returns wraps
// ErrCorrupt.
func decodeLeaf(kind uint8, payload []byte) (Record, error) {
	r := reader{buf: payload}
	var rec Record
	switch kind {
	case kindPut:
		key, ok := r.field()
		if !ok {
			return nil, fmt.Errorf("%w: truncated payload", ErrCorrupt)
		}
		val, ok := r.field()
		if !ok {
			return nil, fmt.Errorf("%w: truncated payload", ErrCorrupt)
		}
		v := []byte{}
		if len(val) > 0 {
			v = make([]byte, len(val))
			copy(v, val)
		}
		rec = Put{Key: string(key), Value: v}
	case kindDelete:
		key, ok := r.field()
		if !ok {
			return nil, fmt.Errorf("%w: truncated payload", ErrCorrupt)
		}
		rec = Delete{Key: string(key)}
	default:
		return nil, fmt.Errorf("%w: unknown record type %d", ErrCorrupt, kind)
	}
	if len(r.buf) != 0 {
		return nil, fmt.Errorf("%w: trailing bytes in payload", ErrCorrupt)
	}
	return rec, nil
}

// reader consumes big-endian fields from the front of buf.
type reader struct{ buf []byte }

func (r *reader) u32() (uint32, bool) {
	if len(r.buf) < u32Size {
		return 0, false
	}
	v := binary.BigEndian.Uint32(r.buf)
	r.buf = r.buf[u32Size:]
	return v, true
}

func (r *reader) bytes(n uint32) ([]byte, bool) {
	if uint64(n) > uint64(len(r.buf)) {
		return nil, false
	}
	b := r.buf[:n:n]
	r.buf = r.buf[n:]
	return b, true
}

// field reads a [u32 len][bytes] field. The returned slice aliases the input.
func (r *reader) field() ([]byte, bool) {
	n, ok := r.u32()
	if !ok {
		return nil, false
	}
	return r.bytes(n)
}
