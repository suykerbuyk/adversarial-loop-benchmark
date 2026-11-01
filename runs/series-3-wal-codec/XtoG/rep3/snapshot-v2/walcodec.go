package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

var (
	ErrCorrupt = errors.New("walcodec: corrupt stream")
	ErrType    = errors.New("walcodec: unsupported record or size")
)

const maxFrame = uint64(16 << 20)

type Record interface{ Kind() uint8 }
type Put struct {
	Key   string
	Value []byte
}

func (Put) Kind() uint8 { return 1 }

type Delete struct{ Key string }

func (Delete) Kind() uint8 { return 2 }

type Batch struct{ Records []Record }

func (Batch) Kind() uint8 { return 3 }

func Encode(records []Record) ([]byte, error) {
	out := []byte{2}
	for _, r := range records {
		f, err := encodeFrame(r, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

func encodeFrame(r Record, depth int) ([]byte, error) {
	var typ byte
	var p []byte
	switch x := r.(type) {
	case Put:
		typ = 1
		if uint64(len(x.Key)) > uint64(^uint32(0)) || uint64(len(x.Value)) > uint64(^uint32(0)) {
			return nil, ErrType
		}
		p = make([]byte, 8+len(x.Key)+len(x.Value))
		binary.BigEndian.PutUint32(p, uint32(len(x.Key)))
		copy(p[4:], x.Key)
		off := 4 + len(x.Key)
		binary.BigEndian.PutUint32(p[off:], uint32(len(x.Value)))
		copy(p[off+4:], x.Value)
	case Delete:
		typ = 2
		if uint64(len(x.Key)) > uint64(^uint32(0)) {
			return nil, ErrType
		}
		p = make([]byte, 4+len(x.Key))
		binary.BigEndian.PutUint32(p, uint32(len(x.Key)))
		copy(p[4:], x.Key)
	case Batch:
		typ = 3
		if depth >= 8 {
			return nil, ErrCorrupt
		}
		if uint64(len(x.Records)) > uint64(^uint32(0)) {
			return nil, ErrType
		}
		p = make([]byte, 4)
		binary.BigEndian.PutUint32(p, uint32(len(x.Records)))
		for _, child := range x.Records {
			f, err := encodeFrame(child, depth+1)
			if err != nil {
				return nil, err
			}
			if uint64(len(p))+uint64(len(f)) > maxFrame {
				return nil, ErrType
			}
			p = append(p, f...)
		}
	default:
		return nil, ErrType
	}
	if uint64(1+len(p)) > maxFrame {
		return nil, ErrType
	}
	body := make([]byte, 1+len(p))
	body[0] = typ
	copy(body[1:], p)
	out := make([]byte, 4+len(body)+4)
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	copy(out[4:], body)
	binary.BigEndian.PutUint32(out[4+len(body):], crc32.ChecksumIEEE(body))
	return out, nil
}

func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 || (data[0] != 1 && data[0] != 2) {
		return nil, ErrCorrupt
	}
	return decodeStream(data[1:], 0, data[0])
}

func decodeStream(data []byte, depth int, version byte) ([]Record, error) {
	if depth > 8 {
		return nil, ErrCorrupt
	}
	var records []Record
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, ErrCorrupt
		}
		n := uint64(binary.BigEndian.Uint32(data[:4]))
		data = data[4:]
		if n < 1 || n > maxFrame || n+4 > uint64(len(data)) {
			return nil, ErrCorrupt
		}
		body := data[:int(n)]
		crc := binary.BigEndian.Uint32(data[int(n) : int(n)+4])
		data = data[int(n)+4:]
		if crc32.ChecksumIEEE(body) != crc {
			return nil, ErrCorrupt
		}
		r, err := decodeRecord(body[0], body[1:], depth, version)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

func decodeRecord(typ byte, p []byte, depth int, version byte) (Record, error) {
	switch typ {
	case 1:
		if len(p) < 8 {
			return nil, ErrCorrupt
		}
		kl := uint64(binary.BigEndian.Uint32(p))
		if kl > uint64(len(p)-4) {
			return nil, ErrCorrupt
		}
		off := 4 + int(kl)
		vl := uint64(binary.BigEndian.Uint32(p[off:]))
		if vl != uint64(len(p)-off-4) {
			return nil, ErrCorrupt
		}
		key := string(append([]byte(nil), p[4:off]...))
		value := append([]byte{}, p[off+4:]...)
		return Put{Key: key, Value: value}, nil
	case 2:
		if len(p) < 4 {
			return nil, ErrCorrupt
		}
		kl := uint64(binary.BigEndian.Uint32(p))
		if kl != uint64(len(p)-4) {
			return nil, ErrCorrupt
		}
		return Delete{Key: string(append([]byte(nil), p[4:]...))}, nil
	case 3:
		if version != 2 || len(p) < 4 || depth >= 8 {
			return nil, ErrCorrupt
		}
		count := uint64(binary.BigEndian.Uint32(p))
		p = p[4:]
		if count > uint64(len(p))/9 {
			return nil, ErrCorrupt
		}
		children := make([]Record, 0)
		for i := uint64(0); i < count; i++ {
			if len(p) < 4 {
				return nil, ErrCorrupt
			}
			n := uint64(binary.BigEndian.Uint32(p[:4]))
			if n < 1 || n > maxFrame || n+8 > uint64(len(p)) {
				return nil, ErrCorrupt
			}
			frameLen := 4 + int(n) + 4
			parsed, err := decodeStream(p[:frameLen], depth+1, version)
			if err != nil || len(parsed) != 1 {
				return nil, ErrCorrupt
			}
			children = append(children, parsed[0])
			p = p[frameLen:]
		}
		if len(p) != 0 {
			return nil, ErrCorrupt
		}
		return Batch{Records: children}, nil
	default:
		return nil, ErrCorrupt
	}
}
