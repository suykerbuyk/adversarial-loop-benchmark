// Package walcodec encodes records as a versioned stream of length-prefixed
// frames. Each frame contains a type and payload followed by its CRC32-IEEE;
// frame bodies are limited to 16 MiB.
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	version1    byte   = 1
	kindPut     uint8  = 1
	kindDelete  uint8  = 2
	maxFrameLen uint64 = 16 << 20
)

// Record is a record that can be written to a WAL stream.
type Record interface{ Kind() uint8 }

// Put stores a key and value.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put record tag.
func (Put) Kind() uint8 { return kindPut }

// Delete removes a key.
type Delete struct{ Key string }

// Kind returns the Delete record tag.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt reports a malformed or unsupported encoded stream.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// ErrUnsupportedRecord reports an Encode input that is not a Put or Delete value.
var ErrUnsupportedRecord = errors.New("walcodec: unsupported record")

// ErrRecordTooLarge reports a record that exceeds the maximum frame size.
var ErrRecordTooLarge = errors.New("walcodec: record too large")

// Encode returns the version-1 encoding of recs. It accepts Put and Delete
// values; pointers and other Record implementations are rejected.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{version1}
	for i, rec := range recs {
		var bodyLen uint64
		switch r := rec.(type) {
		case Put:
			bodyLen = 1 + 4 + uint64(len(r.Key)) + 4 + uint64(len(r.Value))
		case Delete:
			bodyLen = 1 + 4 + uint64(len(r.Key))
		default:
			return nil, fmt.Errorf("record %d: %w", i, ErrUnsupportedRecord)
		}
		if bodyLen > maxFrameLen {
			return nil, fmt.Errorf("record %d: %w", i, ErrRecordTooLarge)
		}
		start := len(out)
		out = append(out, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(out[start:start+4], uint32(bodyLen))
		switch r := rec.(type) {
		case Put:
			out = appendPutPayload(out, r)
		case Delete:
			out = appendDeletePayload(out, r)
		}
		crc := crc32.ChecksumIEEE(out[start+4:])
		out = appendU32(out, crc)
	}
	return out, nil
}

func appendPutPayload(out []byte, r Put) []byte {
	out = append(out, kindPut)
	out = appendU32(out, uint32(len(r.Key)))
	out = append(out, r.Key...)
	out = appendU32(out, uint32(len(r.Value)))
	return append(out, r.Value...)
}

func appendDeletePayload(out []byte, r Delete) []byte {
	out = append(out, kindDelete)
	out = appendU32(out, uint32(len(r.Key)))
	return append(out, r.Key...)
}

func appendU32(b []byte, n uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], n)
	return append(b, buf[:]...)
}

// Decode parses a version-1 stream. Malformed input returns ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt(0, "missing version")
	}
	if b[0] != version1 {
		return nil, corrupt(0, "unsupported version %d", b[0])
	}
	recs := make([]Record, 0)
	pos := uint64(1)
	for pos < uint64(len(b)) {
		remaining := uint64(len(b)) - pos
		if remaining < 4 {
			return nil, corrupt(pos, "truncated frame length")
		}
		frameLen := uint64(binary.BigEndian.Uint32(b[int(pos):int(pos+4)]))
		if frameLen > maxFrameLen {
			return nil, corrupt(pos, "frame length exceeds limit")
		}
		if frameLen == 0 {
			return nil, corrupt(pos, "frame has no type")
		}
		frameEnd := pos + 4 + frameLen
		if frameEnd > uint64(len(b)) || uint64(len(b))-frameEnd < 4 {
			return nil, corrupt(pos, "truncated frame")
		}
		body := b[int(pos+4):int(frameEnd)]
		want := binary.BigEndian.Uint32(b[int(frameEnd):int(frameEnd+4)])
		if crc32.ChecksumIEEE(body) != want {
			return nil, corrupt(pos, "checksum mismatch")
		}
		var rec Record
		var err error
		switch body[0] {
		case kindPut:
			rec, err = decodePutPayload(body[1:])
		case kindDelete:
			rec, err = decodeDeletePayload(body[1:])
		default:
			return nil, corrupt(pos, "unknown record type %d", body[0])
		}
		if err != nil {
			return nil, corrupt(pos, "%v", err)
		}
		recs = append(recs, rec)
		pos = frameEnd + 4
	}
	return recs, nil
}

func decodePutPayload(p []byte) (Record, error) {
	key, rest, err := takeString(p)
	if err != nil {
		return nil, err
	}
	if len(rest) < 4 {
		return nil, errors.New("missing value length")
	}
	n := uint64(binary.BigEndian.Uint32(rest[:4]))
	rest = rest[4:]
	if n != uint64(len(rest)) {
		return nil, errors.New("invalid value length or trailing bytes")
	}
	value := make([]byte, int(n))
	copy(value, rest)
	return Put{Key: key, Value: value}, nil
}

func decodeDeletePayload(p []byte) (Record, error) {
	key, rest, err := takeString(p)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing delete payload bytes")
	}
	return Delete{Key: key}, nil
}

func takeString(p []byte) (string, []byte, error) {
	if len(p) < 4 {
		return "", nil, errors.New("missing key length")
	}
	n := uint64(binary.BigEndian.Uint32(p[:4]))
	p = p[4:]
	if n > uint64(len(p)) {
		return "", nil, errors.New("key length exceeds payload")
	}
	return string(p[:int(n)]), p[int(n):], nil
}

func corrupt(offset uint64, format string, args ...any) error {
	return fmt.Errorf("%w at byte %d: %s", ErrCorrupt, offset, fmt.Sprintf(format, args...))
}
