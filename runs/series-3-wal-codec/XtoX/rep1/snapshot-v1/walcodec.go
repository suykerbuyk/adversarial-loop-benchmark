package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	streamVersion = byte(1)
	maxFrameLen   = uint32(16 << 20)
	putKind       = uint8(1)
	deleteKind    = uint8(2)
)

// ErrCorrupt indicates that an input stream is malformed or unsupported.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a WAL record supported by the codec.
type Record interface {
	Kind() uint8
}

// Put stores a value under a key.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put record tag.
func (Put) Kind() uint8 { return putKind }

// Delete removes a key.
type Delete struct {
	Key string
}

// Kind returns the Delete record tag.
func (Delete) Kind() uint8 { return deleteKind }

// Encode serializes records into a version-1 framed stream.
func Encode(recs []Record) ([]byte, error) {
	out := []byte{streamVersion}
	for i, rec := range recs {
		var typ byte
		var key string
		var value []byte
		switch r := rec.(type) {
		case Put:
			typ, key, value = byte(putKind), r.Key, r.Value
		case *Put:
			if r == nil {
				return nil, fmt.Errorf("walcodec: record %d is nil", i)
			}
			typ, key, value = byte(putKind), r.Key, r.Value
		case Delete:
			typ, key = byte(deleteKind), r.Key
		case *Delete:
			if r == nil {
				return nil, fmt.Errorf("walcodec: record %d is nil", i)
			}
			typ, key = byte(deleteKind), r.Key
		default:
			return nil, fmt.Errorf("walcodec: unsupported record at index %d", i)
		}

		keyLen := uint64(len(key))
		valueLen := uint64(len(value))
		payloadLen := uint64(4) + keyLen
		if typ == byte(putKind) {
			payloadLen += 4 + valueLen
		}
		frameLen := payloadLen + 1
		if keyLen > uint64(^uint32(0)) || valueLen > uint64(^uint32(0)) || frameLen > uint64(maxFrameLen) {
			return nil, fmt.Errorf("walcodec: record %d exceeds frame limit", i)
		}

		body := make([]byte, int(frameLen))
		body[0] = typ
		pos := 1
		binary.BigEndian.PutUint32(body[pos:pos+4], uint32(keyLen))
		pos += 4
		copy(body[pos:], key)
		pos += len(key)
		if typ == byte(putKind) {
			binary.BigEndian.PutUint32(body[pos:pos+4], uint32(valueLen))
			pos += 4
			copy(body[pos:], value)
		}

		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(frameLen))
		out = append(out, prefix[:]...)
		out = append(out, body...)
		binary.BigEndian.PutUint32(prefix[:], crc32.ChecksumIEEE(body))
		out = append(out, prefix[:]...)
	}
	return out, nil
}

// Decode parses a version-1 stream and returns its records in stream order.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 || b[0] != streamVersion {
		return nil, ErrCorrupt
	}
	pos := 1
	records := make([]Record, 0)
	for pos < len(b) {
		if len(b)-pos < 4 {
			return nil, ErrCorrupt
		}
		frameLen := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		if frameLen > maxFrameLen {
			return nil, ErrCorrupt
		}
		remaining := len(b) - pos
		if uint64(remaining) < uint64(frameLen)+4 {
			return nil, ErrCorrupt
		}
		bodyEnd := pos + int(frameLen)
		body := b[pos:bodyEnd]
		wantCRC := binary.BigEndian.Uint32(b[bodyEnd : bodyEnd+4])
		if crc32.ChecksumIEEE(body) != wantCRC || len(body) < 1 {
			return nil, ErrCorrupt
		}
		rec, err := decodeRecord(body)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
		pos = bodyEnd + 4
	}
	return records, nil
}

func decodeRecord(body []byte) (Record, error) {
	typ := body[0]
	cur := cursor{b: body[1:]}
	keyBytes, ok := cur.readBytes()
	if !ok {
		return nil, ErrCorrupt
	}
	key := string(keyBytes)
	switch typ {
	case byte(putKind):
		value, ok := cur.readBytes()
		if !ok || cur.remaining() != 0 {
			return nil, ErrCorrupt
		}
		copied := append([]byte{}, value...)
		return Put{Key: key, Value: copied}, nil
	case byte(deleteKind):
		if cur.remaining() != 0 {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	default:
		return nil, ErrCorrupt
	}
}

type cursor struct {
	b   []byte
	pos int
}

func (c *cursor) remaining() int { return len(c.b) - c.pos }

func (c *cursor) readBytes() ([]byte, bool) {
	if c.remaining() < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(c.b[c.pos : c.pos+4])
	c.pos += 4
	if uint64(n) > uint64(c.remaining()) {
		return nil, false
	}
	end := c.pos + int(n)
	result := c.b[c.pos:end]
	c.pos = end
	return result, true
}
