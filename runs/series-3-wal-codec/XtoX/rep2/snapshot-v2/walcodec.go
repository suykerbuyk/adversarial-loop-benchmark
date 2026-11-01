package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const maxFrameLen = 16 << 20

var ErrCorrupt = errors.New("walcodec: corrupt data")

type Record interface {
	Kind() uint8
}

type Put struct {
	Key   string
	Value []byte
}

func (Put) Kind() uint8 { return 1 }

type Delete struct {
	Key string
}

func (Delete) Kind() uint8 { return 2 }

type Batch struct {
	Records []Record
}

func (Batch) Kind() uint8 { return 3 }

func corrupt() error { return ErrCorrupt }

func appendU32(dst []byte, n uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	return append(dst, b[:]...)
}

func encodeStream(records []Record, depth int) ([]byte, error) {
	if depth > 8 {
		return nil, fmt.Errorf("encode: %w", ErrCorrupt)
	}
	out := make([]byte, 0)
	for _, record := range records {
		var typ byte
		var payload []byte
		switch r := record.(type) {
		case Put:
			typ = 1
			if uint64(len(r.Key)) > uint64(^uint32(0)) || uint64(len(r.Value)) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("encode: %w", ErrCorrupt)
			}
			payload = appendU32(payload, uint32(len(r.Key)))
			payload = append(payload, r.Key...)
			payload = appendU32(payload, uint32(len(r.Value)))
			payload = append(payload, r.Value...)
		case Delete:
			typ = 2
			if uint64(len(r.Key)) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("encode: %w", ErrCorrupt)
			}
			payload = appendU32(payload, uint32(len(r.Key)))
			payload = append(payload, r.Key...)
		case Batch:
			typ = 3
			if uint64(len(r.Records)) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("encode: %w", ErrCorrupt)
			}
			payload = appendU32(payload, uint32(len(r.Records)))
			children, err := encodeStream(r.Records, depth+1)
			if err != nil {
				return nil, err
			}
			payload = append(payload, children...)
		default:
			return nil, fmt.Errorf("encode: unsupported record %T", record)
		}
		bodyLen := 1 + len(payload)
		if bodyLen > maxFrameLen {
			return nil, fmt.Errorf("encode: frame too large")
		}
		body := make([]byte, 1, bodyLen)
		body[0] = typ
		body = append(body, payload...)
		out = appendU32(out, uint32(bodyLen))
		out = append(out, body...)
		out = appendU32(out, crc32.ChecksumIEEE(body))
	}
	return out, nil
}

func Encode(records []Record) ([]byte, error) {
	frames, err := encodeStream(records, 0)
	if err != nil {
		return nil, err
	}
	return append([]byte{2}, frames...), nil
}

func readU32(b []byte, off *int) (uint32, bool) {
	if *off < 0 || len(b)-*off < 4 {
		return 0, false
	}
	n := binary.BigEndian.Uint32(b[*off : *off+4])
	*off += 4
	return n, true
}

func takeField(p []byte, off *int) ([]byte, bool) {
	n, ok := readU32(p, off)
	if !ok || uint64(n) > uint64(len(p)-*off) {
		return nil, false
	}
	field := p[*off : *off+int(n)]
	*off += int(n)
	return field, true
}

func decodeStream(data []byte, version byte, depth int) ([]Record, error) {
	if depth > 8 {
		return nil, corrupt()
	}
	var records []Record
	for off := 0; off < len(data); {
		length, ok := readU32(data, &off)
		if !ok || length < 1 || length > maxFrameLen {
			return nil, corrupt()
		}
		if uint64(length)+4 > uint64(len(data)-off) {
			return nil, corrupt()
		}
		body := data[off : off+int(length)]
		off += int(length)
		crc, ok := readU32(data, &off)
		if !ok || crc32.ChecksumIEEE(body) != crc {
			return nil, corrupt()
		}
		typ, payload := body[0], body[1:]
		p := 0
		switch typ {
		case 1:
			key, ok := takeField(payload, &p)
			if !ok {
				return nil, corrupt()
			}
			value, ok := takeField(payload, &p)
			if !ok || p != len(payload) {
				return nil, corrupt()
			}
			records = append(records, Put{Key: string(key), Value: append([]byte(nil), value...)})
		case 2:
			key, ok := takeField(payload, &p)
			if !ok || p != len(payload) {
				return nil, corrupt()
			}
			records = append(records, Delete{Key: string(key)})
		case 3:
			if version != 2 || len(payload) < 4 {
				return nil, corrupt()
			}
			n := binary.BigEndian.Uint32(payload[:4])
			childrenData := payload[4:]
			if uint64(n) > uint64(len(childrenData))/9 {
				return nil, corrupt()
			}
			children, err := decodeChildren(childrenData, n, version, depth+1)
			if err != nil {
				return nil, err
			}
			records = append(records, Batch{Records: children})
		default:
			return nil, corrupt()
		}
	}
	return records, nil
}

func decodeChildren(data []byte, count uint32, version byte, depth int) ([]Record, error) {
	if depth > 8 {
		return nil, corrupt()
	}
	children := make([]Record, 0, int(count))
	off := 0
	for i := uint32(0); i < count; i++ {
		start := off
		length, ok := readU32(data, &off)
		if !ok || length < 1 || length > maxFrameLen || uint64(length)+4 > uint64(len(data)-off) {
			return nil, corrupt()
		}
		off += int(length) + 4
		parsed, err := decodeStream(data[start:off], version, depth)
		if err != nil || len(parsed) != 1 {
			return nil, corrupt()
		}
		children = append(children, parsed[0])
	}
	if off != len(data) {
		return nil, corrupt()
	}
	return children, nil
}

func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 || (data[0] != 1 && data[0] != 2) {
		return nil, corrupt()
	}
	return decodeStream(data[1:], data[0], 0)
}
