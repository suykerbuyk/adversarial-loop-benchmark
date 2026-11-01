package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const maxFrameLen = 16 << 20
const maxDepth = 8

var ErrCorrupt = errors.New("corrupt wal stream")

type Record interface {
	Kind() byte
}

type Put struct {
	Key   string
	Value []byte
}

func (Put) Kind() byte { return 1 }

type Delete struct {
	Key string
}

func (Delete) Kind() byte { return 2 }

type Batch struct {
	Records []Record
}

func (Batch) Kind() byte { return 3 }

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

func Encode(records []Record) ([]byte, error) {
	out := []byte{2}
	frames, err := encodeRecords(records, 0)
	if err != nil {
		return nil, err
	}
	return append(out, frames...), nil
}

func encodeRecords(records []Record, depth int) ([]byte, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("batch nesting exceeds %d", maxDepth)
	}
	out := make([]byte, 0)
	for _, rec := range records {
		var payload []byte
		var kind byte
		switch r := rec.(type) {
		case Put:
			kind = 1
			if uint64(len(r.Key)) > uint64(^uint32(0)) || uint64(len(r.Value)) > uint64(^uint32(0)) {
				return nil, errors.New("wal record too large")
			}
			payload = make([]byte, 8+len(r.Key)+len(r.Value))
			binary.BigEndian.PutUint32(payload, uint32(len(r.Key)))
			copy(payload[4:], r.Key)
			pos := 4 + len(r.Key)
			binary.BigEndian.PutUint32(payload[pos:], uint32(len(r.Value)))
			copy(payload[pos+4:], r.Value)
		case Delete:
			kind = 2
			if uint64(len(r.Key)) > uint64(^uint32(0)) {
				return nil, errors.New("wal record too large")
			}
			payload = make([]byte, 4+len(r.Key))
			binary.BigEndian.PutUint32(payload, uint32(len(r.Key)))
			copy(payload[4:], r.Key)
		case Batch:
			kind = 3
			if depth >= maxDepth {
				return nil, fmt.Errorf("batch nesting exceeds %d", maxDepth)
			}
			if uint64(len(r.Records)) > uint64(^uint32(0)) {
				return nil, errors.New("too many wal records")
			}
			inner, err := encodeRecords(r.Records, depth+1)
			if err != nil {
				return nil, err
			}
			payload = make([]byte, 4, 4+len(inner))
			binary.BigEndian.PutUint32(payload, uint32(len(r.Records)))
			payload = append(payload, inner...)
		default:
			return nil, fmt.Errorf("unsupported wal record %T", rec)
		}
		bodyLen := 1 + len(payload)
		if bodyLen > maxFrameLen {
			return nil, errors.New("wal frame exceeds 16 MiB")
		}
		body := make([]byte, bodyLen)
		body[0] = kind
		copy(body[1:], payload)
		frame := make([]byte, 4+bodyLen+4)
		binary.BigEndian.PutUint32(frame, uint32(bodyLen))
		copy(frame[4:], body)
		binary.BigEndian.PutUint32(frame[4+bodyLen:], crc32.ChecksumIEEE(body))
		out = append(out, frame...)
	}
	return out, nil
}

func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 {
		return nil, corrupt("missing version")
	}
	version := data[0]
	if version != 1 && version != 2 {
		return nil, corrupt("unsupported version %d", version)
	}
	return decodeRecords(data[1:], 0, version)
}

func decodeRecords(data []byte, depth int, version byte) ([]Record, error) {
	if depth > maxDepth {
		return nil, corrupt("batch nesting exceeds %d", maxDepth)
	}
	var records []Record
	for pos := 0; pos < len(data); {
		if len(data)-pos < 4 {
			return nil, corrupt("truncated frame length")
		}
		n := binary.BigEndian.Uint32(data[pos:])
		if n > maxFrameLen {
			return nil, corrupt("frame length exceeds limit")
		}
		if n < 1 {
			return nil, corrupt("empty frame body")
		}
		end := uint64(pos) + 4 + uint64(n) + 4
		if end > uint64(len(data)) {
			return nil, corrupt("truncated frame")
		}
		body := data[pos+4 : pos+4+int(n)]
		want := binary.BigEndian.Uint32(data[pos+4+int(n) : int(end)])
		if crc32.ChecksumIEEE(body) != want {
			return nil, corrupt("checksum mismatch")
		}
		payload := body[1:]
		var rec Record
		switch body[0] {
		case 1:
			r, err := decodePut(payload)
			if err != nil {
				return nil, err
			}
			rec = r
		case 2:
			r, err := decodeDelete(payload)
			if err != nil {
				return nil, err
			}
			rec = r
		case 3:
			if version != 2 {
				return nil, corrupt("batch in version 1 stream")
			}
			if depth >= maxDepth {
				return nil, corrupt("batch nesting exceeds %d", maxDepth)
			}
			r, err := decodeBatch(payload, depth+1, version)
			if err != nil {
				return nil, err
			}
			rec = r
		default:
			return nil, corrupt("unknown record kind %d", body[0])
		}
		records = append(records, rec)
		pos = int(end)
	}
	return records, nil
}

func decodePut(p []byte) (Put, error) {
	if len(p) < 8 {
		return Put{}, corrupt("truncated put payload")
	}
	kl := uint64(binary.BigEndian.Uint32(p))
	if kl > uint64(len(p)-4) {
		return Put{}, corrupt("truncated put key")
	}
	pos := 4 + int(kl)
	if len(p)-pos < 4 {
		return Put{}, corrupt("truncated put value length")
	}
	vl := uint64(binary.BigEndian.Uint32(p[pos:]))
	pos += 4
	if vl != uint64(len(p)-pos) {
		return Put{}, corrupt("invalid put value length")
	}
	return Put{Key: string(p[4 : pos-4]), Value: append([]byte(nil), p[pos:]...)}, nil
}

func decodeDelete(p []byte) (Delete, error) {
	if len(p) < 4 {
		return Delete{}, corrupt("truncated delete payload")
	}
	n := uint64(binary.BigEndian.Uint32(p))
	if n != uint64(len(p)-4) {
		return Delete{}, corrupt("invalid delete key length")
	}
	return Delete{Key: string(p[4:])}, nil
}

func decodeBatch(p []byte, depth int, version byte) (Batch, error) {
	if len(p) < 4 {
		return Batch{}, corrupt("truncated batch count")
	}
	count := binary.BigEndian.Uint32(p)
	remaining := len(p) - 4
	if uint64(count) > uint64(remaining/9) {
		return Batch{}, corrupt("impossible batch count")
	}
	data := p[4:]
	var records []Record
	pos := 0
	for i := uint32(0); i < count; i++ {
		if len(data)-pos < 4 {
			return Batch{}, corrupt("truncated batch frame")
		}
		n := binary.BigEndian.Uint32(data[pos:])
		if n > maxFrameLen {
			return Batch{}, corrupt("frame length exceeds limit")
		}
		end := uint64(pos) + 4 + uint64(n) + 4
		if end > uint64(len(data)) {
			return Batch{}, corrupt("truncated batch frame")
		}
		one, err := decodeRecords(data[pos:int(end)], depth, version)
		if err != nil {
			return Batch{}, err
		}
		if len(one) != 1 {
			return Batch{}, corrupt("invalid batch frame")
		}
		records = append(records, one[0])
		pos = int(end)
	}
	if pos != len(data) {
		return Batch{}, corrupt("trailing batch bytes")
	}
	return Batch{Records: records}, nil
}
