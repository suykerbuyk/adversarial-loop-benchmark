package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	versionV1    byte = 1
	putKind      byte = 1
	deleteKind   byte = 2
	maxFrameBody      = 16 << 20
)

// ErrCorrupt indicates that a stream is malformed or has an unsupported version.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// ErrType indicates that Encode received an unsupported record or a record too large to encode.
var ErrType = errors.New("walcodec: unsupported record or size")

// Record is a write-ahead-log operation.
type Record interface {
	Kind() uint8
}

// Put stores a value under a key.
type Put struct {
	Key   string
	Value []byte
}

func (Put) Kind() uint8 { return uint8(putKind) }

// Delete removes a key.
type Delete struct {
	Key string
}

func (Delete) Kind() uint8 { return uint8(deleteKind) }

// Encode serializes records as a version-1 stream.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{versionV1}
	for _, rec := range recs {
		var kind byte
		var key string
		var value []byte
		withValue := false
		switch r := rec.(type) {
		case Put:
			kind, key, value, withValue = putKind, r.Key, r.Value, true
		case *Put:
			if r == nil {
				return nil, ErrType
			}
			kind, key, value, withValue = putKind, r.Key, r.Value, true
		case Delete:
			kind, key = deleteKind, r.Key
		case *Delete:
			if r == nil {
				return nil, ErrType
			}
			kind, key = deleteKind, r.Key
		default:
			return nil, ErrType
		}

		keyLen := uint64(len(key))
		valueLen := uint64(len(value))
		if keyLen > math.MaxUint32 || (withValue && valueLen > math.MaxUint32) {
			return nil, ErrType
		}
		bodyLen := uint64(1+4) + keyLen
		if withValue {
			bodyLen += 4 + valueLen
		}
		if bodyLen > maxFrameBody {
			return nil, ErrType
		}

		start := len(out)
		out = append(out, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(out[start:start+4], uint32(bodyLen))
		out = append(out, kind)
		appendU32 := func(n uint32) {
			var buf [4]byte
			binary.BigEndian.PutUint32(buf[:], n)
			out = append(out, buf[:]...)
		}
		appendU32(uint32(keyLen))
		out = append(out, key...)
		if withValue {
			appendU32(uint32(valueLen))
			out = append(out, value...)
		}
		crc := crc32.ChecksumIEEE(out[start+4:])
		var crcBuf [4]byte
		binary.BigEndian.PutUint32(crcBuf[:], crc)
		out = append(out, crcBuf[:]...)
	}
	return out, nil
}

// Decode parses a version-1 stream and returns records in stream order.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 || b[0] != versionV1 {
		return nil, ErrCorrupt
	}
	var records []Record
	pos := 1
	for pos < len(b) {
		off := pos
		if len(b)-pos < 4 {
			return records, corruptAt(off)
		}
		n := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if n > maxFrameBody {
			return records, corruptAt(off)
		}
		remaining := len(b) - pos
		if uint64(remaining) < uint64(n)+4 {
			return records, corruptAt(off)
		}
		body := b[pos : pos+int(n)]
		pos += int(n)
		wantCRC := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4

		rec, ok := parseBody(body)
		if !ok || crc32.ChecksumIEEE(body) != wantCRC {
			return records, corruptAt(off)
		}
		records = append(records, rec)
	}
	return records, nil
}

func corruptAt(offset int) error {
	return fmt.Errorf("walcodec: frame at %d: %w", offset, ErrCorrupt)
}

func parseBody(body []byte) (Record, bool) {
	if len(body) < 1 {
		return nil, false
	}
	kind := body[0]
	p := 1
	key, ok := readField(body, &p)
	if !ok {
		return nil, false
	}
	keyString := string(key)
	switch kind {
	case putKind:
		value, ok := readField(body, &p)
		if !ok || p != len(body) {
			return nil, false
		}
		// Allocate even for length zero, so decoded empty values are non-nil.
		owned := make([]byte, len(value))
		copy(owned, value)
		return Put{Key: keyString, Value: owned}, true
	case deleteKind:
		if p != len(body) {
			return nil, false
		}
		return Delete{Key: keyString}, true
	default:
		return nil, false
	}
}

func readField(body []byte, pos *int) ([]byte, bool) {
	if len(body)-*pos < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(body[*pos : *pos+4])
	*pos += 4
	if uint64(len(body)-*pos) < uint64(n) {
		return nil, false
	}
	field := body[*pos : *pos+int(n)]
	*pos += int(n)
	return field, true
}
