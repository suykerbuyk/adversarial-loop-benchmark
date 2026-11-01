package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a stream produced by Encode (version 2) or by a version-1
// encoder and returns its records in stream order. Decoded records never
// alias b.
//
// Any malformed input — a missing or unknown version byte, a len over 16 MiB,
// a zero len, a truncated frame, a crc mismatch, an unknown type tag (including
// a Batch in a version-1 stream), a payload that does not exactly match its
// type's layout, a Batch whose inner frames overrun or do not exactly fill its
// payload, or a Batch nested more than 8 levels deep — yields (nil,
// ErrCorrupt). Oversize len values are rejected before any allocation.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	var d decoder
	switch b[0] {
	case version1:
		d.allowBatch = false
	case version2:
		d.allowBatch = true
	default:
		return nil, ErrCorrupt
	}
	recs := make([]Record, 0)
	rest := b[1:]
	for len(rest) > 0 {
		rec, r, ok := d.readFrame(rest, 0)
		if !ok {
			return nil, ErrCorrupt
		}
		recs = append(recs, rec)
		rest = r
	}
	return recs, nil
}

// decoder carries per-stream decoding options. It holds no mutable state.
type decoder struct {
	allowBatch bool // true for version 2 streams
}

// readFrame reads one frame from the front of region, which sits at stream
// depth depth.
//
// Contract: region is exactly the enclosing region that frames may occupy —
// the stream remainder at top level, or the unread remainder of the enclosing
// Batch payload when nested. Never pass a larger slice. The returned rest is
// always a suffix of region. An inner len that extends past region is
// reported as ok=false, so a frame can never read into a sibling or an outer
// frame.
func (d decoder) readFrame(region []byte, depth int) (rec Record, rest []byte, ok bool) {
	if len(region) < lenFieldSize {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(region)
	if n > maxFrameLen || n == 0 {
		return nil, nil, false
	}
	// n <= 16 MiB, so int arithmetic cannot overflow even on 32-bit.
	end := lenFieldSize + int(n)
	if len(region) < end+crcFieldSize {
		return nil, nil, false
	}
	// Three-index slice: cap == len, so any accidental re-slice past the
	// frame body panics instead of reading the crc or a sibling frame.
	body := region[lenFieldSize:end:end]
	// Integrity-check the whole body before dispatching on its contents.
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(region[end:]) {
		return nil, nil, false
	}
	rec, ok = d.decodePayload(body[0], body[typeSize:], depth)
	if !ok {
		return nil, nil, false
	}
	return rec, region[end+crcFieldSize:], true
}

// decodePayload parses p as the payload for kind, for a frame that sits at
// stream depth depth. It requires the payload to be consumed exactly;
// trailing bytes or an unknown (or disallowed) kind report ok=false.
func (d decoder) decodePayload(kind uint8, p []byte, depth int) (Record, bool) {
	switch kind {
	case kindPut:
		key, p, ok := readLenPrefixed(p)
		if !ok {
			return nil, false
		}
		val, p, ok := readLenPrefixed(p)
		if !ok || len(p) != 0 {
			return nil, false
		}
		v := make([]byte, len(val))
		copy(v, val)
		return Put{Key: string(key), Value: v}, true
	case kindDelete:
		key, p, ok := readLenPrefixed(p)
		if !ok || len(p) != 0 {
			return nil, false
		}
		return Delete{Key: string(key)}, true
	case kindBatch:
		if !d.allowBatch {
			return nil, false
		}
		return d.decodeBatch(p, depth+1)
	default:
		return nil, false
	}
}

// decodeBatch parses p as a Batch payload whose sub-stream sits at depth
// depth. The count's frames must consume p exactly.
func (d decoder) decodeBatch(p []byte, depth int) (Record, bool) {
	if depth > maxDepth {
		return nil, false
	}
	if len(p) < batchCountSize {
		return nil, false
	}
	count := binary.BigEndian.Uint32(p)
	rest := p[batchCountSize:len(p):len(p)]
	// Reject counts that cannot possibly fit; this also bounds the
	// preallocation below against forged counts.
	if uint64(count) > uint64(len(rest)/minFrameSize) {
		return nil, false
	}
	recs := make([]Record, 0, count)
	for i := uint32(0); i < count; i++ {
		rec, r, ok := d.readFrame(rest, depth)
		if !ok {
			return nil, false
		}
		recs = append(recs, rec)
		rest = r
	}
	if len(rest) != 0 {
		return nil, false
	}
	return Batch{Records: recs}, true
}

// readLenPrefixed reads [uint32 n][n bytes] from p, returning the n bytes (a
// sub-slice of p) and the remainder.
func readLenPrefixed(p []byte) (field, rest []byte, ok bool) {
	if len(p) < lenFieldSize {
		return nil, nil, false
	}
	n := uint64(binary.BigEndian.Uint32(p))
	p = p[lenFieldSize:]
	if n > uint64(len(p)) {
		return nil, nil, false
	}
	return p[:n], p[n:], true
}
