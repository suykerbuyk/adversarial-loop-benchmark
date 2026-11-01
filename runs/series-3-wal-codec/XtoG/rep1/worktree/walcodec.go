package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const maxFrameLen = 16 << 20
const maxDepth = 8

var ErrCorrupt = errors.New("walcodec: corrupt stream")
var errEncode = errors.New("walcodec: cannot encode record")

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

func putU32(dst []byte, v uint32) { binary.BigEndian.PutUint32(dst, v) }

func fieldSize(n int) (uint32, bool) {
	if n < 0 || uint64(n) > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(n), true
}

// frameBodySize validates and sizes one frame, recursively checking nested batches.
func frameBodySize(r Record, depth int) (uint64, error) {
	if r == nil || depth > maxDepth {
		return 0, errEncode
	}
	switch v := r.(type) {
	case Put:
		k, ok := fieldSize(len(v.Key))
		if !ok {
			return 0, errEncode
		}
		val, ok := fieldSize(len(v.Value))
		if !ok {
			return 0, errEncode
		}
		return 1 + 4 + uint64(k) + 4 + uint64(val), nil
	case Delete:
		k, ok := fieldSize(len(v.Key))
		if !ok {
			return 0, errEncode
		}
		return 1 + 4 + uint64(k), nil
	case Batch:
		return batchBodySize(v.Records, depth)
	case *Put:
		if v == nil {
			return 0, errEncode
		}
		return frameBodySize(*v, depth)
	case *Delete:
		if v == nil {
			return 0, errEncode
		}
		return frameBodySize(*v, depth)
	case *Batch:
		if v == nil {
			return 0, errEncode
		}
		return batchBodySize(v.Records, depth)
	default:
		return 0, errEncode
	}
}

func batchBodySize(records []Record, depth int) (uint64, error) {
	// A Batch opens a sub-stream one level deeper, even when it is empty.
	if depth >= maxDepth {
		return 0, errEncode
	}
	if uint64(len(records)) > uint64(^uint32(0)) {
		return 0, errEncode
	}
	sz := uint64(1 + 4) // kind and count
	for _, rec := range records {
		body, err := frameBodySize(rec, depth+1)
		if err != nil {
			return 0, err
		}
		if body > maxFrameLen {
			return 0, errEncode
		}
		sz += 8 + body
		if sz > maxFrameLen {
			return 0, errEncode
		}
	}
	return sz, nil
}

func encodeFrame(r Record, depth int) ([]byte, error) {
	bodyLen, err := frameBodySize(r, depth)
	if err != nil || bodyLen > maxFrameLen {
		return nil, errEncode
	}
	body := make([]byte, int(bodyLen))
	var kind uint8
	switch v := r.(type) {
	case Put:
		kind = 1
		encodePutPayload(body[1:], v.Key, v.Value)
	case *Put:
		kind = 1
		encodePutPayload(body[1:], v.Key, v.Value)
	case Delete:
		kind = 2
		encodeDeletePayload(body[1:], v.Key)
	case *Delete:
		kind = 2
		encodeDeletePayload(body[1:], v.Key)
	case Batch:
		kind = 3
		encodeBatchPayload(body[1:], v.Records, depth)
	case *Batch:
		kind = 3
		encodeBatchPayload(body[1:], v.Records, depth)
	}
	body[0] = byte(kind)
	out := make([]byte, 4+len(body)+4)
	putU32(out[:4], uint32(len(body)))
	copy(out[4:], body)
	putU32(out[4+len(body):], crc32.ChecksumIEEE(body))
	return out, nil
}

func encodePutPayload(dst []byte, key string, value []byte) {
	putU32(dst[:4], uint32(len(key)))
	copy(dst[4:], key)
	p := 4 + len(key)
	putU32(dst[p:p+4], uint32(len(value)))
	copy(dst[p+4:], value)
}
func encodeDeletePayload(dst []byte, key string) {
	putU32(dst[:4], uint32(len(key)))
	copy(dst[4:], key)
}
func encodeBatchPayload(dst []byte, records []Record, depth int) {
	putU32(dst[:4], uint32(len(records)))
	p := 4
	for _, r := range records {
		frame, _ := encodeFrame(r, depth+1)
		copy(dst[p:], frame)
		p += len(frame)
	}
}

