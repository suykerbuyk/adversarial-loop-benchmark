package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
)

const (
	versionV1 = 1
	versionV2 = 2

	kindPut    = 1
	kindDelete = 2
	kindBatch  = 3

	// maxFrameLen is the inclusive cap on the frame len field (type + payload).
	// Invariant: maxFrameLen <= math.MaxInt, so int(n) is well-defined after the cap check.
	maxFrameLen = 16 << 20

	// maxBatchDepth is the deepest legal sub-stream depth. The top-level stream is
	// depth 0. A Batch's inner sub-stream is one level deeper than the Batch itself.
	// A sub-stream entered at depth greater than 8 is ErrCorrupt.
	maxBatchDepth = 8

	lenSize  = 4
	crcSize  = 4
	typeSize = 1

	frameOverhead = lenSize + crcSize

	// minFrameSize is the smallest well-formed frame: len(4) + type(1) + crc(4).
	minFrameSize = frameOverhead + typeSize
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

// Batch records a nested framed sub-stream of Put, Delete, or Batch records. Kind is 3.
type Batch struct {
	Records []Record
}

// Kind returns the Put wire tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete wire tag.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch wire tag.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is returned for any malformed stream or unencodable record.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

func init() {
	if maxFrameLen > math.MaxInt {
		panic("walcodec: maxFrameLen exceeds math.MaxInt")
	}
}

// Encode writes a version-2 stream of recs. An empty or nil recs yields a one-byte version stream.
// Nesting deeper than maxBatchDepth is ErrCorrupt; Encode does not emit a stream it would reject.
func Encode(recs []Record) ([]byte, error) {
	return encodeAt(recs, 0)
}

func encodeAt(recs []Record, depth int) ([]byte, error) {
	if depth > maxBatchDepth {
		return nil, ErrCorrupt
	}
	var buf []byte
	if depth == 0 {
		buf = []byte{versionV2}
	}
	var err error
	for _, rec := range recs {
		buf, err = appendRecord(buf, rec, depth)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func appendRecord(dst []byte, rec Record, depth int) ([]byte, error) {
	switch r := rec.(type) {
	case Put:
		return encodePut(dst, r)
	case *Put:
		if r == nil {
			return nil, ErrCorrupt
		}
		return encodePut(dst, *r)
	case Delete:
		return encodeDelete(dst, r)
	case *Delete:
		if r == nil {
			return nil, ErrCorrupt
		}
		return encodeDelete(dst, *r)
	case Batch:
		return encodeBatch(dst, r, depth)
	case *Batch:
		if r == nil {
			return nil, ErrCorrupt
		}
		return encodeBatch(dst, *r, depth)
	default:
		return nil, ErrCorrupt
	}
}

func encodePut(dst []byte, r Put) ([]byte, error) {
	if uint64(len(r.Key)) > math.MaxUint32 || uint64(len(r.Value)) > math.MaxUint32 {
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
	if uint64(len(r.Key)) > math.MaxUint32 {
		return nil, ErrCorrupt
	}
	payloadLen := lenSize + len(r.Key)
	payload := make([]byte, 0, payloadLen)
	payload = appendU32(payload, uint32(len(r.Key)))
	payload = append(payload, r.Key...)
	return appendFrame(dst, kindDelete, payload)
}

func encodeBatch(dst []byte, b Batch, depth int) ([]byte, error) {
	if uint64(len(b.Records)) > math.MaxUint32 {
		return nil, ErrCorrupt
	}
	inner, err := encodeAt(b.Records, depth+1)
	if err != nil {
		return nil, err
	}
	payload := appendU32(nil, uint32(len(b.Records)))
	payload = append(payload, inner...)
	return appendFrame(dst, kindBatch, payload)
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

// Decode reads a version-1 or version-2 stream. A missing version byte, unknown version,
// bad CRC, unknown type, truncated frame, oversize length, or nesting past depth 8 is ErrCorrupt.
// Version 1 accepts only Put and Delete. Version 2 also accepts Batch.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	version := b[0]
	if version != versionV1 && version != versionV2 {
		return nil, ErrCorrupt
	}
	return decodeStream(b[1:], version, 0)
}

func decodeStream(buf []byte, version byte, depth int) ([]Record, error) {
	if depth > maxBatchDepth {
		return nil, ErrCorrupt
	}
	out := []Record{}
	off := 0
	for off < len(buf) {
		rec, next, err := decodeFrame(buf, off, version, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		off = next
	}
	return out, nil
}

func decodeFrame(buf []byte, off int, version byte, depth int) (Record, int, error) {
	if len(buf)-off < lenSize {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(buf[off : off+lenSize])
	if n > maxFrameLen || n < 1 {
		return nil, 0, ErrCorrupt
	}
	remain := len(buf) - off
	if remain < frameOverhead || uint64(n) > uint64(remain-frameOverhead) {
		return nil, 0, ErrCorrupt
	}
	body := buf[off+lenSize : off+lenSize+int(n)]
	gotCRC := binary.BigEndian.Uint32(buf[off+lenSize+int(n) : off+frameOverhead+int(n)])
	if crc32.ChecksumIEEE(body) != gotCRC {
		return nil, 0, ErrCorrupt
	}
	typ := body[0]
	payload := body[1:]
	next := off + frameOverhead + int(n)
	switch typ {
	case kindPut:
		rec, err := decodePayloadPut(payload)
		if err != nil {
			return nil, 0, err
		}
		return rec, next, nil
	case kindDelete:
		rec, err := decodePayloadDelete(payload)
		if err != nil {
			return nil, 0, err
		}
		return rec, next, nil
	case kindBatch:
		if version != versionV2 {
			return nil, 0, ErrCorrupt
		}
		rec, err := decodeBatchPayload(payload, version, depth+1)
		if err != nil {
			return nil, 0, err
		}
		return rec, next, nil
	default:
		return nil, 0, ErrCorrupt
	}
}

func decodeBatchPayload(payload []byte, version byte, childDepth int) (Batch, error) {
	if len(payload) < lenSize {
		return Batch{}, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[:lenSize])
	rest := payload[lenSize:]
	if uint64(n) > uint64(len(rest))/minFrameSize {
		return Batch{}, ErrCorrupt
	}
	inner, err := decodeStream(rest, version, childDepth)
	if err != nil {
		return Batch{}, err
	}
	if uint64(len(inner)) != uint64(n) {
		return Batch{}, ErrCorrupt
	}
	return Batch{Records: inner}, nil
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
// The length is compared as uint64 before any int narrowing so a 0xFFFFFFFF
// count cannot wrap to a negative index on 32-bit architectures.
func takeBytes(payload []byte, i int) ([]byte, int, error) {
	if i < 0 || i > len(payload)-lenSize {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[i : i+lenSize])
	avail := len(payload) - (i + lenSize)
	if uint64(n) > uint64(avail) {
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
