package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	version1   = byte(1)
	putKind    = byte(1)
	deleteKind = byte(2)
	maxBody    = 16 << 20
)

// ErrCorrupt indicates that a stream is malformed or uses an unsupported version.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var errEncode = errors.New("walcodec: cannot encode record")

// Record is a WAL record with a wire-format kind tag.
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

// Encode serializes records as a version-1 WAL stream.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{version1}
	for _, rec := range recs {
		var kind byte
		var key string
		var value []byte
		hasValue := false
		switch r := rec.(type) {
		case Put:
			kind, key, value, hasValue = putKind, r.Key, r.Value, true
		case *Put:
			if r == nil {
				return nil, errEncode
			}
			kind, key, value, hasValue = putKind, r.Key, r.Value, true
		case Delete:
			kind, key = deleteKind, r.Key
		case *Delete:
			if r == nil {
				return nil, errEncode
			}
			kind, key = deleteKind, r.Key
		default:
			return nil, errEncode
		}
		if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
			return nil, errEncode
		}
		bodyLen := uint64(1 + 4 + len(key))
		if hasValue {
			bodyLen += 4 + uint64(len(value))
		}
		if bodyLen > maxBody {
			return nil, errEncode
		}
		body := make([]byte, int(bodyLen))
		body[0] = kind
		binary.BigEndian.PutUint32(body[1:5], uint32(len(key)))
		copy(body[5:], key)
		pos := 5 + len(key)
		if hasValue {
			binary.BigEndian.PutUint32(body[pos:pos+4], uint32(len(value)))
			copy(body[pos+4:], value)
		}
		frame := make([]byte, 4+len(body)+4)
		binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
		copy(frame[4:], body)
		binary.BigEndian.PutUint32(frame[4+len(body):], crc32.ChecksumIEEE(body))
		out = append(out, frame...)
	}
	return out, nil
}

// Decode parses a version-1 WAL stream. Any malformed input returns ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 || b[0] != version1 {
		return nil, ErrCorrupt
	}
	records := make([]Record, 0)
	pos := 1
	for pos < len(b) {
		if len(b)-pos < 4 {
			return nil, ErrCorrupt
		}
		bodyLen := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if bodyLen == 0 || bodyLen > maxBody {
			return nil, ErrCorrupt
		}
		if uint64(bodyLen)+4 > uint64(len(b)-pos) {
			return nil, ErrCorrupt
		}
		body := b[pos : pos+int(bodyLen)]
		pos += int(bodyLen)
		wantCRC := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if crc32.ChecksumIEEE(body) != wantCRC {
			return nil, ErrCorrupt
		}
		payload := body[1:]
		switch body[0] {
		case putKind:
			key, rest, ok := readField(payload)
			if !ok {
				return nil, ErrCorrupt
			}
			value, rest, ok := readField(rest)
			if !ok || len(rest) != 0 {
				return nil, ErrCorrupt
			}
			v := make([]byte, len(value))
			copy(v, value)
			records = append(records, Put{Key: string(key), Value: v})
		case deleteKind:
			key, rest, ok := readField(payload)
			if !ok || len(rest) != 0 {
				return nil, ErrCorrupt
			}
			records = append(records, Delete{Key: string(key)})
		default:
			return nil, ErrCorrupt
		}
	}
	return records, nil
}

func readField(b []byte) ([]byte, []byte, bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b[:4])
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	end := 4 + int(n)
	return b[4:end], b[end:], true
}
