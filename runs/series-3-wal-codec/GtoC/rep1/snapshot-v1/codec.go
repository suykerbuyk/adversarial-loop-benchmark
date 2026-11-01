package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Record is one write-ahead-log entry. Put and Delete are the concrete
// records. Encode accepts those values only: Kind uses a value receiver, so
// *Put and *Delete also satisfy Record, but Encode rejects pointers. Decode
// returns Put and Delete values, matching the type assertions in the tests.
type Record interface {
	Kind() uint8
}

// Put records a key and value. A nil Value and an empty Value are equal:
// both encode as vlen 0 and both decode to a non-nil length-0 slice.
type Put struct {
	Key   string
	Value []byte
}

// Delete records removal of a key.
type Delete struct {
	Key string
}

const (
	versionV1 = 1

	kindPut    uint8 = 1
	kindDelete uint8 = 2

	// maxFrameLen is the largest legal frame length word (type + payload).
	// A length equal to the cap is legal; a length above it is ErrCorrupt.
	maxFrameLen = 16 << 20
)

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned for every wire failure: a missing or unknown version,
// a truncated frame, a CRC mismatch, an unknown type tag, an oversize length,
// or a payload whose inner lengths do not match the frame.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnknownRecord = errors.New("walcodec: unknown record")
	errFrameTooLarge = errors.New("walcodec: frame exceeds 16MiB")
)

