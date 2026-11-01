// Package walcodec encodes and decodes versioned write-ahead-log records.
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

type Record interface{ Kind() uint8 }

type Put struct {
	Key   string
	Value []byte
}

type Delete struct{ Key string }

func (Put) Kind() uint8    { return 1 }
func (Delete) Kind() uint8 { return 2 }

var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version     = byte(1)
	putTag      = byte(1)
	deleteTag   = byte(2)
	uint32Width = 4
	crcWidth    = 4
	maxFrameLen = 16 << 20
)

func Encode(recs []Record) ([]byte, error) {
	out := []byte{version}
	for i, rec := range recs {
		var tag byte
		var key string
		var value []byte
		switch r := rec.(type) {
		case Put:
			tag, key, value = putTag, r.Key, r.Value
		case *Put:
			if r == nil {
				return nil, fmt.Errorf("walcodec: record %d is nil", i)
			}
			tag, key, value = putTag, r.Key, r.Value
		case Delete:
			tag, key = deleteTag, r.Key
		case *Delete:
			if r == nil {
				return nil, fmt.Errorf("walcodec: record %d is nil", i)
			}
			tag, key = deleteTag, r.Key
		case nil:
			return nil, fmt.Errorf("walcodec: record %d is nil", i)
		default:
			return nil, fmt.Errorf("walcodec: unsupported record %T", rec)
		}

		if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("walcodec: record %d field exceeds uint32 length", i)
		}
		bodyLen := uint64(1+uint32Width) + uint64(len(key))
		if tag == putTag {
			bodyLen += uint32Width + uint64(len(value))
		}
		if bodyLen > maxFrameLen {
			return nil, fmt.Errorf("walcodec: record %d frame exceeds %d bytes", i, maxFrameLen)
		}

		body := make([]byte, int(bodyLen))
		body[0] = tag
		binary.BigEndian.PutUint32(body[1:5], uint32(len(key)))
		pos := 5
		copy(body[pos:], key)
		pos += len(key)
		if tag == putTag {
			binary.BigEndian.PutUint32(body[pos:pos+4], uint32(len(value)))
			pos += 4
			copy(body[pos:], value)
		}

		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
		out = append(out, prefix[:]...)
		out = append(out, body...)
		binary.BigEndian.PutUint32(prefix[:], crc32.ChecksumIEEE(body))
		out = append(out, prefix[:]...)
	}
	return out, nil
}

func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 || b[0] != version {
		return nil, ErrCorrupt
	}
	pos := 1
	records := make([]Record, 0)
	for pos < len(b) {
		if len(b)-pos < uint32Width {
			return nil, ErrCorrupt
		}
		bodyLen := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += uint32Width
		if bodyLen > maxFrameLen {
			return nil, ErrCorrupt
		}
		remaining := len(b) - pos
		if uint64(bodyLen)+crcWidth > uint64(remaining) {
			return nil, ErrCorrupt
		}
		bodyEnd := pos + int(bodyLen)
		body := b[pos:bodyEnd]
		wantCRC := binary.BigEndian.Uint32(b[bodyEnd : bodyEnd+crcWidth])
		if crc32.ChecksumIEEE(body) != wantCRC || len(body) < 1 {
			return nil, ErrCorrupt
		}
		record, err := decodeBody(body)
		if err != nil {
			return nil, ErrCorrupt
		}
		records = append(records, record)
		pos = bodyEnd + crcWidth
	}
	return records, nil
}

func decodeBody(body []byte) (Record, error) {
	pos := 1
	readBytes := func() ([]byte, error) {
		if len(body)-pos < uint32Width {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(body[pos : pos+4])
		pos += uint32Width
		if uint64(n) > uint64(len(body)-pos) {
			return nil, ErrCorrupt
		}
		start := pos
		pos += int(n)
		value := make([]byte, int(n))
		copy(value, body[start:pos])
		return value, nil
	}
	keyBytes, err := readBytes()
	if err != nil {
		return nil, err
	}
	key := string(keyBytes)
	switch body[0] {
	case deleteTag:
		if pos != len(body) {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	case putTag:
		value, err := readBytes()
		if err != nil || pos != len(body) {
			return nil, ErrCorrupt
		}
		return Put{Key: key, Value: value}, nil
	default:
		return nil, ErrCorrupt
	}
}
