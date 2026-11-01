package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Record is the log entry interface. Concrete types return their type tag from Kind.
type Record interface {
	Kind() uint8
}

// Put is a key/value write. Kind() returns 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete is a key removal. Kind() returns 2.
type Delete struct {
	Key string
}

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// Encode writes a version-1 stream: version byte, then one frame per record in order.
func Encode(recs []Record) ([]byte, error) {
	buf := []byte{versionV1}
	if len(recs) == 0 {
		return buf, nil
	}
	for _, r := range recs {
		tag, ok := kindOf(r)
		if !ok {
			return nil, errBadRecord
		}
		payload, err := encodePayload(r)
		if err != nil {
			return nil, err
		}
		var aerr error
		buf, aerr = appendFrame(buf, tag, payload)
		if aerr != nil {
			return nil, aerr
		}
	}
	return buf, nil
}

// Decode parses a version-1 stream back into records in stream order.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	if b[0] != versionV1 {
		return nil, ErrCorrupt
	}
	rest := b[1:]
	out := make([]Record, 0)
	for len(rest) > 0 {
		if len(rest) < 4 {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(rest[:4])
		rest = rest[4:]
		if n > maxFrameLen || n < 1 {
			return nil, ErrCorrupt
		}
		if uint64(len(rest)) < uint64(n)+4 {
			return nil, ErrCorrupt
		}
		body := rest[:n]
		crcBytes := rest[n : n+4]
		rest = rest[n+4:]
		if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(crcBytes) {
			return nil, ErrCorrupt
		}
		typ := body[0]
		payload := body[1:]
		if typ != kindPut && typ != kindDelete {
			return nil, ErrCorrupt
		}
		rec, ok := decodePayload(typ, payload)
		if !ok {
			return nil, ErrCorrupt
		}
		out = append(out, rec)
	}
	return out, nil
}

// ErrCorrupt is the decode-time sentinel for a malformed stream.
// Callers compare with errors.Is. Encode does not return ErrCorrupt.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1   uint8  = 1
	kindPut     uint8  = 1
	kindDelete  uint8  = 2
	maxFrameLen uint32 = 16 << 20
)

var errBadRecord = errors.New("walcodec: record is not Put or Delete")

func kindOf(r Record) (uint8, bool) {
	switch r.(type) {
	case Put:
		if r.Kind() != kindPut {
			return 0, false
		}
		return kindPut, true
	case Delete:
		if r.Kind() != kindDelete {
			return 0, false
		}
		return kindDelete, true
	default:
		return 0, false
	}
}

func appendU32(dst []byte, v uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	return append(dst, tmp[:]...)
}

func encodePayload(r Record) ([]byte, error) {
	switch x := r.(type) {
	case Put:
		payload := appendU32(nil, uint32(len(x.Key)))
		payload = append(payload, x.Key...)
		payload = appendU32(payload, uint32(len(x.Value)))
		payload = append(payload, x.Value...)
		return payload, nil
	case Delete:
		payload := appendU32(nil, uint32(len(x.Key)))
		payload = append(payload, x.Key...)
		return payload, nil
	default:
		return nil, errBadRecord
	}
}

func appendFrame(dst []byte, typ byte, payload []byte) ([]byte, error) {
	if len(payload) > int(maxFrameLen)-1 {
		return nil, errBadRecord
	}
	bodyLen := 1 + len(payload)
	dst = appendU32(dst, uint32(bodyLen))
	dst = append(dst, typ)
	dst = append(dst, payload...)
	sum := crc32.ChecksumIEEE(dst[len(dst)-bodyLen:])
	return appendU32(dst, sum), nil
}

func decodePayload(typ byte, payload []byte) (Record, bool) {
	if len(payload) < 4 {
		return nil, false
	}
	klen := binary.BigEndian.Uint32(payload[:4])
	if uint64(klen) > uint64(len(payload)-4) {
		return nil, false
	}
	key := string(payload[4 : 4+klen])
	tail := payload[4+klen:]
	switch typ {
	case kindDelete:
		if len(tail) != 0 {
			return nil, false
		}
		return Delete{Key: key}, true
	case kindPut:
		if len(tail) < 4 {
			return nil, false
		}
		vlen := binary.BigEndian.Uint32(tail[:4])
		if uint64(vlen) != uint64(len(tail)-4) {
			return nil, false
		}
		var value []byte
		if vlen > 0 {
			value = append([]byte(nil), tail[4:]...)
		}
		return Put{Key: key, Value: value}, true
	default:
		return nil, false
	}
}
