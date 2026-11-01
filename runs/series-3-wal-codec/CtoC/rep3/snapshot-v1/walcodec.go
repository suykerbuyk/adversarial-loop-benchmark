// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// A stream is a single version byte followed by zero or more frames. All
// integers are big-endian:
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len counts type+payload only; crc is CRC32-IEEE over type+payload. len is
// capped at 16 MiB. Encode writes version 1, the only version Decode accepts.
//
// # Errors
//
// Every Decode failure wraps ErrCorrupt with context (such as the byte offset
// of the offending frame). Test for it with errors.Is(err, ErrCorrupt), not
// with ==; the returned error is never the bare sentinel.
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	version1    = 1        // the only version this decoder knows; Encode writes it
	maxFrameLen = 16 << 20 // inclusive cap on the len field (type+payload)
	lenSize     = 4
	crcSize     = 4
	kindPut     = 1
	kindDelete  = 2
)

// Record is a single log entry. Kind returns the record's wire type tag.
type Record interface{ Kind() uint8 }

// Put sets Key to Value. A nil and an empty Value are encoded identically.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes Key.
type Delete struct{ Key string }

// Kind returns the Put type tag, 1.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag, 2.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is wrapped by every error Decode returns for a malformed stream.
// Match it with errors.Is(err, ErrCorrupt); a direct == comparison never
// succeeds because the error carries additional context.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnsupportedRecord = errors.New("unsupported record type")
	errRecordTooLarge    = fmt.Errorf("record exceeds %d-byte frame cap", maxFrameLen)
)

// Encode writes recs as a version-1 stream, in order.
//
// Only Put and Delete values are accepted; nil records, pointer records and
// other Record implementations are rejected, as is any record whose frame would
// exceed the 16 MiB cap. On error Encode returns a nil slice and a non-nil
// error; callers should treat any such error as "this input cannot be encoded"
// and not match on its text.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{version1}
	for i, r := range recs {
		var err error
		out, err = appendFrame(out, r)
		if err != nil {
			return nil, fmt.Errorf("walcodec: encode record %d: %w", i, err)
		}
	}
	return out, nil
}

// appendFrame appends the frame for r to dst. Nothing is appended on error.
// The wire tag is taken from r.Kind(), so the tag has a single source of truth.
func appendFrame(dst []byte, r Record) ([]byte, error) {
	var (
		start int
		err   error
	)
	switch rec := r.(type) {
	case Put:
		dst, start, err = beginFrame(dst, rec.Kind(), 8+uint64(len(rec.Key))+uint64(len(rec.Value)))
		if err != nil {
			return dst, err
		}
		dst = appendField(dst, rec.Key)
		dst = appendField(dst, rec.Value)
		return finishFrame(dst, start), nil
	case Delete:
		dst, start, err = beginFrame(dst, rec.Kind(), 4+uint64(len(rec.Key)))
		if err != nil {
			return dst, err
		}
		dst = appendField(dst, rec.Key)
		return finishFrame(dst, start), nil
	default:
		return dst, errUnsupportedRecord
	}
}

// beginFrame checks the frame size against the cap, reserves capacity for the
// whole frame, and appends the len and type fields. It returns the offset at
// which the frame starts. Nothing is appended on error.
func beginFrame(dst []byte, tag uint8, payloadLen uint64) ([]byte, int, error) {
	n := 1 + payloadLen
	if n > maxFrameLen {
		return dst, 0, errRecordTooLarge
	}
	// n <= 16 MiB, so the frame size fits in int even on 32-bit platforms.
	dst = grow(dst, lenSize+int(n)+crcSize)
	start := len(dst)
	dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	return append(dst, tag), start, nil
}

