package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

const (
	version1   = byte(1)
	putKind    = byte(1)
	deleteKind = byte(2)
	maxFrame   = uint32(16 << 20)
)

// ErrCorrupt is returned when a record cannot be encoded or a stream is invalid.
var ErrCorrupt = errCorrupt{}

type errCorrupt struct{}

func (errCorrupt) Error() string { return "walcodec: corrupt stream" }

// Record is a record that can be stored in a WAL stream.
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
	out := []byte{version1}
	for _, record := range recs {
		var typ byte
		var key string
		var value []byte
		switch r := record.(type) {
		case Put:
			typ, key, value = putKind, r.Key, r.Value
		case Delete:
			typ, key = deleteKind, r.Key
		default:
			return nil, ErrCorrupt
		}
		keyLen, valueLen := uint64(len(key)), uint64(len(value))
		payloadLen := uint64(4) + keyLen
		if typ == putKind {
			payloadLen += 4 + valueLen
		}
		frameLen := payloadLen + 1
		if keyLen > uint64(^uint32(0)) || valueLen > uint64(^uint32(0)) || frameLen > uint64(maxFrame) {
			return nil, ErrCorrupt
		}
		frame := make([]byte, 0, int(frameLen))
		frame = append(frame, typ)
		frame = appendU32(frame, uint32(keyLen))
		frame = append(frame, key...)
		if typ == putKind {
			frame = appendU32(frame, uint32(valueLen))
			frame = append(frame, value...)
		}
		out = appendU32(out, uint32(len(frame)))
		out = append(out, frame...)
		out = appendU32(out, crc32.ChecksumIEEE(frame))
	}
	return out, nil
}

// Decode parses a version-1 stream.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 || b[0] != version1 {
		return nil, ErrCorrupt
	}
	b = b[1:]
	var records []Record
	for len(b) != 0 {
		if len(b) < 4 {
			return nil, ErrCorrupt
		}
		frameLen := binary.BigEndian.Uint32(b[:4])
		b = b[4:]
		if frameLen == 0 || frameLen > maxFrame {
			return nil, ErrCorrupt
		}
		// Check the complete frame extent before slicing or allocating from its length.
		need := uint64(frameLen) + 4
		if uint64(len(b)) < need {
			return nil, ErrCorrupt
		}
		frame := b[:int(frameLen)]
		wantCRC := binary.BigEndian.Uint32(b[int(frameLen):int(need)])
		b = b[int(need):]
		if crc32.ChecksumIEEE(frame) != wantCRC {
			return nil, ErrCorrupt
		}
		record, ok := decodeFrame(frame)
		if !ok {
			return nil, ErrCorrupt
		}
		records = append(records, record)
	}
	return records, nil
}

func decodeFrame(frame []byte) (Record, bool) {
	if len(frame) < 1 {
		return nil, false
	}
	typ := frame[0]
	p := frame[1:]
	key, rest, ok := takeBytes(p)
	if !ok {
		return nil, false
	}
	switch typ {
	case putKind:
		value, rest, ok := takeBytes(rest)
		if !ok || len(rest) != 0 {
			return nil, false
		}
		return Put{Key: string(key), Value: value}, true
	case deleteKind:
		if len(rest) != 0 {
			return nil, false
		}
		return Delete{Key: string(key)}, true
	default:
		return nil, false
	}
}

func takeBytes(b []byte) ([]byte, []byte, bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b[:4])
	b = b[4:]
	if uint64(len(b)) < uint64(n) {
		return nil, nil, false
	}
	return b[:int(n)], b[int(n):], true
}

func appendU32(dst []byte, n uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], n)
	return append(dst, buf[:]...)
}
