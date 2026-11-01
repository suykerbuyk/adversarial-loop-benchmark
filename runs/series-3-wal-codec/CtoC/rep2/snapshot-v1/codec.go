package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// --- encoding ---

// fieldsWithinCap reports whether every variable-length field of r is at most
// maxFrameLen bytes, so size sums over them cannot overflow int. Unsupported
// types report true; payloadLen rejects them first.
func fieldsWithinCap(r Record) bool {
	switch r := r.(type) {
	case Put:
		return len(r.Key) <= maxFrameLen && len(r.Value) <= maxFrameLen
	case Delete:
		return len(r.Key) <= maxFrameLen
	}
	return true
}

// payloadLen reports the payload size of r, and whether r is a supported record.
func payloadLen(r Record) (int, bool) {
	switch r := r.(type) {
	case Put:
		return u32Size + len(r.Key) + u32Size + len(r.Value), true
	case Delete:
		return u32Size + len(r.Key), true
	}
	return 0, false
}

// appendPayload appends r's payload. r must have passed payloadLen.
func appendPayload(dst []byte, r Record) []byte {
	switch r := r.(type) {
	case Put:
		dst = appendBytes(dst, r.Key)
		return appendBytes(dst, r.Value)
	case Delete:
		return appendBytes(dst, r.Key)
	}
	return dst // unreachable after payloadLen succeeded
}

// appendBytes appends s as [uint32 len][bytes].
func appendBytes[T string | []byte](dst []byte, s T) []byte {
	dst = appendU32(dst, uint32(len(s)))
	return append(dst, s...)
}

func appendU32(dst []byte, x uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, x)
}

// --- decoding ---

// byteReader is a bounds-checked cursor over a byte slice. It never panics.
type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) remaining() int { return len(r.b) - r.off }

// take returns the next n bytes as a sub-slice of the input, or false if
// fewer than n bytes remain.
func (r *byteReader) take(n uint32) ([]byte, bool) {
	if uint64(n) > uint64(r.remaining()) {
		return nil, false
	}
	s := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return s, true
}

func (r *byteReader) u32() (uint32, bool) {
	s, ok := r.take(4)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint32(s), true
}

// decodeV1Frames decodes the frames that follow a version-1 byte.
func decodeV1Frames(p []byte) ([]Record, error) {
	r := byteReader{b: p}
	var recs []Record
	for r.remaining() > 0 {
		n, ok := r.u32()
		if !ok {
			return nil, ErrCorrupt // truncated header
		}
		if n == 0 || n > maxFrameLen {
			return nil, ErrCorrupt // no type byte, or oversize: rejected before reading
		}
		body, ok := r.take(n)
		if !ok {
			return nil, ErrCorrupt
		}
		sum, ok := r.u32()
		if !ok {
			return nil, ErrCorrupt
		}
		if crc32.ChecksumIEEE(body) != sum {
			return nil, ErrCorrupt
		}
		var rec Record
		switch body[0] {
		case kindPut:
			rec, ok = decodePut(body[1:])
		case kindDelete:
			rec, ok = decodeDelete(body[1:])
		default:
			ok = false
		}
		if !ok {
			return nil, ErrCorrupt
		}
		recs = append(recs, rec)
	}
	if recs == nil {
		recs = []Record{}
	}
	return recs, nil
}

// readField reads a [uint32 len][bytes] field.
func readField(r *byteReader) ([]byte, bool) {
	n, ok := r.u32()
	if !ok {
		return nil, false
	}
	return r.take(n)
}

// decodePut decodes a Put payload, which must be consumed exactly.
func decodePut(p []byte) (Record, bool) {
	r := byteReader{b: p}
	k, ok := readField(&r)
	if !ok {
		return nil, false
	}
	v, ok := readField(&r)
	if !ok || r.remaining() != 0 {
		return nil, false
	}
	return Put{Key: string(k), Value: append(make([]byte, 0, len(v)), v...)}, true
}

// decodeDelete decodes a Delete payload, which must be consumed exactly.
func decodeDelete(p []byte) (Record, bool) {
	r := byteReader{b: p}
	k, ok := readField(&r)
	if !ok || r.remaining() != 0 {
		return nil, false
	}
	return Delete{Key: string(k)}, true
}
