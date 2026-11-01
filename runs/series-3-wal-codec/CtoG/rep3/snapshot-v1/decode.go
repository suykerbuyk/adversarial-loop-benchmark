package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a stream produced by Encode. It returns records in stream
// order as Put and Delete values (never pointers; pointer identity of encoded
// inputs is not preserved). Any malformed input yields a nil slice and exactly
// ErrCorrupt. Decoded keys and values never alias b.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 || b[0] != version1 {
		return nil, ErrCorrupt // missing or unknown version, checked before any frame
	}

	// Pass 1: structural scan of headers only. Rejects truncated, zero and
	// oversize frames before any allocation and yields an exact count that is
	// bounded by the bytes physically present.
	count := 0
	for rest := b[1:]; len(rest) > 0; count++ {
		_, _, tail, ok := nextFrame(rest)
		if !ok {
			return nil, ErrCorrupt
		}
		rest = tail
	}

	// Pass 2: verify crc and parse payloads.
	recs := make([]Record, 0, count)
	for rest := b[1:]; len(rest) > 0; {
		body, sum, tail, _ := nextFrame(rest) // validated by pass 1
		if crc32.ChecksumIEEE(body) != sum {
			return nil, ErrCorrupt
		}
		rec, ok := decodeBody(body[0], body[1:])
		if !ok {
			return nil, ErrCorrupt
		}
		recs = append(recs, rec)
		rest = tail
	}
	return recs, nil
}

// nextFrame splits one frame off the front of rest. Every bounds check on the
// header happens before anything of length n is sliced.
func nextFrame(rest []byte) (body []byte, sum uint32, tail []byte, ok bool) {
	if len(rest) < lenSize {
		return nil, 0, nil, false
	}
	n := binary.BigEndian.Uint32(rest)
	if n < minFrameLen || n > maxFrameLen {
		return nil, 0, nil, false
	}
	avail := rest[lenSize:]
	if uint64(len(avail)) < uint64(n)+crcSize {
		return nil, 0, nil, false
	}
	return avail[:n], binary.BigEndian.Uint32(avail[n:]), avail[n+crcSize:], true
}

// decodeBody parses a verified type+payload into a record. It reports false
// for an unknown type tag or a payload that does not match the layout exactly
// (including trailing bytes).
func decodeBody(typ byte, p []byte) (Record, bool) {
	switch typ {
	case kindPut:
		key, p, ok := readBytes(p)
		if !ok {
			return nil, false
		}
		val, p, ok := readBytes(p)
		if !ok || len(p) != 0 {
			return nil, false
		}
		v := make([]byte, len(val))
		copy(v, val)
		return Put{Key: string(key), Value: v}, true
	case kindDelete:
		key, p, ok := readBytes(p)
		if !ok || len(p) != 0 {
			return nil, false
		}
		return Delete{Key: string(key)}, true
	}
	return nil, false
}

// readBytes reads a [uint32 len][bytes] field from the front of p.
func readBytes(p []byte) (field, rest []byte, ok bool) {
	if len(p) < u32Size {
		return nil, nil, false
	}
	l := binary.BigEndian.Uint32(p)
	p = p[u32Size:]
	if uint64(l) > uint64(len(p)) {
		return nil, nil, false
	}
	return p[:l], p[l:], true
}