// Encode writes a version-1 stream: a version byte, then one frame per record
// in order. Only Put and Delete values are accepted. A nil element or any
// other concrete type, including *Put and *Delete, returns an error that does
// not wrap ErrCorrupt. A record whose type+payload would exceed 16 MiB returns
// errFrameTooLarge before any of that record's bytes are written.
// Encode(nil) and Encode of an empty slice both return a one-byte stream.
func Encode(recs []Record) ([]byte, error) {
	// Size every frame before writing so a too-large record cannot leave a
	// partial frame in the output, and so the buffer is allocated once.
	n := 1
	for _, r := range recs {
		frameLen, err := frameSize(r)
		if err != nil {
			return nil, err
		}
		next, ok := addSize(n, 4+frameLen+4)
		if !ok {
			return nil, errFrameTooLarge
		}
		n = next
	}

	buf := make([]byte, 1, n)
	buf[0] = versionV1
	for _, r := range recs {
		var err error
		switch x := r.(type) {
		case Put:
			buf, err = appendPut(buf, x)
		case Delete:
			buf, err = appendDelete(buf, x)
		default:
			// frameSize already rejected this element.
			return nil, fmt.Errorf("%w: %T", errUnknownRecord, r)
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// frameSize reports the type+payload length of r.
// A nil interface does not panic: the type switch selects the nil case.
// Only a method call on a nil interface would panic, and this function
// never makes one.
func frameSize(r Record) (int, error) {
	switch x := r.(type) {
	case Put:
		return putFrameLen(x.Key, x.Value)
	case Delete:
		return deleteFrameLen(x.Key)
	case nil:
		return 0, fmt.Errorf("%w: nil", errUnknownRecord)
	default:
		return 0, fmt.Errorf("%w: %T", errUnknownRecord, r)
	}
}

// putFrameLen is 1 (type) + 4 (klen) + len(key) + 4 (vlen) + len(value).
// The 16 MiB frame cap is the only size bound: it is far below MaxUint32, so
// a length that passes it always fits in a uint32 prefix. Comparing against
// the cap in int also compiles on 32-bit ports.
func putFrameLen(key string, value []byte) (int, error) {
	k := len(key)
	v := len(value)
	if k > maxFrameLen-9 || v > maxFrameLen-9 || k+v > maxFrameLen-9 {
		return 0, errFrameTooLarge
	}
	return 9 + k + v, nil
}

// deleteFrameLen is 1 (type) + 4 (klen) + len(key).
func deleteFrameLen(key string) (int, error) {
	k := len(key)
	if k > maxFrameLen-5 {
		return 0, errFrameTooLarge
	}
	return 5 + k, nil
}

func addSize(n, delta int) (int, bool) {
	if delta < 0 || n < 0 {
		return 0, false
	}
	sum := n + delta
	if sum < n {
		return 0, false
	}
	return sum, true
}

func appendPut(buf []byte, p Put) ([]byte, error) {
	frameLen, err := putFrameLen(p.Key, p.Value)
	if err != nil {
		return nil, err
	}
	start := len(buf)
	buf = appendU32(buf, uint32(frameLen))
	buf = append(buf, kindPut)
	buf = appendU32(buf, uint32(len(p.Key)))
	buf = append(buf, p.Key...)
	buf = appendU32(buf, uint32(len(p.Value)))
	buf = append(buf, p.Value...)
	return appendU32(buf, crc32.ChecksumIEEE(buf[start+4:])), nil
}

func appendDelete(buf []byte, d Delete) ([]byte, error) {
	frameLen, err := deleteFrameLen(d.Key)
	if err != nil {
		return nil, err
	}
	start := len(buf)
	buf = appendU32(buf, uint32(frameLen))
	buf = append(buf, kindDelete)
	buf = appendU32(buf, uint32(len(d.Key)))
	buf = append(buf, d.Key...)
	return appendU32(buf, crc32.ChecksumIEEE(buf[start+4:])), nil
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// Decode parses a version-1 stream. Every wire failure is ErrCorrupt and is
// not wrapped. On error the record slice is nil. A version-only stream
// returns a non-nil empty slice.
//
// Cursor rule: i is the next unread byte. After the 4-byte length word is
// accepted, i advances by 4, then by n+4 (type+payload and CRC).
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	switch b[0] {
	case versionV1:
		return decodeFramesV1(b[1:])
	default:
		return nil, ErrCorrupt
	}
}

func decodeFramesV1(b []byte) ([]Record, error) {
	// Leave out nil until the first valid record so a corrupt length word
	// (including an oversize len) is rejected before any allocation.
	var out []Record
	i := 0
	for i < len(b) {
		if len(b)-i < 4 {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(b[i : i+4])
		if n > maxFrameLen || n == 0 {
			return nil, ErrCorrupt
		}
		i += 4
		need := int(n) + 4
		if len(b)-i < need {
			return nil, ErrCorrupt
		}
		body := b[i : i+int(n)]
		gotCRC := binary.BigEndian.Uint32(b[i+int(n) : i+need])
		if crc32.ChecksumIEEE(body) != gotCRC {
			return nil, ErrCorrupt
		}
		rec, err := decodeFrame(body)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		i += need
	}
	if out == nil {
		return []Record{}, nil
	}
	return out, nil
}

// decodeFrame parses a CRC-verified type+payload span.
func decodeFrame(body []byte) (Record, error) {
	if len(body) < 1 {
		return nil, ErrCorrupt
	}
	switch body[0] {
	case kindPut:
		return parsePut(body[1:])
	case kindDelete:
		return parseDelete(body[1:])
	default:
		return nil, ErrCorrupt
	}
}

func parsePut(payload []byte) (Put, error) {
	if len(payload) < 8 {
		return Put{}, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[0:4])
	// klen must leave the 4-byte vlen prefix. uint64 keeps a hostile klen
	// from wrapping the slice bound.
	if uint64(klen) > uint64(len(payload)-8) {
		return Put{}, ErrCorrupt
	}
	keyEnd := 4 + int(klen)
	vlen := binary.BigEndian.Uint32(payload[keyEnd : keyEnd+4])
	valOff := keyEnd + 4
	if uint64(vlen) != uint64(len(payload)-valOff) {
		return Put{}, ErrCorrupt
	}
	value := []byte{}
	if vlen != 0 {
		value = append([]byte(nil), payload[valOff:]...)
	}
	return Put{Key: string(payload[4:keyEnd]), Value: value}, nil
}

func parseDelete(payload []byte) (Delete, error) {
	if len(payload) < 4 {
		return Delete{}, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[0:4])
	if uint64(klen) != uint64(len(payload)-4) {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: string(payload[4:])}, nil
}
