package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Decode parses a stream produced by Encode. Every failure wraps ErrCorrupt
// (test with errors.Is) and returns a nil slice. On success the result is
// non-nil (empty for a version-only stream).
//
// Decode is strict: it accepts only canonical streams. Each frame is checked
// in this order: header present, len within the 16 MiB cap (before any read
// of the body or any allocation), frame complete, len non-zero, crc, then type
// tag and payload. Returned records never alias b.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: missing version byte", ErrCorrupt)
	}
	if b[0] != version1 {
		return nil, fmt.Errorf("%w: unknown version %d", ErrCorrupt, b[0])
	}

	recs := make([]Record, 0, min((len(b)-1)/13, 1024))
	off := 1
	for off < len(b) {
		if len(b)-off < lenSize {
			return nil, fmt.Errorf("%w: truncated frame header at offset %d", ErrCorrupt, off)
		}
		n := binary.BigEndian.Uint32(b[off:])
		if n > maxFrameLen {
			return nil, fmt.Errorf("%w: frame length %d exceeds cap at offset %d", ErrCorrupt, n, off)
		}
		if uint64(len(b)-off) < lenSize+uint64(n)+crcSize {
			return nil, fmt.Errorf("%w: truncated frame at offset %d", ErrCorrupt, off)
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: zero-length frame at offset %d", ErrCorrupt, off)
		}
		end := off + lenSize + int(n)
		tp := b[off+lenSize : end]
		if crc32.ChecksumIEEE(tp) != binary.BigEndian.Uint32(b[end:]) {
			return nil, fmt.Errorf("%w: crc mismatch in frame at offset %d", ErrCorrupt, off)
		}
		rec, err := decodePayload(tp[0], tp[typeSize:])
		if err != nil {
			return nil, fmt.Errorf("%w in frame at offset %d", err, off)
		}
		recs = append(recs, rec)
		off = end + crcSize
	}
	return recs, nil
}

// decodePayload parses the payload of a frame with the given type tag. Every
// error it returns wraps ErrCorrupt.
func decodePayload(kind uint8, payload []byte) (Record, error) {
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
