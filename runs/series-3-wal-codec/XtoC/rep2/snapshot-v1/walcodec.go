// Package walcodec encodes and decodes versioned write-ahead-log streams.
package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	streamVersion uint8 = 1
	putKind       uint8 = 1
	deleteKind    uint8 = 2
	maxFrameLen         = 16 << 20
)

// ErrCorrupt indicates that a stream is malformed or uses an unsupported version.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a record that can be written to a WAL stream.
type Record interface{ Kind() uint8 }

// Put stores a value at Key.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put record tag.
func (Put) Kind() uint8 { return putKind }

// Delete removes Key.
type Delete struct{ Key string }

// Kind returns the Delete record tag.
func (Delete) Kind() uint8 { return deleteKind }

// Encode writes recs as a version-1 stream.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{streamVersion}
	for _, rec := range recs {
		var kind uint8
		var key string
		var value []byte
		switch r := rec.(type) {
		case Put:
			kind, key, value = putKind, r.Key, r.Value
		case *Put:
			if r == nil {
				return nil, errors.New("walcodec: nil record")
			}
			kind, key, value = putKind, r.Key, r.Value
		case Delete:
			kind, key = deleteKind, r.Key
		case *Delete:
			if r == nil {
				return nil, errors.New("walcodec: nil record")
			}
			kind, key = deleteKind, r.Key
		default:
			return nil, errors.New("walcodec: unsupported record")
		}

		if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
			return nil, errors.New("walcodec: field too large")
		}
		payloadLen := uint64(4) + uint64(len(key))
		if kind == putKind {
			payloadLen += 4 + uint64(len(value))
		}
		bodyLen := uint64(1) + payloadLen
		if bodyLen > maxFrameLen {
			return nil, errors.New("walcodec: frame too large")
		}

		start := len(out)
		out = append(out, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(out[start:start+4], uint32(bodyLen))
		bodyStart := len(out)
		out = append(out, kind)
		out = appendU32Bytes(out, []byte(key))
		if kind == putKind {
			out = appendU32Bytes(out, value)
		}
		crc := crc32.ChecksumIEEE(out[bodyStart:])
		out = append(out, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(out[len(out)-4:], crc)
	}
	return out, nil
}

func appendU32Bytes(dst, data []byte) []byte {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))
	dst = append(dst, prefix[:]...)
	return append(dst, data...)
}

// Decode parses a version-1 stream. Malformed input returns ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 || b[0] != streamVersion {
		return nil, ErrCorrupt
	}
	pos := 1
	var records []Record
	for pos < len(b) {
		if len(b)-pos < 4 {
			return nil, ErrCorrupt
		}
		bodyLen := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if bodyLen > maxFrameLen {
			return nil, ErrCorrupt
		}
		if bodyLen == 0 {
			return nil, ErrCorrupt
		}
		remaining := len(b) - pos
		if uint64(bodyLen)+4 > uint64(remaining) {
			return nil, ErrCorrupt
		}
		bodyEnd := pos + int(bodyLen)
		body := b[pos:bodyEnd]
		wantCRC := binary.BigEndian.Uint32(b[bodyEnd : bodyEnd+4])
		if crc32.ChecksumIEEE(body) != wantCRC {
			return nil, ErrCorrupt
		}
		record, err := decodeBody(body)
		if err != nil {
			return nil, ErrCorrupt
		}
		records = append(records, record)
		pos = bodyEnd + 4
	}
	return records, nil
}

func decodeBody(body []byte) (Record, error) {
	kind := body[0]
	pos := 1
	keyBytes, ok := readField(body, &pos)
	if !ok {
		return nil, ErrCorrupt
	}
	key := string(keyBytes)
	switch kind {
	case putKind:
		valueBytes, ok := readField(body, &pos)
		if !ok || pos != len(body) {
			return nil, ErrCorrupt
		}
		value := make([]byte, len(valueBytes))
		copy(value, valueBytes)
		return Put{Key: key, Value: value}, nil
	case deleteKind:
		if pos != len(body) {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	default:
		return nil, ErrCorrupt
	}
}

func readField(b []byte, pos *int) ([]byte, bool) {
	if len(b)-*pos < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(b[*pos : *pos+4])
	*pos += 4
	if uint64(n) > uint64(len(b)-*pos) {
		return nil, false
	}
	start := *pos
	*pos += int(n)
	return b[start:*pos], true
}
