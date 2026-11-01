// Package walcodec encodes and decodes versioned write-ahead-log records.
package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
)

const (
	streamVersion = byte(1)
	maxFrameLen   = uint64(16 << 20)
)

// ErrCorrupt is returned when a stream is malformed or unsupported.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var errEncode = errors.New("walcodec: cannot encode record")

// Record is a WAL record with a wire-format kind tag.
type Record interface{ Kind() uint8 }

// Put stores a value under a key.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns Put's wire-format tag.
func (Put) Kind() uint8 { return 1 }

// Delete removes a key.
type Delete struct{ Key string }

// Kind returns Delete's wire-format tag.
func (Delete) Kind() uint8 { return 2 }

// Encode serializes records in order into a version-1 stream.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{streamVersion}
	for _, rec := range recs {
		var kind byte
		var key string
		var value []byte
		isPut := false
		switch r := rec.(type) {
		case Put:
			kind, key, value, isPut = r.Kind(), r.Key, r.Value, true
		case *Put:
			if r == nil {
				return nil, errEncode
			}
			kind, key, value, isPut = r.Kind(), r.Key, r.Value, true
		case Delete:
			kind, key = r.Kind(), r.Key
		case *Delete:
			if r == nil {
				return nil, errEncode
			}
			kind, key = r.Kind(), r.Key
		default:
			return nil, errEncode
		}
		keyLen, valueLen := uint64(len(key)), uint64(len(value))
		if keyLen > math.MaxUint32 || valueLen > math.MaxUint32 {
			return nil, errEncode
		}
		bodyLen := uint64(1) + 4 + keyLen
		if isPut {
			bodyLen += 4 + valueLen
		}
		if bodyLen > maxFrameLen {
			return nil, errEncode
		}
		body := make([]byte, int(bodyLen))
		body[0] = kind
		pos := 1
		binary.BigEndian.PutUint32(body[pos:pos+4], uint32(keyLen))
		pos += 4
		copy(body[pos:], key)
		pos += int(keyLen)
		if isPut {
			binary.BigEndian.PutUint32(body[pos:pos+4], uint32(valueLen))
			pos += 4
			copy(body[pos:], value)
		}
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(bodyLen))
		out = append(out, prefix[:]...)
		out = append(out, body...)
		binary.BigEndian.PutUint32(prefix[:], crc32.ChecksumIEEE(body))
		out = append(out, prefix[:]...)
	}
	return out, nil
}

// Decode parses a version-1 stream, preserving record order and duplicates.
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
		frameLen := uint64(binary.BigEndian.Uint32(b[pos : pos+4]))
		pos += 4
		if frameLen == 0 || frameLen > maxFrameLen {
			return nil, ErrCorrupt
		}
		// The bounded length is checked against remaining bytes before slicing.
		if frameLen > uint64(len(b)-pos) || uint64(len(b)-pos)-frameLen < 4 {
			return nil, ErrCorrupt
		}
		n := int(frameLen)
		body := b[pos : pos+n]
		pos += n
		gotCRC := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if crc32.ChecksumIEEE(body) != gotCRC {
			return nil, ErrCorrupt
		}
		record, err := decodeBody(body)
		if err != nil {
			return nil, ErrCorrupt
		}
		records = append(records, record)
	}
	return records, nil
}

func decodeBody(body []byte) (Record, error) {
	if len(body) == 0 {
		return nil, ErrCorrupt
	}
	kind := body[0]
	pos := 1
	readField := func() ([]byte, bool) {
		if len(body)-pos < 4 {
			return nil, false
		}
		n := uint64(binary.BigEndian.Uint32(body[pos : pos+4]))
		pos += 4
		if n > uint64(len(body)-pos) {
			return nil, false
		}
		start := pos
		pos += int(n)
		return body[start:pos], true
	}
	keyBytes, ok := readField()
	if !ok {
		return nil, ErrCorrupt
	}
	key := string(keyBytes)
	switch kind {
	case 1:
		valueBytes, ok := readField()
		if !ok || pos != len(body) {
			return nil, ErrCorrupt
		}
		value := make([]byte, len(valueBytes))
		copy(value, valueBytes)
		return Put{Key: key, Value: value}, nil
	case 2:
		if pos != len(body) {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	default:
		return nil, ErrCorrupt
	}
}
