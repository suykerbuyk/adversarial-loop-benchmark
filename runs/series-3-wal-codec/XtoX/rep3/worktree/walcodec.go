package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
)

// ErrCorrupt is returned when an encoded stream is malformed.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1    = byte(1)
	version2    = byte(2)
	maxFrameLen = uint32(16 << 20)
	maxDepth    = 8
)

// Record is a record that can be stored in a WAL stream.
type Record interface {
	Kind() uint8
}

// Put stores a value under a key.
type Put struct {
	Key   string
	Value []byte
}

func (Put) Kind() uint8 { return 1 }

// Delete removes a key.
type Delete struct {
	Key string
}

func (Delete) Kind() uint8 { return 2 }

// Batch groups records into a nested framed sub-stream.
type Batch struct {
	Records []Record
}

func (Batch) Kind() uint8 { return 3 }

// Encode serializes records into a version 2 WAL stream.
func Encode(records []Record) ([]byte, error) {
	frames, err := encodeStream(records, 0)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1, len(frames)+1)
	out[0] = version2
	out = append(out, frames...)
	return out, nil
}

func encodeStream(records []Record, depth int) ([]byte, error) {
	if depth > maxDepth || uint64(len(records)) > math.MaxUint32 {
		return nil, ErrCorrupt
	}
	var out []byte
	for _, rec := range records {
		kind, payload, err := encodeRecord(rec, depth)
		if err != nil {
			return nil, err
		}
		if uint64(len(payload))+1 > uint64(maxFrameLen) {
			return nil, ErrCorrupt
		}
		frameLen := uint32(len(payload) + 1)
		if uint64(len(out))+4+uint64(frameLen)+4 > uint64(math.MaxInt) {
			return nil, ErrCorrupt
		}
		body := make([]byte, int(frameLen))
		body[0] = kind
		copy(body[1:], payload)
		out = appendU32(out, frameLen)
		out = append(out, body...)
		out = appendU32(out, crc32.ChecksumIEEE(body))
	}
	return out, nil
}

func encodeRecord(rec Record, depth int) (byte, []byte, error) {
	switch r := rec.(type) {
	case Put:
		p, ok := encodeStrings(r.Key, r.Value)
		if !ok {
			return 0, nil, ErrCorrupt
		}
		return 1, p, nil
	case Delete:
		p, ok := encodeDelete(r.Key)
		if !ok {
			return 0, nil, ErrCorrupt
		}
		return 2, p, nil
	case Batch:
		if depth >= maxDepth {
			return 0, nil, ErrCorrupt
		}
		frames, err := encodeStream(r.Records, depth+1)
		if err != nil || uint64(len(frames))+4 > uint64(maxFrameLen)-1 {
			return 0, nil, ErrCorrupt
		}
		p := make([]byte, 4, len(frames)+4)
		binary.BigEndian.PutUint32(p, uint32(len(r.Records)))
		p = append(p, frames...)
		return 3, p, nil
	default:
		return 0, nil, ErrCorrupt
	}
}

func encodeStrings(key string, value []byte) ([]byte, bool) {
	if uint64(len(key)) > math.MaxUint32 || uint64(len(value)) > math.MaxUint32 ||
		uint64(len(key))+uint64(len(value))+8 > uint64(maxFrameLen)-1 {
		return nil, false
	}
	p := make([]byte, 4, len(key)+len(value)+8)
	binary.BigEndian.PutUint32(p, uint32(len(key)))
	p = append(p, key...)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(value)))
	p = append(p, n[:]...)
	p = append(p, value...)
	return p, true
}

func encodeDelete(key string) ([]byte, bool) {
	if uint64(len(key)) > math.MaxUint32 || uint64(len(key))+4 > uint64(maxFrameLen)-1 {
		return nil, false
	}
	p := make([]byte, 4, len(key)+4)
	binary.BigEndian.PutUint32(p, uint32(len(key)))
	p = append(p, key...)
	return p, true
}

func appendU32(dst []byte, n uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	return append(dst, b[:]...)
}