func Encode(records []Record) ([]byte, error) {
	out := []byte{2}
	for _, r := range records {
		frame, err := encodeFrame(r, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
	}
	return out, nil
}

func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 || (data[0] != 1 && data[0] != 2) {
		return nil, ErrCorrupt
	}
	recs, used, err := decodeStream(data[1:], 0, data[0])
	if err != nil || used != len(data)-1 {
		return nil, ErrCorrupt
	}
	return recs, nil
}

func decodeStream(data []byte, depth int, version byte) ([]Record, int, error) {
	if depth > maxDepth {
		return nil, 0, ErrCorrupt
	}
	out := make([]Record, 0)
	p := 0
	for p < len(data) {
		if len(data)-p < 4 {
			return nil, 0, ErrCorrupt
		}
		n := uint64(binary.BigEndian.Uint32(data[p : p+4]))
		if n < 1 || n > maxFrameLen || n+8 > uint64(len(data)-p) {
			return nil, 0, ErrCorrupt
		}
		bodyStart := p + 4
		bodyEnd := bodyStart + int(n)
		crcEnd := bodyEnd + 4
		body := data[bodyStart:bodyEnd]
		if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(data[bodyEnd:crcEnd]) {
			return nil, 0, ErrCorrupt
		}
		r, err := decodeRecord(body, depth, version)
		if err != nil {
			return nil, 0, ErrCorrupt
		}
		out = append(out, r)
		p = crcEnd
	}
	return out, p, nil
}

func decodeRecord(body []byte, depth int, version byte) (Record, error) {
	if len(body) < 1 {
		return nil, ErrCorrupt
	}
	payload := body[1:]
	switch body[0] {
	case 1:
		if len(payload) < 8 {
			return nil, ErrCorrupt
		}
		k, p, ok := takeField(payload, 0)
		if !ok {
			return nil, ErrCorrupt
		}
		v, p2, ok := takeField(payload, p)
		if !ok || p2 != len(payload) {
			return nil, ErrCorrupt
		}
		return Put{Key: string(k), Value: append([]byte(nil), v...)}, nil
	case 2:
		if len(payload) < 4 {
			return nil, ErrCorrupt
		}
		k, p, ok := takeField(payload, 0)
		if !ok || p != len(payload) {
			return nil, ErrCorrupt
		}
		return Delete{Key: string(k)}, nil
	case 3:
		if version == 1 || len(payload) < 4 || depth >= maxDepth {
			return nil, ErrCorrupt
		}
		count := uint64(binary.BigEndian.Uint32(payload[:4]))
		inner := payload[4:]
		out := make([]Record, 0, minInt(uint64(len(inner))/8, count))
		p := 0
		for i := uint64(0); i < count; i++ {
			if len(inner)-p < 4 {
				return nil, ErrCorrupt
			}
			n := uint64(binary.BigEndian.Uint32(inner[p : p+4]))
			if n < 1 || n > maxFrameLen || n+8 > uint64(len(inner)-p) {
				return nil, ErrCorrupt
			}
			end := p + 4 + int(n) + 4
			fr := inner[p:end]
			r, err := decodeFrameExact(fr, depth+1, version)
			if err != nil {
				return nil, ErrCorrupt
			}
			out = append(out, r)
			p = end
		}
		if p != len(inner) {
			return nil, ErrCorrupt
		}
		return Batch{Records: out}, nil
	default:
		return nil, ErrCorrupt
	}
}

func decodeFrameExact(frame []byte, depth int, version byte) (Record, error) {
	if len(frame) < 9 {
		return nil, ErrCorrupt
	}
	n := uint64(binary.BigEndian.Uint32(frame[:4]))
	if n < 1 || n > maxFrameLen || n+8 != uint64(len(frame)) {
		return nil, ErrCorrupt
	}
	body := frame[4 : 4+int(n)]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(frame[4+int(n):]) {
		return nil, ErrCorrupt
	}
	return decodeRecord(body, depth, version)
}

func takeField(data []byte, p int) ([]byte, int, bool) {
	if p < 0 || len(data)-p < 4 {
		return nil, 0, false
	}
	n := uint64(binary.BigEndian.Uint32(data[p : p+4]))
	p += 4
	if n > uint64(len(data)-p) {
		return nil, 0, false
	}
	end := p + int(n)
	return data[p:end], end, true
}

func minInt(a, b uint64) int {
	if a < b {
		return int(a)
	}
	return int(b)
}
