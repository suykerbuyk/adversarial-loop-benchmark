package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
)

const (
	versionV1 = 1

	kindPut    = 1
	kindDelete = 2

	// maxFrameLen is the inclusive cap on the frame len field (type + payload).
	// Invariant: maxFrameLen <= math.MaxInt, so int(n) is well-defined after the cap check.
	maxFrameLen = 16 << 20

	lenSize  = 4
	crcSize  = 4
	typeSize = 1

	frameOverhead = lenSize + crcSize
)

// Record is one write-ahead-log entry. Concrete types return their wire tag from Kind.
type Record interface {
	Kind() uint8
}

// Put records a key and value. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete records a key removal. Kind is 2.
type Delete struct {
	Key string
}

// Kind returns the Put wire tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete wire tag.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned for any malformed stream or unencodable record.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

func init() {
	if maxFrameLen > math.MaxInt {
		panic("walcodec: maxFrameLen exceeds math.MaxInt")
	}
}

// Encode writes a version-1 stream of recs. An empty or nil recs yields a one-byte version stream.
func Encode(recs []Record) ([]byte, error) {
	if len(recs) == 0 {
		return []byte{versionV1}, nil
	}
	buf := []byte{versionV1}
	var err error
	for _, rec := range recs {
		switch r := rec.(type) {
		case Put:
			buf, err = encodePut(buf, r)
		case *Put:
			if r == nil {
				return nil, ErrCorrupt
			}
			buf, err = encodePut(buf, *r)
		case Delete:
			buf, err = encodeDelete(buf, r)
		case *Delete:
			if r == nil {
				return nil, ErrCorrupt
			}
			buf, err = encodeDelete(buf, *r)
		default:
			return nil, ErrCorrupt
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func encodePut(dst []byte, r Put) ([]byte, error) {
	if len(r.Key) > math.MaxUint32 || len(r.Value) > math.MaxUint32 {
		return nil, ErrCorrupt
	}
	payloadLen := lenSize + len(r.Key) + lenSize + len(r.Value)
	payload := make([]byte, 0, payloadLen)
	payload = appendU32(payload, uint32(len(r.Key)))
	payload = append(payload, r.Key...)
	payload = appendU32(payload, uint32(len(r.Value)))
	if len(r.Value) > 0 {
		payload = append(payload, r.Value...)
	}
	return appendFrame(dst, kindPut, payload)
}

func encodeDelete(dst []byte, r Delete) ([]byte, error) {
	if len(r.Key) > math.MaxUint32 {
		return nil, ErrCorrupt
	}
	payloadLen := lenSize + len(r.Key)
	payload := make([]byte, 0, payloadLen)
	payload = appendU32(payload, uint32(len(r.Key)))
	payload = append(payload, r.Key...)
	return appendFrame(dst, kindDelete, payload)
}

func appendFrame(dst []byte, typ byte, payload []byte) ([]byte, error) {
	payloadLen := len(payload)
	if payloadLen < 0 || payloadLen > maxFrameLen-typeSize {
		return nil, ErrCorrupt
	}
	bodyLen := typeSize + payloadLen
	dst = appendU32(dst, uint32(bodyLen))
	dst = append(dst, typ)
	dst = append(dst, payload...)
	sum := crc32.ChecksumIEEE(dst[len(dst)-bodyLen:])
	return appendU32(dst, sum), nil
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// Decode reads a version-1 stream. A missing version byte, unknown version, bad CRC,
// unknown type, truncated frame, or oversize length is ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	if b[0] != versionV1 {
		return nil, ErrCorrupt
	}
	off := 1
	out := []Record{}
	for off < len(b) {
		if len(b)-off < lenSize {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(b[off : off+lenSize])
		if n > maxFrameLen || n < 1 {
			return nil, ErrCorrupt
		}
		remain := len(b) - off
		if remain < frameOverhead || uint64(n) > uint64(remain-frameOverhead) {
			return nil, ErrCorrupt
		}
		body := b[off+lenSize : off+lenSize+int(n)]
		gotCRC := binary.BigEndian.Uint32(b[off+lenSize+int(n) : off+frameOverhead+int(n)])
		if crc32.ChecksumIEEE(body) != gotCRC {
			return nil, ErrCorrupt
		}
		typ := body[0]
		payload := body[1:]
		switch typ {
		case kindPut:
			rec, err := decodePayloadPut(payload)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		case kindDelete:
			rec, err := decodePayloadDelete(payload)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		default:
			return nil, ErrCorrupt
		}
		off += frameOverhead + int(n)
	}
	return out, nil
}

func decodePayloadPut(payload []byte) (Put, error) {
	key, next, err := takeBytes(payload, 0)
	if err != nil {
		return Put{}, err
	}
	raw, next, err := takeBytes(payload, next)
	if err != nil {
		return Put{}, err
	}
	if next != len(payload) {
		return Put{}, ErrCorrupt
	}
	return Put{Key: string(key), Value: copyValue(raw)}, nil
}

func decodePayloadDelete(payload []byte) (Delete, error) {
	key, next, err := takeBytes(payload, 0)
	if err != nil {
		return Delete{}, err
	}
	if next != len(payload) {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: string(key)}, nil
}

// takeBytes reads a uint32 length at i and the following bytes.
// i is the index of the length field, not the index after it.
func takeBytes(payload []byte, i int) ([]byte, int, error) {
	if i < 0 || i > len(payload)-lenSize {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[i : i+lenSize])
	if int(n) > len(payload)-(i+lenSize) {
		return nil, 0, ErrCorrupt
	}
	start := i + lenSize
	end := start + int(n)
	return payload[start:end], end, nil
}

func copyValue(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte{}
	}
	return append([]byte(nil), raw...)
}
