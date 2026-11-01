package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a stream produced by Encode and returns its records in stream
// order. Decoded records never alias b.
//
// Any malformed input — a missing or unknown version byte, a len over 16 MiB,
// a zero len, a truncated frame, a crc mismatch, an unknown type tag, or a
// payload that does not exactly match its type's layout — yields (nil,
// ErrCorrupt). Oversize len values are rejected before any allocation.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 || b[0] != version1 {
		return nil, ErrCorrupt
	}
	recs := make([]Record, 0)
	rest := b[1:]
	for len(rest) > 0 {
		if len(rest) < lenFieldSize {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(rest)
		if n > maxFrameLen || n == 0 {
			return nil, ErrCorrupt
		}
		rest = rest[lenFieldSize:]
		// n <= 16 MiB, so int arithmetic cannot overflow even on 32-bit.
		if len(rest) < int(n)+crcFieldSize {
			return nil, ErrCorrupt
		}
		body := rest[:n]
		if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(rest[n:]) {
			return nil, ErrCorrupt
		}
		rec, ok := decodePayload(body[0], body[typeSize:])
		if !ok {
			return nil, ErrCorrupt
		}
		recs = append(recs, rec)
		rest = rest[int(n)+crcFieldSize:]
	}
	return recs, nil
}

// decodePayload parses p as the payload for kind. It requires the payload to
// be consumed exactly; trailing bytes or an unknown kind report ok=false.
func decodePayload(kind uint8, p []byte) (Record, bool) {
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
	default:
		return nil, false
	}
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
