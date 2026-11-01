package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ErrCorrupt is returned for any stream Decode cannot accept: a missing or
// unknown version, a truncated frame, an oversize length, a CRC mismatch, an
// unknown type tag, or a payload that does not match its length prefixes.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1

	kindPut    = 1
	kindDelete = 2

	// maxFrameLen is the inclusive cap on the frame len field (type + payload).
	maxFrameLen = 16 << 20 // 16 MiB

	lenSize = 4
	crcSize = 4
)

// Record is one write-ahead-log entry. Concrete types return their wire tag
// from Kind.
type Record interface {
	Kind() uint8
}

// Put is a key/value write. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes a key. Kind is 2.
type Delete struct {
	Key string
}

// Kind returns the Put wire tag. A value receiver so both Put and *Put
// satisfy Record.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete wire tag. A value receiver so both Delete and
// *Delete satisfy Record.
func (Delete) Kind() uint8 { return kindDelete }

// Encode writes a version-1 stream: a version byte, then one frame per record
// in slice order. A nil or empty slice encodes as the version byte alone.
//
// A nil element, an unknown concrete type, a length that does not fit in
// uint32, or a frame that would exceed maxFrameLen returns (nil, error).
func Encode(recs []Record) ([]byte, error) {
	// Version byte plus a modest per-record overhead; append grows as needed.
	out := make([]byte, 0, 1+len(recs)*16)
	out = append(out, versionV1)

	for i, rec := range recs {
		if rec == nil {
			return nil, fmt.Errorf("walcodec: nil record at index %d", i)
		}
		var (
			typ     byte
			payload []byte
			err     error
		)
		switch r := rec.(type) {
		case Put:
			typ = kindPut
			payload, err = putPayload(r)
		case *Put:
			if r == nil {
				return nil, fmt.Errorf("walcodec: nil record at index %d", i)
			}
			typ = kindPut
			payload, err = putPayload(*r)
		case Delete:
			typ = kindDelete
			payload, err = deletePayload(r)
		case *Delete:
			if r == nil {
				return nil, fmt.Errorf("walcodec: nil record at index %d", i)
			}
			typ = kindDelete
			payload, err = deletePayload(*r)
		default:
			return nil, fmt.Errorf("walcodec: unknown record type %T", rec)
		}
		if err != nil {
			return nil, err
		}
		out, err = appendFrame(out, typ, payload)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func putPayload(r Put) ([]byte, error) {
	keyLen := len(r.Key)
	valLen := len(r.Value) // nil and empty both report 0
	if !fitsU32(keyLen) || !fitsU32(valLen) {
		return nil, fmt.Errorf("walcodec: key or value length exceeds uint32")
	}
	payload := make([]byte, 0, lenSize+keyLen+lenSize+valLen)
	payload = appendU32(payload, uint32(keyLen))
	payload = append(payload, r.Key...)
	payload = appendU32(payload, uint32(valLen))
	payload = append(payload, r.Value...)
	return payload, nil
}

func deletePayload(r Delete) ([]byte, error) {
	keyLen := len(r.Key)
	if !fitsU32(keyLen) {
		return nil, fmt.Errorf("walcodec: key length exceeds uint32")
	}
	payload := make([]byte, 0, lenSize+keyLen)
	payload = appendU32(payload, uint32(keyLen))
	payload = append(payload, r.Key...)
	return payload, nil
}

// appendFrame writes len || type || payload || crc.
// len counts type+payload only; crc is CRC32-IEEE over type+payload, outside len.
func appendFrame(dst []byte, typ byte, payload []byte) ([]byte, error) {
	// len = 1 (type) + payload. Reject before writing a frame Decode must refuse.
	bodyLen := 1 + len(payload)
	if bodyLen > maxFrameLen || bodyLen < 1 {
		return nil, fmt.Errorf("walcodec: frame length %d exceeds %d", bodyLen, maxFrameLen)
	}
	dst = appendU32(dst, uint32(bodyLen))
	bodyStart := len(dst)
	dst = append(dst, typ)
	dst = append(dst, payload...)
	sum := crc32.ChecksumIEEE(dst[bodyStart:])
	dst = appendU32(dst, sum)
	return dst, nil
}

func appendU32(dst []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

// Decode reads a version-1 stream into records in frame order. Duplicate keys
// are kept. On any corruption it returns (nil, ErrCorrupt) with no partial list.
//
// A nil value and an empty value both decode to a non-nil length-0 slice.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	// Version is validated before any frame is read or allocated.
	if b[0] != versionV1 {
		return nil, ErrCorrupt
	}

	recs := make([]Record, 0)
	cursor := 1
	for cursor < len(b) {
		if len(b)-cursor < lenSize {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(b[cursor : cursor+lenSize])
		cursor += lenSize

		// Cap check before any allocation sized from n. The body, when present,
		// is a subslice of the caller buffer, not a make of size n.
		if n > maxFrameLen || n == 0 {
			return nil, ErrCorrupt
		}
		need := int(n) + crcSize
		if len(b)-cursor < need {
			return nil, ErrCorrupt
		}
		body := b[cursor : cursor+int(n)]
		cursor += int(n)
		gotCRC := binary.BigEndian.Uint32(b[cursor : cursor+crcSize])
		cursor += crcSize
		if crc32.ChecksumIEEE(body) != gotCRC {
			return nil, ErrCorrupt
		}

		typ := body[0]
		payload := body[1:]
		switch typ {
		case kindPut:
			rec, err := decodePut(payload)
			if err != nil {
				return nil, err
			}
			recs = append(recs, rec)
		case kindDelete:
			rec, err := decodeDelete(payload)
			if err != nil {
				return nil, err
			}
			recs = append(recs, rec)
		default:
			return nil, ErrCorrupt
		}
	}
	return recs, nil
}

func decodePut(payload []byte) (Put, error) {
	key, off, ok := readKey(payload, 0)
	if !ok {
		return Put{}, ErrCorrupt
	}
	val, off, ok := readBytes(payload, off)
	if !ok || off != len(payload) {
		return Put{}, ErrCorrupt
	}
	return Put{Key: key, Value: val}, nil
}

func decodeDelete(payload []byte) (Delete, error) {
	key, off, ok := readKey(payload, 0)
	if !ok || off != len(payload) {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: key}, nil
}

// readKey copies a length-prefixed string. The conversion to string copies,
// so the result does not alias the input buffer.
func readKey(b []byte, off int) (string, int, bool) {
	raw, next, ok := readRaw(b, off)
	if !ok {
		return "", off, false
	}
	return string(raw), next, true
}

// readBytes copies a length-prefixed byte slice. A zero length returns a
// non-nil empty slice so a decoded nil value and a decoded empty value compare
// equal under bytes.Equal and both have length 0.
func readBytes(b []byte, off int) ([]byte, int, bool) {
	raw, next, ok := readRaw(b, off)
	if !ok {
		return nil, off, false
	}
	if len(raw) == 0 {
		return []byte{}, next, true
	}
	return append([]byte(nil), raw...), next, true
}

// readRaw returns the length-prefixed span at off. It does not allocate and
// does not accept a length that runs past b.
func readRaw(b []byte, off int) ([]byte, int, bool) {
	if off < 0 || len(b)-off < lenSize {
		return nil, off, false
	}
	n := binary.BigEndian.Uint32(b[off : off+lenSize])
	off += lenSize
	// n is bounded by the already-accepted frame; reject a claim past this payload
	// before any copy.
	if uint64(off)+uint64(n) > uint64(len(b)) {
		return nil, off, false
	}
	end := off + int(n)
	return b[off:end], end, true
}

// fitsU32 reports whether n can be stored in a uint32 length prefix.
func fitsU32(n int) bool {
	return n >= 0 && uint64(n) <= uint64(^uint32(0))
}
