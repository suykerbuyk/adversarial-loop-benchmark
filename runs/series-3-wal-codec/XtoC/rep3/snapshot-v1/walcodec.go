// Package walcodec encodes an ordered sequence of records as a versioned,
// checksummed write-ahead-log stream. A stream starts with version byte 1 and
// contains frames with a big-endian uint32 body length, a type byte and payload,
// and a big-endian CRC32-IEEE checksum. The body length includes the type and
// payload but excludes the checksum; bodies are limited to 16 MiB. Payload
// fields are big-endian uint32 lengths followed by their bytes.
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	version1      byte   = 1
	encodeVersion byte   = version1
	putTag        uint8  = 1
	deleteTag     uint8  = 2
	maxBody       uint32 = 16 << 20
	uint32Len            = 4
	crcLen               = 4
)

// ErrCorrupt indicates that a stream is malformed or uses an unsupported version.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnsupportedRecord = errors.New("walcodec: unsupported record")
	errFrameTooLarge     = errors.New("walcodec: frame exceeds 16 MiB limit")
)

// Record is a WAL record. Encode accepts Put and Delete values only; nil
// records, pointers to either type, and other implementations are rejected.
type Record interface{ Kind() uint8 }

// Put stores a value for a key.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put record tag.
func (Put) Kind() uint8 { return putTag }

// Delete removes a key.
type Delete struct{ Key string }

// Kind returns the Delete record tag.
func (Delete) Kind() uint8 { return deleteTag }

// Encode writes records in order. It accepts Put and Delete values only; nil
// records, pointers, and unsupported records return an error and no output.
// Nil and empty values encode identically. Its errors are distinct from
// ErrCorrupt and can be classified with errors.Is.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{encodeVersion}
	for _, rec := range recs {
		var key string
		var value []byte
		var tag uint8
		switch r := rec.(type) {
		case Put:
			tag, key, value = putTag, r.Key, r.Value
		case Delete:
			tag, key = deleteTag, r.Key
		default:
			return nil, fmt.Errorf("%w: %T", errUnsupportedRecord, rec)
		}

		fieldTotal := uint64(uint32Len) + uint64(len(key))
		if tag == putTag {
			fieldTotal += uint64(uint32Len) + uint64(len(value))
		}
		bodyLen := uint64(1) + fieldTotal
		if bodyLen > uint64(maxBody) {
			return nil, fmt.Errorf("%w", errFrameTooLarge)
		}

		start := len(out)
		out = append(out, 0, 0, 0, 0) // reserve frame length
		bodyStart := len(out)
		out = append(out, tag)
		out = appendField(out, []byte(key))
		if tag == putTag {
			out = appendField(out, value)
		}
		binary.BigEndian.PutUint32(out[start:start+uint32Len], uint32(len(out)-bodyStart))
		checksum := crc32.ChecksumIEEE(out[bodyStart:])
		out = binary.BigEndian.AppendUint32(out, checksum)
	}
	return out, nil
}

func appendField(dst, field []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(field)))
	return append(dst, field...)
}

// Decode parses a stream and returns records in wire order, preserving duplicate
// keys. It returns bare ErrCorrupt and nil records for every malformed stream.
// A version-only stream returns a non-nil empty slice; zero-length values decode
// to non-nil empty slices. Unknown tags, zero-length frames, malformed fields,
// trailing payload bytes, and trailing partial frames are rejected.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, ErrCorrupt
	}
	switch b[0] {
	case version1:
		return decodeV1(b[1:])
	default:
		return nil, ErrCorrupt
	}
}

func decodeV1(data []byte) ([]Record, error) {
	records := make([]Record, 0)
	for len(data) > 0 {
		if len(data) < uint32Len {
			return nil, ErrCorrupt
		}
		bodyLen := binary.BigEndian.Uint32(data[:uint32Len])
		data = data[uint32Len:]
		if bodyLen == 0 || bodyLen > maxBody {
			return nil, ErrCorrupt
		}
		if uint64(len(data)) < uint64(bodyLen)+crcLen {
			return nil, ErrCorrupt
		}
		body := data[:int(bodyLen)]
		gotCRC := binary.BigEndian.Uint32(data[int(bodyLen) : int(bodyLen)+crcLen])
		if crc32.ChecksumIEEE(body) != gotCRC {
			return nil, ErrCorrupt
		}
		rec, ok := decodeBody(body)
		if !ok {
			return nil, ErrCorrupt
		}
		records = append(records, rec)
		data = data[int(bodyLen)+crcLen:]
	}
	return records, nil
}

func decodeBody(body []byte) (Record, bool) {
	if len(body) == 0 {
		return nil, false
	}
	tag := body[0]
	pos := 1
	keyBytes, ok := readField(body, &pos)
	if !ok {
		return nil, false
	}
	key := string(keyBytes)
	switch tag {
	case putTag:
		valueBytes, ok := readField(body, &pos)
		if !ok || pos != len(body) {
			return nil, false
		}
		value := make([]byte, len(valueBytes))
		copy(value, valueBytes)
		return Put{Key: key, Value: value}, true
	case deleteTag:
		if pos != len(body) {
			return nil, false
		}
		return Delete{Key: key}, true
	default:
		return nil, false
	}
}

func readField(body []byte, pos *int) ([]byte, bool) {
	if *pos > len(body) || len(body)-*pos < uint32Len {
		return nil, false
	}
	n := binary.BigEndian.Uint32(body[*pos : *pos+uint32Len])
	*pos += uint32Len
	remaining := len(body) - *pos
	if uint64(n) > uint64(remaining) {
		return nil, false
	}
	end := *pos + int(n)
	field := body[*pos:end]
	*pos = end
	return field, true
}
