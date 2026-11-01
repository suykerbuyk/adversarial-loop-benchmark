package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// corrupt returns an error wrapping ErrCorrupt with a detail message.
func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// Decode parses a stream produced by Encode and returns its records in stream
// order. Put and Delete are returned as values; a decoded Put's Value is
// always non-nil and never aliases b.
//
// Any malformed input yields an error wrapping ErrCorrupt and nil records:
// a missing or unknown version byte, a truncated frame, a len of zero or
// above 16 MiB, a crc mismatch, an unknown type tag, or a payload that does
// not match its type's field layout exactly (including trailing bytes).
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt("missing version byte")
	}
	if b[0] != version1 {
		return nil, corrupt("unknown version %d", b[0])
	}
	recs := []Record{}
	rest := b[1:]
	for len(rest) > 0 {
		rec, n, err := decodeFrame(rest)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		rest = rest[n:]
	}
	return recs, nil
}

// decodeFrame decodes the frame at the start of p and returns the record and
// the number of bytes consumed.
func decodeFrame(p []byte) (Record, int, error) {
	if len(p) < lenSize {
		return nil, 0, corrupt("truncated frame header")
	}
	n := binary.BigEndian.Uint32(p)
	if n > maxFrameLen { // checked before any length-dependent work
		return nil, 0, corrupt("frame length %d exceeds cap %d", n, maxFrameLen)
	}
	if n == 0 {
		return nil, 0, corrupt("empty frame")
	}
	if uint64(len(p)) < lenSize+uint64(n)+crcSize {
		return nil, 0, corrupt("truncated frame")
	}
	end := lenSize + int(n)
	body := p[lenSize:end]
	if want := binary.BigEndian.Uint32(p[end:]); crc32.ChecksumIEEE(body) != want {
		return nil, 0, corrupt("crc mismatch")
	}
	rec, err := decodePayload(body[0], body[1:])
	if err != nil {
		return nil, 0, err
	}
	return rec, end + crcSize, nil
}

// decodePayload builds a record of type typ from payload, which must match
// the type's field layout exactly.
func decodePayload(typ uint8, payload []byte) (Record, error) {
	switch typ {
	case kindPut:
		k, rest, ok := readField(payload)
		if !ok {
			return nil, corrupt("truncated put key")
		}
		v, rest, ok := readField(rest)
		if !ok {
			return nil, corrupt("truncated put value")
		}
		if len(rest) != 0 {
			return nil, corrupt("trailing bytes in put payload")
		}
		return Put{Key: string(k), Value: append([]byte{}, v...)}, nil
	case kindDelete:
		k, rest, ok := readField(payload)
		if !ok {
			return nil, corrupt("truncated delete key")
		}
		if len(rest) != 0 {
			return nil, corrupt("trailing bytes in delete payload")
		}
		return Delete{Key: string(k)}, nil
	default:
		return nil, corrupt("unknown record type %d", typ)
	}
}

// readField reads a [uint32 len][bytes] field from p, bounds-checking the
// length against the bytes actually present.
func readField(p []byte) (field, rest []byte, ok bool) {
	if len(p) < lenSize {
		return nil, nil, false
	}
	l := binary.BigEndian.Uint32(p)
	if uint64(l) > uint64(len(p)-lenSize) {
		return nil, nil, false
	}
	end := lenSize + int(l)
	return p[lenSize:end], p[end:], true
}