// Decode parses a version 1 or version 2 WAL stream.
func Decode(data []byte) ([]Record, error) {
	if len(data) == 0 || (data[0] != version1 && data[0] != version2) {
		return nil, ErrCorrupt
	}
	records, rest, err := decodeStream(data[1:], 0, data[0])
	if err != nil || len(rest) != 0 {
		return nil, ErrCorrupt
	}
	return records, nil
}

func decodeStream(data []byte, depth int, version byte) ([]Record, []byte, error) {
	if depth > maxDepth {
		return nil, nil, ErrCorrupt
	}
	var records []Record
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(data[:4])
		if n == 0 || n > maxFrameLen {
			return nil, nil, ErrCorrupt
		}
		total := uint64(4) + uint64(n) + 4
		if total > uint64(len(data)) {
			return nil, nil, ErrCorrupt
		}
		body := data[4 : 4+int(n)]
		wantCRC := binary.BigEndian.Uint32(data[4+int(n) : int(total)])
		if crc32.ChecksumIEEE(body) != wantCRC {
			return nil, nil, ErrCorrupt
		}
		rec, err := decodeRecord(body[0], body[1:], depth, version)
		if err != nil {
			return nil, nil, ErrCorrupt
		}
		records = append(records, rec)
		data = data[int(total):]
	}
	return records, data, nil
}

func decodeRecord(kind byte, payload []byte, depth int, version byte) (Record, error) {
	switch kind {
	case 1:
		key, value, err := decodePut(payload)
		if err != nil {
			return nil, ErrCorrupt
		}
		return Put{Key: key, Value: value}, nil
	case 2:
		key, err := decodeDelete(payload)
		if err != nil {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	case 3:
		if version != version2 || depth >= maxDepth || len(payload) < 4 {
			return nil, ErrCorrupt
		}
		count := binary.BigEndian.Uint32(payload[:4])
		inner, rest, err := decodeCounted(payload[4:], count, depth+1, version)
		if err != nil || len(rest) != 0 {
			return nil, ErrCorrupt
		}
		return Batch{Records: inner}, nil
	default:
		return nil, ErrCorrupt
	}
}

func decodeCounted(data []byte, count uint32, depth int, version byte) ([]Record, []byte, error) {
	if depth > maxDepth || uint64(count) > uint64(len(data))/8 {
		return nil, nil, ErrCorrupt
	}
	records := make([]Record, 0, int(count))
	for i := uint32(0); i < count; i++ {
		if len(data) < 4 {
			return nil, nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(data[:4])
		if n == 0 || n > maxFrameLen || uint64(n)+8 > uint64(len(data)) {
			return nil, nil, ErrCorrupt
		}
		total := int(n) + 8
		body := data[4 : 4+int(n)]
		crc := binary.BigEndian.Uint32(data[4+int(n) : total])
		if crc32.ChecksumIEEE(body) != crc {
			return nil, nil, ErrCorrupt
		}
		rec, err := decodeRecord(body[0], body[1:], depth, version)
		if err != nil {
			return nil, nil, ErrCorrupt
		}
		records = append(records, rec)
		data = data[total:]
	}
	return records, data, nil
}

func decodePut(p []byte) (string, []byte, error) {
	if len(p) < 8 {
		return "", nil, ErrCorrupt
	}
	keyLen := uint64(binary.BigEndian.Uint32(p[:4]))
	if keyLen > uint64(len(p)-8) {
		return "", nil, ErrCorrupt
	}
	pos := 4 + int(keyLen)
	if pos+4 > len(p) {
		return "", nil, ErrCorrupt
	}
	valueLen := uint64(binary.BigEndian.Uint32(p[pos : pos+4]))
	pos += 4
	if valueLen != uint64(len(p)-pos) {
		return "", nil, ErrCorrupt
	}
	return string(p[4 : pos-4]), append([]byte(nil), p[pos:]...), nil
}

func decodeDelete(p []byte) (string, error) {
	if len(p) < 4 {
		return "", ErrCorrupt
	}
	keyLen := uint64(binary.BigEndian.Uint32(p[:4]))
	if keyLen != uint64(len(p)-4) {
		return "", ErrCorrupt
	}
	return string(p[4:]), nil
}
