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

// Decode parses a stream produced by Encode (version 2) or by a version-1
// encoder and returns its records in stream order. Records are returned as
// values; a decoded Put's Value is always non-nil and never aliases b.
//
// Any malformed input yields an error wrapping ErrCorrupt and nil records:
// a missing or unknown version byte, a truncated frame, a len of zero or
// above 16 MiB, a crc mismatch, an unknown type tag (including a Batch in a
// version-1 stream), a payload that does not match its type's layout
// exactly, or Batch nesting deeper than 8.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt("missing version byte")
	}
	ver := b[0]
	if ver != version1 && ver != version2 {
		return nil, corrupt("unknown version %d", ver)
	}
	recs := []Record{}
	rest := b[1:]
	for len(rest) > 0 {
		rec, n, err := decodeFrame(rest, ver, 0)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		rest = rest[n:]
	}
	return recs, nil
}

// decodeFrame decodes the frame at the start of p, found in a sub-stream at
// the given depth, and returns the record and the number of bytes consumed.
// p is always bounded by the enclosing payload, so a frame can never read
// past it.
func decodeFrame(p []byte, ver uint8, depth int) (Record, int, error) {
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
	rec, err := decodePayload(body[0], body[1:], ver, depth)
	if err != nil {
		return nil, 0, err
	}
	return rec, end + crcSize, nil
}

// decodePayload builds a record of type typ from payload, which must match
// the type's field layout exactly. depth is that of the stream holding the
// frame.
func decodePayload(typ uint8, payload []byte, ver uint8, depth int) (Record, error) {
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
	case kindBatch:
		if ver == version1 {
			return nil, corrupt("batch record in version-1 stream")
		}
		return decodeBatch(payload, ver, depth+1)
	default:
		return nil, corrupt("unknown record type %d", typ)
	}
}

// decodeBatch parses a Batch payload as a sub-stream at the given depth. The
// n inner frames must consume the payload exactly.
func decodeBatch(payload []byte, ver uint8, depth int) (Record, error) {
	if depth > maxDepth {
		return nil, corrupt("batch nesting exceeds depth %d", maxDepth)
	}
	if len(payload) < countSize {
		return nil, corrupt("truncated batch count")
	}
	n := binary.BigEndian.Uint32(payload)
	rest := payload[countSize:]
	// Every frame takes at least minFrameSize bytes, so this bounds the
	// allocation below by the input size.
	if uint64(n)*minFrameSize > uint64(len(rest)) {
		return nil, corrupt("batch count %d exceeds payload", n)
	}
	recs := make([]Record, 0, n)
	for i := uint32(0); i < n; i++ {
		rec, k, err := decodeFrame(rest, ver, depth)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		rest = rest[k:]
	}
	if len(rest) != 0 {
		return nil, corrupt("trailing bytes after %d batch records", n)
	}
	return Batch{Records: recs}, nil
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
