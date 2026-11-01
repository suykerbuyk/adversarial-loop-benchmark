// Package walcodec encodes and decodes versioned write-ahead-log streams.
package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	encodeVersion uint8 = 2
	version1      uint8 = 1
	version2      uint8 = 2
	putKind       uint8 = 1
	deleteKind    uint8 = 2
	batchKind     uint8 = 3
	maxFrameLen         = 16 << 20
	maxNestDepth        = 8
)

var ErrCorrupt = errors.New("walcodec: corrupt stream")

type Record interface{ Kind() uint8 }

type Put struct {
	Key   string
	Value []byte
}

func (Put) Kind() uint8 { return putKind }

type Delete struct{ Key string }

func (Delete) Kind() uint8 { return deleteKind }

type Batch struct{ Records []Record }

func (Batch) Kind() uint8 { return batchKind }

var errEncode = errors.New("walcodec: cannot encode record")

func Encode(recs []Record) ([]byte, error) {
	out := []byte{encodeVersion}
	var err error
	out, err = appendRecords(out, recs, 0)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func appendRecords(out []byte, recs []Record, depth int) ([]byte, error) {
	for _, rec := range recs {
		start := len(out)
		out = append(out, 0, 0, 0, 0)
		bodyStart := len(out)
		var key string
		var value []byte
		var kind uint8
		switch r := rec.(type) {
		case Put:
			kind, key, value = putKind, r.Key, r.Value
		case *Put:
			if r == nil {
				return nil, errEncode
			}
			kind, key, value = putKind, r.Key, r.Value
		case Delete:
			kind, key = deleteKind, r.Key
		case *Delete:
			if r == nil {
				return nil, errEncode
			}
			kind, key = deleteKind, r.Key
		case Batch:
			kind = batchKind
			if depth+1 > maxNestDepth {
				return nil, errEncode
			}
			if uint64(len(r.Records)) > uint64(^uint32(0)) {
				return nil, errEncode
			}
			out = append(out, kind)
			var count [4]byte
			binary.BigEndian.PutUint32(count[:], uint32(len(r.Records)))
			out = append(out, count[:]...)
			var err error
			out, err = appendRecords(out, r.Records, depth+1)
			if err != nil {
				return nil, err
			}
		case *Batch:
			if r == nil {
				return nil, errEncode
			}
			kind = batchKind
			if depth+1 > maxNestDepth {
				return nil, errEncode
			}
			if uint64(len(r.Records)) > uint64(^uint32(0)) {
				return nil, errEncode
			}
			out = append(out, kind)
			var count [4]byte
			binary.BigEndian.PutUint32(count[:], uint32(len(r.Records)))
			out = append(out, count[:]...)
			var err error
			out, err = appendRecords(out, r.Records, depth+1)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errEncode
		}
		if kind != batchKind {
			if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
				return nil, errEncode
			}
			out = append(out, kind)
			out = appendU32Bytes(out, []byte(key))
			if kind == putKind {
				out = appendU32Bytes(out, value)
			}
		}
		bodyLen := uint64(len(out) - bodyStart)
		if bodyLen == 0 || bodyLen > maxFrameLen || bodyLen > uint64(^uint32(0)) {
			return nil, errEncode
		}
		binary.BigEndian.PutUint32(out[start:start+4], uint32(bodyLen))
		crc := crc32.ChecksumIEEE(out[bodyStart:])
		out = append(out, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(out[len(out)-4:], crc)
	}
	return out, nil
}

func appendU32Bytes(dst, data []byte) []byte {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(len(data)))
	dst = append(dst, p[:]...)
	return append(dst, data...)
}

func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 || (b[0] != version1 && b[0] != version2) {
		return nil, ErrCorrupt
	}
	return decodeStream(b[1:], 0, b[0])
}

func decodeStream(b []byte, depth int, version uint8) ([]Record, error) {
	if depth > maxNestDepth {
		return nil, ErrCorrupt
	}
	pos := 0
	var records []Record
	for pos < len(b) {
		if len(b)-pos < 4 {
			return nil, ErrCorrupt
		}
		bodyLen := uint64(binary.BigEndian.Uint32(b[pos : pos+4]))
		pos += 4
		if bodyLen == 0 || bodyLen > maxFrameLen || bodyLen+4 > uint64(len(b)-pos) {
			return nil, ErrCorrupt
		}
		end := uint64(pos) + bodyLen
		bodyEnd := int(end)
		body := b[pos:bodyEnd]
		crc := binary.BigEndian.Uint32(b[bodyEnd : bodyEnd+4])
		if crc32.ChecksumIEEE(body) != crc {
			return nil, ErrCorrupt
		}
		record, err := decodeBody(body, depth, version)
		if err != nil {
			return nil, ErrCorrupt
		}
		records = append(records, record)
		pos = bodyEnd + 4
	}
	return records, nil
}

func decodeBody(body []byte, depth int, version uint8) (Record, error) {
	kind := body[0]
	if kind == batchKind {
		if version != version2 || depth+1 > maxNestDepth || len(body) < 5 {
			return nil, ErrCorrupt
		}
		payload := body[1:]
		n := binary.BigEndian.Uint32(payload[:4])
		remaining := payload[4:]
		if uint64(n) > uint64(len(remaining))/13 {
			return nil, ErrCorrupt
		}
		pos := 0
		children := make([]Record, 0)
		for i := uint32(0); i < n; i++ {
			if len(remaining)-pos < 4 {
				return nil, ErrCorrupt
			}
			frameLen := uint64(binary.BigEndian.Uint32(remaining[pos : pos+4]))
			if frameLen == 0 || frameLen > maxFrameLen || frameLen+8 > uint64(len(remaining)-pos) {
				return nil, ErrCorrupt
			}
			frameSize := int(frameLen) + 8
			bodyStart := pos + 4
			bodyEnd := bodyStart + int(frameLen)
			innerBody := remaining[bodyStart:bodyEnd]
			wantCRC := binary.BigEndian.Uint32(remaining[bodyEnd : bodyEnd+4])
			if crc32.ChecksumIEEE(innerBody) != wantCRC {
				return nil, ErrCorrupt
			}
			child, err := decodeBody(innerBody, depth+1, version)
			if err != nil {
				return nil, ErrCorrupt
			}
			children = append(children, child)
			pos += frameSize
		}
		if pos != len(remaining) {
			return nil, ErrCorrupt
		}
		return Batch{Records: children}, nil
	}
	pos := 1
	keyBytes, ok := readField(body, &pos)
	if !ok {
		return nil, ErrCorrupt
	}
	key := string(keyBytes)
	switch kind {
	case putKind:
		valueBytes, ok := readField(body, &pos)
		if !ok || pos != len(body) {
			return nil, ErrCorrupt
		}
		value := append([]byte(nil), valueBytes...)
		return Put{Key: key, Value: value}, nil
	case deleteKind:
		if pos != len(body) {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	default:
		return nil, ErrCorrupt
	}
}

func readField(b []byte, pos *int) ([]byte, bool) {
	if *pos < 0 || len(b)-*pos < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32(b[*pos : *pos+4]))
	*pos += 4
	if n > uint64(len(b)-*pos) {
		return nil, false
	}
	start := *pos
	*pos += int(n)
	return b[start:*pos], true
}