// grow returns dst with room for at least extra more bytes. When it must
// reallocate it makes exactly one allocation (at least doubling the capacity,
// so appending many small frames stays amortised O(n)). It is written out
// rather than using slices.Grow because slices.Grow's temporary is only elided
// in optimised, uninstrumented builds; under -race, -asan or -gcflags=-N it
// allocates the requested size twice.
func grow(dst []byte, extra int) []byte {
	if cap(dst)-len(dst) >= extra {
		return dst
	}
	nb := make([]byte, len(dst), max(2*cap(dst), len(dst)+extra))
	copy(nb, dst)
	return nb
}

// appendField appends a [u32 len][bytes] field. The caller has already bounded
// len(b) by the frame cap, so the uint32 conversion cannot truncate.
func appendField[T string | []byte](dst []byte, b T) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(b)))
	return append(dst, b...)
}

// finishFrame appends the CRC over the type+payload of the frame at start.
func finishFrame(dst []byte, start int) []byte {
	return binary.BigEndian.AppendUint32(dst, crc32.ChecksumIEEE(dst[start+lenSize:]))
}

// Decode parses a stream produced by Encode. Every failure wraps ErrCorrupt,
// and on failure no records are returned. A version-only stream decodes to an
// empty, non-nil slice.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt(0, "missing version byte")
	}
	// The version is validated before any frame is examined.
	switch b[0] {
	case version1:
		return decodeFramesV1(b, 1)
	default:
		return nil, corrupt(0, "unknown version %d", b[0])
	}
}

func decodeFramesV1(b []byte, off int) ([]Record, error) {
	recs := []Record{}
	for off < len(b) {
		rest := b[off:]
		if len(rest) < lenSize {
			return nil, corrupt(off, "truncated frame header")
		}
		n := binary.BigEndian.Uint32(rest)
		if n == 0 {
			return nil, corrupt(off, "zero-length frame")
		}
		if n > maxFrameLen {
			return nil, corrupt(off, "frame len %d exceeds cap %d", n, maxFrameLen)
		}
		need := lenSize + int(n) + crcSize // n <= 16 MiB: no overflow even on 32-bit
		if len(rest) < need {
			return nil, corrupt(off, "truncated frame: need %d bytes, have %d", need, len(rest))
		}
		body := rest[lenSize : lenSize+int(n)]
		want := binary.BigEndian.Uint32(rest[lenSize+int(n) : need])
		if crc32.ChecksumIEEE(body) != want {
			return nil, corrupt(off, "crc mismatch")
		}
		r, err := decodeRecordV1(body[0], body[1:], off)
		if err != nil {
			return nil, err
		}
		recs = append(recs, r)
		off += need
	}
	return recs, nil
}

func decodeRecordV1(tag byte, p []byte, off int) (Record, error) {
	switch tag {
	case kindPut:
		key, rest, ok := readField(p)
		if !ok {
			return nil, corrupt(off, "malformed put payload")
		}
		val, rest, ok := readField(rest)
		if !ok || len(rest) != 0 {
			return nil, corrupt(off, "malformed put payload")
		}
		return Put{Key: string(key), Value: append([]byte{}, val...)}, nil
	case kindDelete:
		key, rest, ok := readField(p)
		if !ok || len(rest) != 0 {
			return nil, corrupt(off, "malformed delete payload")
		}
		return Delete{Key: string(key)}, nil
	default:
		return nil, corrupt(off, "unknown record type %d", tag)
	}
}

// readField reads a [u32 len][bytes] field from p, returning sub-slices.
func readField(p []byte) (field, rest []byte, ok bool) {
	if len(p) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(p)
	if uint64(n) > uint64(len(p)-4) {
		return nil, nil, false
	}
	end := 4 + int(n)
	return p[4:end], p[end:], true
}

// corrupt returns an error wrapping ErrCorrupt with context; off is the byte
// offset of the frame (or 0 for stream-level errors).
func corrupt(off int, format string, args ...any) error {
	return fmt.Errorf("%w: %s (offset %d)", ErrCorrupt, fmt.Sprintf(format, args...), off)
}
