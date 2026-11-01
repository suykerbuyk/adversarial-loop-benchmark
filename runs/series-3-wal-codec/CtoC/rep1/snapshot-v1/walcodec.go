// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len is a uint32 counting type+payload only; the trailing crc is outside len.
// crc is the CRC32-IEEE checksum of type+payload. len is capped at 16 MiB
// (16777216, inclusive). This version writes and accepts only version 1.
//
// Payloads:
//
//	Put    (type 1) = klen(4) key vlen(4) value
//	Delete (type 2) = klen(4) key
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	kindPut    uint8 = 1
	kindDelete uint8 = 2

	version1    = 1        // the only version written and accepted
	maxFrameLen = 16 << 20 // inclusive cap on the frame len field (type+payload)
	lenSize     = 4        // size of the len field
	crcSize     = 4        // size of the trailing crc (outside len)
	u32Size     = 4        // size of each klen/vlen prefix
)

// ErrCorrupt is wrapped by every error returned from Decode.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a single log entry. Kind returns the record's wire type tag.
//
// Only the value types Put and Delete are encodable. *Put and *Delete also
// satisfy Record through Go's method-set rules, but Encode rejects them (and any
// other implementation) because Decode always returns values, and accepting
// other forms would break round-trip equality.
type Record interface{ Kind() uint8 }

// Put records that Key was set to Value. A nil and an empty Value are
// equivalent: both encode with a zero length and decode to a length-0 slice.
// Pass Put by value, not as *Put, to Encode.
type Put struct {
	Key   string
	Value []byte
}

// Delete records that Key was removed. Pass Delete by value, not as *Delete,
// to Encode.
type Delete struct{ Key string }

// Kind returns 1, the wire type tag of Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire type tag of Delete.
func (Delete) Kind() uint8 { return kindDelete }

// Encode writes recs as a version-1 stream, one frame per record in slice
// order. Only Put and Delete values are accepted (not pointers). Encode returns
// nil and an error if an element is nil, is of any other type, or would produce
// a frame whose len exceeds 16 MiB. These errors are descriptive only: they do
// not wrap ErrCorrupt and there is no sentinel to match with errors.Is.
// No partial output is ever returned.
func Encode(recs []Record) ([]byte, error) {
	total := 0
	for i, r := range recs {
		frameLen, err := frameSize(i, r)
		if err != nil {
			return nil, err
		}
		frameTotal := lenSize + frameLen + crcSize
		// Reachable only on 32-bit platforms, where many records sharing large
		// backing arrays could overflow int; on 64-bit the input cannot exist.
		if total > math.MaxInt-1-frameTotal {
			return nil, errors.New("walcodec: encoded stream too large")
		}
		total += frameTotal
	}

	buf := make([]byte, 0, 1+total)
	buf = append(buf, version1)
	for _, r := range recs {
		buf = appendFrame(buf, r)
	}
	return buf, nil
}

// frameSize validates record i and returns its frame len (type+payload).
func frameSize(i int, r Record) (int, error) {
	tooLarge := func() (int, error) {
		return 0, fmt.Errorf("walcodec: record %d: frame length exceeds limit %d", i, maxFrameLen)
	}
	var payloadLen int
	switch x := r.(type) {
	case Put:
		// Per-field checks first so the sum below cannot overflow int.
		if len(x.Key) > maxFrameLen || len(x.Value) > maxFrameLen {
			return tooLarge()
		}
		payloadLen = u32Size + len(x.Key) + u32Size + len(x.Value)
	case Delete:
		if len(x.Key) > maxFrameLen {
			return tooLarge()
		}
		payloadLen = u32Size + len(x.Key)
	case nil:
		return 0, fmt.Errorf("walcodec: record %d: nil record", i)
	default:
		return 0, fmt.Errorf("walcodec: record %d: unsupported record type %T", i, r)
	}
	frameLen := 1 + payloadLen
	if frameLen > maxFrameLen {
		return tooLarge()
	}
	return frameLen, nil
}

