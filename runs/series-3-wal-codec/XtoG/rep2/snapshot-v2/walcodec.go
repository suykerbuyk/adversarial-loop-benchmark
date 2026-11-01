package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const maxFrameLen = 16 << 20

var (
	ErrCorrupt = errors.New("walcodec: corrupt stream")
	errEncode  = errors.New("walcodec: cannot encode record")
)

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

func Encode(records []Record) ([]byte, error) {
	out := []byte{2}
	frames, err := encodeRecords(records, 0)
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out, nil
}

func encodeRecords(records []Record, depth int) ([][]byte, error) {
	frames := make([][]byte, 0, len(records))
	for _, record := range records {
		var payload []byte
		var kind byte
		switch r := record.(type) {
		case Put:
			kind = 1
			if !validPutSize(r.Key, r.Value) {
				return nil, errEncode
			}
			payload = putPayload(r.Key, r.Value)
		case *Put:
			if r == nil {
				return nil, errEncode
			}
			kind = 1
			if !validPutSize(r.Key, r.Value) {
				return nil, errEncode
			}
			payload = putPayload(r.Key, r.Value)
		case Delete:
			kind = 2
			if uint64(len(r.Key))+5 > maxFrameLen {
				return nil, errEncode
			}
			payload = deletePayload(r.Key)
		case *Delete:
			if r == nil {
				return nil, errEncode
			}
			kind = 2
			if uint64(len(r.Key))+5 > maxFrameLen {
				return nil, errEncode
			}
			payload = deletePayload(r.Key)
		case Batch:
			kind = 3
			if depth+1 > 8 {
				return nil, errEncode
			}
			children, err := encodeRecords(r.Records, depth+1)
			if err != nil {
				return nil, err
			}
			if uint64(len(r.Records)) > uint64(^uint32(0)) {
				return nil, errEncode
			}
			payload = make([]byte, 4, 4)
			binary.BigEndian.PutUint32(payload, uint32(len(r.Records)))
			for _, child := range children {
				if uint64(len(payload))+uint64(len(child))+1 > maxFrameLen {
					return nil, errEncode
				}
				payload = append(payload, child...)
			}
		case *Batch:
			if r == nil {
				return nil, errEncode
			}
			kind = 3
			if depth+1 > 8 {
				return nil, errEncode
			}
			children, err := encodeRecords(r.Records, depth+1)
			if err != nil {
				return nil, err
			}
			if uint64(len(r.Records)) > uint64(^uint32(0)) {
				return nil, errEncode
			}
			payload = make([]byte, 4, 4)
			binary.BigEndian.PutUint32(payload, uint32(len(r.Records)))
			for _, child := range children {
				if uint64(len(payload))+uint64(len(child))+1 > maxFrameLen {
					return nil, errEncode
				}
				payload = append(payload, child...)
			}
		default:
			return nil, errEncode
		}
		if uint64(len(payload))+1 > maxFrameLen {
			return nil, errEncode
		}
		body := make([]byte, 1, 1+len(payload))
		body[0] = kind
		body = append(body, payload...)
		frame := make([]byte, 4, 8+len(body))
		binary.BigEndian.PutUint32(frame, uint32(len(body)))
		frame = append(frame, body...)
		crc := make([]byte, 4)
		binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(body))
		frame = append(frame, crc...)
		frames = append(frames, frame)
	}
	return frames, nil
}

func validPutSize(key string, value []byte) bool {
	return uint64(len(key)) <= uint64(^uint32(0)) && uint64(len(value)) <= uint64(^uint32(0)) && uint64(len(key))+uint64(len(value))+9 <= maxFrameLen
}

func putPayload(key string, value []byte) []byte {
	out := make([]byte, 4, 8+len(key)+len(value))
	binary.BigEndian.PutUint32(out, uint32(len(key)))
	out = append(out, key...)
	n := make([]byte, 4)
	binary.BigEndian.PutUint32(n, uint32(len(value)))
	out = append(out, n...)
	out = append(out, value...)
	return out
}

func deletePayload(key string) []byte {
	out := make([]byte, 4, 4+len(key))
	binary.BigEndian.PutUint32(out, uint32(len(key)))
	return append(out, key...)
}

func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 || (data[0] != 1 && data[0] != 2) {
		return nil, ErrCorrupt
	}
	return decodeRecords(data[1:], 0, data[0])
}

func decodeRecords(data []byte, depth int, version byte) ([]Record, error) {
	var records []Record
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, ErrCorrupt
		}
		n := uint64(binary.BigEndian.Uint32(data[:4]))
		if n == 0 || n > maxFrameLen {
			return nil, ErrCorrupt
		}
		if n > uint64(len(data)-4) || uint64(len(data)-4)-n < 4 {
			return nil, ErrCorrupt
		}
		body := data[4 : 4+int(n)]
		crcAt := 4 + int(n)
		if binary.BigEndian.Uint32(data[crcAt:crcAt+4]) != crc32.ChecksumIEEE(body) {
			return nil, ErrCorrupt
		}
		rec, err := decodeRecord(body[0], body[1:], depth, version)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
		data = data[crcAt+4:]
	}
	return records, nil
}

func decodeRecord(kind byte, payload []byte, depth int, version byte) (Record, error) {
	switch kind {
	case 1:
		if len(payload) < 4 {
			return nil, ErrCorrupt
		}
		kl := uint64(binary.BigEndian.Uint32(payload[:4]))
		if kl > uint64(len(payload)-4) {
			return nil, ErrCorrupt
		}
		at := 4 + int(kl)
		if len(payload)-at < 4 {
			return nil, ErrCorrupt
		}
		vl := uint64(binary.BigEndian.Uint32(payload[at : at+4]))
		at += 4
		if vl != uint64(len(payload)-at) {
			return nil, ErrCorrupt
		}
		value := append([]byte{}, payload[at:]...)
		return Put{Key: string(payload[4 : 4+int(kl)]), Value: value}, nil
	case 2:
		if len(payload) < 4 {
			return nil, ErrCorrupt
		}
		kl := uint64(binary.BigEndian.Uint32(payload[:4]))
		if kl != uint64(len(payload)-4) {
			return nil, ErrCorrupt
		}
		return Delete{Key: string(payload[4:])}, nil
	case 3:
		if version != 2 || len(payload) < 4 || depth+1 > 8 {
			return nil, ErrCorrupt
		}
		count := uint64(binary.BigEndian.Uint32(payload[:4]))
		inner := payload[4:]
		if count > uint64(len(inner))/9 {
			return nil, ErrCorrupt
		}
		records, err := decodeRecords(inner, depth+1, version)
		if err != nil || uint64(len(records)) != count {
			return nil, ErrCorrupt
		}
		return Batch{Records: records}, nil
	default:
		return nil, ErrCorrupt
	}
}
