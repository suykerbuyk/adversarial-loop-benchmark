package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Record is one write-ahead-log entry. Concrete types return their wire tag
// from Kind.
type Record interface {
	Kind() uint8
}

// Put stores a key and value. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes a key. Kind is 2.
type Delete struct {
	Key string
}

// Kind returns the Put wire tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete wire tag.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned for any stream the decoder cannot accept, and for
// Encode inputs that would not decode.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1

	kindPut    = 1
	kindDelete = 2

	// maxFrameLen caps type+payload. The trailing CRC is outside this count.
	maxFrameLen = 16 << 20

	headerSize = 4
	crcSize    = 4
)

// Encode writes a version-1 stream: a version byte, then one frame per record
// in slice order. A nil or empty recs yields a one-byte, version-only stream.
func Encode(recs []Record) ([]byte, error) {
	buf := make([]byte, 0, 1+len(recs)*16)
	buf = append(buf, versionV1)
	for _, rec := range recs {
		frame, err := encodeFrame(rec)
		if err != nil {
			return nil, err
		}
		buf = append(buf, frame...)
	}
	return buf, nil
}

func encodeFrame(rec Record) ([]byte, error) {
	if rec == nil {
		return nil, ErrCorrupt
	}
	var payload []byte
	var typ byte
	switch r := rec.(type) {
	case Put:
		typ = kindPut
		if err := putFits(r); err != nil {
			return nil, err
		}
		payload = encodePutPayload(r)
	case *Put:
		if r == nil {
			return nil, ErrCorrupt
		}
		typ = kindPut
		if err := putFits(*r); err != nil {
			return nil, err
		}
		payload = encodePutPayload(*r)
	case Delete:
		typ = kindDelete
		if err := deleteFits(r); err != nil {
			return nil, err
		}
		payload = encodeDeletePayload(r)
	case *Delete:
		if r == nil {
			return nil, ErrCorrupt
		}
		typ = kindDelete
		if err := deleteFits(*r); err != nil {
			return nil, err
		}
		payload = encodeDeletePayload(*r)
	default:
		return nil, ErrCorrupt
	}

	// len counts type + payload only. The fit checks already rejected a
	// frame this decoder would refuse, before the payload was built.
	n := 1 + len(payload)

	tp := make([]byte, n)
	tp[0] = typ
	copy(tp[1:], payload)

	frame := make([]byte, headerSize+n+crcSize)
	binary.BigEndian.PutUint32(frame[0:headerSize], uint32(n))
	copy(frame[headerSize:], tp)
	binary.BigEndian.PutUint32(frame[headerSize+n:], crc32.ChecksumIEEE(tp))
	return frame, nil
}

// putFits reports whether a Put's type+payload stays within maxFrameLen and
// each length prefix fits in uint32. It inspects lengths only, so an over-cap
// record is rejected before any payload allocation.
func putFits(r Put) error {
	klen := len(r.Key)
	vlen := len(r.Value)
	if uint64(klen) > uint64(^uint32(0)) || uint64(vlen) > uint64(^uint32(0)) {
		return ErrCorrupt
	}
	// type(1) + klen(4) + key + vlen(4) + value
	n64 := uint64(1+4+4) + uint64(klen) + uint64(vlen)
	if n64 > uint64(maxFrameLen) {
		return ErrCorrupt
	}
	return nil
}

// deleteFits reports whether a Delete's type+payload stays within maxFrameLen
// and the key length fits in uint32, without allocating the payload.
func deleteFits(r Delete) error {
	klen := len(r.Key)
	if uint64(klen) > uint64(^uint32(0)) {
		return ErrCorrupt
	}
	// type(1) + klen(4) + key
	n64 := uint64(1+4) + uint64(klen)
	if n64 > uint64(maxFrameLen) {
		return ErrCorrupt
	}
	return nil
}

func encodePutPayload(r Put) []byte {
	kb := []byte(r.Key)
	vb := r.Value
	payload := make([]byte, 4+len(kb)+4+len(vb))
	binary.BigEndian.PutUint32(payload[0:4], uint32(len(kb)))
	copy(payload[4:], kb)
	off := 4 + len(kb)
	binary.BigEndian.PutUint32(payload[off:off+4], uint32(len(vb)))
	copy(payload[off+4:], vb)
	return payload
}

func encodeDeletePayload(r Delete) []byte {
	kb := []byte(r.Key)
	payload := make([]byte, 4+len(kb))
	binary.BigEndian.PutUint32(payload[0:4], uint32(len(kb)))
	copy(payload[4:], kb)
	return payload
}

// Decode reads a version-1 stream. On any failure it returns nil, ErrCorrupt.
// Key strings and value slices own their bytes.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	if b[0] != versionV1 {
		return nil, ErrCorrupt
	}

	var out []Record
	off := 1
	for off < len(b) {
		if len(b)-off < headerSize {
			return nil, ErrCorrupt
		}
		n32 := binary.BigEndian.Uint32(b[off : off+headerSize])
		// Cap check before any allocation or slice of the claimed length.
		// maxFrameLen fits in int on every Go target, so the conversion is safe
		// only after the cap check.
		if n32 < 1 || n32 > maxFrameLen {
			return nil, ErrCorrupt
		}
		n := int(n32)
		need := headerSize + n + crcSize
		if len(b)-off < need {
			return nil, ErrCorrupt
		}
		tp := b[off+headerSize : off+headerSize+n]
		gotCRC := binary.BigEndian.Uint32(b[off+headerSize+n : off+need])
		if crc32.ChecksumIEEE(tp) != gotCRC {
			return nil, ErrCorrupt
		}

		rec, err := decodeRecord(tp)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		off += need
	}
	return out, nil
}

func decodeRecord(tp []byte) (Record, error) {
	typ := tp[0]
	payload := tp[1:]
	switch typ {
	case kindPut:
		return decodePut(payload)
	case kindDelete:
		return decodeDelete(payload)
	default:
		return nil, ErrCorrupt
	}
}

func decodePut(payload []byte) (Put, error) {
	key, rest, err := readPrefixed(payload)
	if err != nil {
		return Put{}, err
	}
	val, rest, err := readPrefixed(rest)
	if err != nil {
		return Put{}, err
	}
	if len(rest) != 0 {
		return Put{}, ErrCorrupt
	}
	// A nil value and an empty value both encode as vlen 0 and both decode
	// to a length-0 slice.
	if val == nil {
		val = []byte{}
	}
	return Put{Key: string(key), Value: val}, nil
}

func decodeDelete(payload []byte) (Delete, error) {
	key, rest, err := readPrefixed(payload)
	if err != nil {
		return Delete{}, err
	}
	if len(rest) != 0 {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: string(key)}, nil
}

// readPrefixed reads a uint32 length and that many bytes. The returned slice
// owns its bytes. The length must fit in the remaining payload; no allocation
// uses an untrusted size that exceeds the already-bounded payload.
func readPrefixed(b []byte) (data, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, ErrCorrupt
	}
	n32 := binary.BigEndian.Uint32(b[0:4])
	// Payload is already capped, so n32 fits in int after this check.
	if uint64(n32) > uint64(len(b)-4) {
		return nil, nil, ErrCorrupt
	}
	n := int(n32)
	raw := b[4 : 4+n]
	data = append([]byte(nil), raw...)
	return data, b[4+n:], nil
}