// appendFrame appends the complete frame for r, which frameSize has already
// validated. The type tag and payload are written from the same switch arm,
// and len is back-patched from the bytes actually written, so neither can
// drift from the payload.
func appendFrame(buf []byte, r Record) []byte {
	start := len(buf)
	buf = append(buf, 0, 0, 0, 0) // len placeholder
	switch x := r.(type) {
	case Put:
		buf = append(buf, kindPut)
		buf = appendField(buf, x.Key)
		buf = appendField(buf, x.Value)
	case Delete:
		buf = append(buf, kindDelete)
		buf = appendField(buf, x.Key)
	default:
		panic(fmt.Sprintf("walcodec: internal error: unvalidated record type %T", r))
	}
	body := buf[start+lenSize:]
	binary.BigEndian.PutUint32(buf[start:], uint32(len(body)))
	return binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(body))
}

// appendField appends a [uint32 len][bytes] field. The caller guarantees
// len(f) fits in a uint32.
func appendField[T string | []byte](buf []byte, f T) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(f)))
	return append(buf, f...)
}

// Decode parses a stream produced by Encode and returns its records in stream
// order as Put and Delete values. Every error wraps ErrCorrupt; on error no
// records are returned. The returned records do not alias b.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt("missing version byte")
	}
	if b[0] != version1 {
		return nil, corrupt("unknown version %d", b[0])
	}
	out := make([]Record, 0)
	off := 1
	for off < len(b) {
		rem := len(b) - off
		if rem < lenSize {
			return nil, corrupt("truncated frame header at offset %d", off)
		}
		n := binary.BigEndian.Uint32(b[off:])
		if n > maxFrameLen {
			return nil, corrupt("frame length %d at offset %d exceeds limit %d", n, off, maxFrameLen)
		}
		if n == 0 {
			return nil, corrupt("zero-length frame at offset %d", off)
		}
		if uint64(n)+lenSize+crcSize > uint64(rem) {
			return nil, corrupt("truncated frame at offset %d", off)
		}
		end := off + lenSize + int(n)
		body := b[off+lenSize : end]
		if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(b[end:]) {
			return nil, corrupt("crc mismatch at offset %d", off)
		}
		rec, err := decodePayload(body[0], body[1:])
		if err != nil {
			return nil, corrupt("%v in frame at offset %d", err, off)
		}
		out = append(out, rec)
		off = end + crcSize
	}
	return out, nil
}

// decodePayload decodes the payload p of a frame with type tag kind. Its
// errors are plain details; Decode wraps them with ErrCorrupt and the frame
// offset so every Decode error has the same shape.
func decodePayload(kind uint8, p []byte) (Record, error) {
	switch kind {
	case kindPut:
		key, rest, err := readField(p, "key")
		if err != nil {
			return nil, err
		}
		value, rest, err := readField(rest, "value")
		if err != nil {
			return nil, err
		}
		if len(rest) != 0 {
			return nil, fmt.Errorf("%d trailing bytes in put payload", len(rest))
		}
		// Copy so the result does not alias the input; a zero-length value
		// decodes to a non-nil, empty slice.
		v := make([]byte, len(value))
		copy(v, value)
		return Put{Key: string(key), Value: v}, nil
	case kindDelete:
		key, rest, err := readField(p, "key")
		if err != nil {
			return nil, err
		}
		if len(rest) != 0 {
			return nil, fmt.Errorf("%d trailing bytes in delete payload", len(rest))
		}
		return Delete{Key: string(key)}, nil
	default:
		return nil, fmt.Errorf("unknown record type %d", kind)
	}
}

// readField reads a [uint32 len][bytes] field from p and returns the field
// bytes (a sub-slice of p, not a copy) plus the remainder of p.
func readField(p []byte, name string) ([]byte, []byte, error) {
	if len(p) < u32Size {
		return nil, nil, fmt.Errorf("truncated %s length", name)
	}
	n := binary.BigEndian.Uint32(p)
	p = p[u32Size:]
	if uint64(n) > uint64(len(p)) {
		return nil, nil, fmt.Errorf("%s length %d exceeds remaining %d bytes", name, n, len(p))
	}
	return p[:n], p[n:], nil
}

// corrupt returns an error wrapping ErrCorrupt with a formatted detail.
func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}
