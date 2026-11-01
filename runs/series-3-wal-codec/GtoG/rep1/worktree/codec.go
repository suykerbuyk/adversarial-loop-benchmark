package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
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

// Batch is a nested framed sub-stream. Kind() returns 3.
type Batch struct {
	Records []Record
}

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch type tag.
func (Batch) Kind() uint8 { return kindBatch }

// Encode writes a version-2 stream: version byte, then one frame per record in order.
// On any error it returns a nil buffer.
func Encode(recs []Record) ([]byte, error) {
	dst := []byte{versionV2}
	var err error
	dst, err = encodeRecords(dst, recs, 0)
	if err != nil {
		return nil, err
	}
	return dst, nil
}

// Decode parses a version-1 or version-2 stream back into records in stream order.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	version := b[0]
	if version != versionV1 && version != versionV2 {
		return nil, ErrCorrupt
	}
	recs, rest, err := decodeFrames(b[1:], version, 0, drain)
	if err != nil {
		return nil, ErrCorrupt
	}
	if len(rest) != 0 {
		return nil, ErrCorrupt
	}
	return recs, nil
}

// ErrCorrupt is the decode-time sentinel for a malformed stream.
// Callers compare with errors.Is. Encode does not return ErrCorrupt.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1   uint8  = 1
	versionV2   uint8  = 2
	kindPut     uint8  = 1
	kindDelete  uint8  = 2
	kindBatch   uint8  = 3
	maxFrameLen uint32 = 16 << 20
	maxDepth           = 8

	// drain asks the walker to read until the slice is empty.
	drain uint64 = ^uint64(0)
)

var (
	errBadRecord = errors.New("walcodec: record is not Put, Delete, or Batch")
	errDepth     = errors.New("walcodec: batch nesting exceeds 8")
	errOversize  = errors.New("walcodec: frame exceeds 16MiB")
)

// encodeRecords appends one frame per record onto dst at the given depth.
// A successful call returns the extended buffer. An error returns a nil buffer.
func encodeRecords(dst []byte, recs []Record, depth int) ([]byte, error) {
	for _, rec := range recs {
		var err error
		dst, err = appendRecord(dst, rec, depth)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// appendRecord appends one frame for rec onto dst.
// On success it returns dst with the new frame appended, never the new frame
// alone. Returning only the new frame would drop sibling frames already
// present in dst at this depth, including frames inside a nested Batch.
// On error it returns a nil buffer. Encode drops its destination on any error,
// and a nested Batch must not append an outer frame after a child failure.
func appendRecord(dst []byte, rec Record, depth int) ([]byte, error) {
	switch x := rec.(type) {
	case Put:
		return appendPut(dst, x)
	case *Put:
		if x == nil {
			return nil, errBadRecord
		}
		return appendPut(dst, *x)
	case Delete:
		return appendDelete(dst, x)
	case *Delete:
		if x == nil {
			return nil, errBadRecord
		}
		return appendDelete(dst, *x)
	case Batch:
		return appendBatch(dst, x, depth)
	case *Batch:
		if x == nil {
			return nil, errBadRecord
		}
		return appendBatch(dst, *x, depth)
	default:
		return nil, errBadRecord
	}
}

func appendPut(dst []byte, x Put) ([]byte, error) {
	if x.Kind() != kindPut {
		return nil, errBadRecord
	}
	// Compare in uint64. math.MaxUint32 does not fit in a 32-bit int, so a
	// direct len > MaxUint32 comparison does not compile on GOARCH=386/arm.
	if uint64(len(x.Key)) > uint64(math.MaxUint32) || uint64(len(x.Value)) > uint64(math.MaxUint32) {
		return nil, errBadRecord
	}
	payload := appendU32(nil, uint32(len(x.Key)))
	payload = append(payload, x.Key...)
	payload = appendU32(payload, uint32(len(x.Value)))
	payload = append(payload, x.Value...)
	return appendFrame(dst, kindPut, payload)
}

func appendDelete(dst []byte, x Delete) ([]byte, error) {
	if x.Kind() != kindDelete {
		return nil, errBadRecord
	}
	if uint64(len(x.Key)) > uint64(math.MaxUint32) {
		return nil, errBadRecord
	}
	payload := appendU32(nil, uint32(len(x.Key)))
	payload = append(payload, x.Key...)
	return appendFrame(dst, kindDelete, payload)
}

func appendBatch(dst []byte, x Batch, depth int) ([]byte, error) {
	if x.Kind() != kindBatch {
		return nil, errBadRecord
	}
	if depth+1 > maxDepth {
		return nil, errDepth
	}
	if uint64(len(x.Records)) > uint64(math.MaxUint32) {
		return nil, errBadRecord
	}
	inner, err := encodeRecords(nil, x.Records, depth+1)
	if err != nil {
		return nil, err
	}
	// The count word is 4 bytes. Refuse before adding 4 in int, and before
	// the frame body (type byte + count + children) can exceed 16 MiB.
	if uint64(len(inner)) > uint64(maxFrameLen)-1-4 {
		return nil, errOversize
	}
	payload := appendU32(nil, uint32(len(x.Records)))
	payload = append(payload, inner...)
	return appendFrame(dst, kindBatch, payload)
}

func appendU32(dst []byte, v uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	return append(dst, tmp[:]...)
}

func appendFrame(dst []byte, typ byte, payload []byte) ([]byte, error) {
	// Do not compute 1+len(payload) in int: that sum wraps at MaxInt.
	if uint64(len(payload)) > uint64(maxFrameLen)-1 {
		return nil, errOversize
	}
	bodyLen := uint32(len(payload)) + 1
	dst = appendU32(dst, bodyLen)
	dst = append(dst, typ)
	dst = append(dst, payload...)
	sum := crc32.ChecksumIEEE(dst[len(dst)-int(bodyLen):])
	return appendU32(dst, sum), nil
}

// decodeFrames reads n well-formed frames from the front of b.
// When n == drain it reads until b is empty. It never reads past len(b).
// rest is the unconsumed suffix.
func decodeFrames(b []byte, version uint8, depth int, n uint64) (recs []Record, rest []byte, err error) {
	rest = b
	var parsed uint64
	for {
		if n == drain {
			if len(rest) == 0 {
				return recs, rest, nil
			}
		} else if parsed == n {
			return recs, rest, nil
		}
		if len(rest) < 4 {
			return nil, nil, ErrCorrupt
		}
		frameLen := binary.BigEndian.Uint32(rest[:4])
		rest = rest[4:]
		if frameLen > maxFrameLen || frameLen < 1 {
			return nil, nil, ErrCorrupt
		}
		if uint64(frameLen)+uint64(4) > uint64(len(rest)) {
			return nil, nil, ErrCorrupt
		}
		body := rest[:frameLen]
		crcBytes := rest[frameLen : frameLen+4]
		rest = rest[frameLen+4:]
		if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(crcBytes) {
			return nil, nil, ErrCorrupt
		}
		typ := body[0]
		payload := body[1:]
		if !typeAllowed(version, typ) {
			return nil, nil, ErrCorrupt
		}
		rec, derr := decodePayload(typ, payload, version, depth)
		if derr != nil {
			return nil, nil, derr
		}
		recs = append(recs, rec)
		parsed++
	}
}

func typeAllowed(version, typ uint8) bool {
	switch version {
	case versionV1:
		return typ == kindPut || typ == kindDelete
	case versionV2:
		return typ == kindPut || typ == kindDelete || typ == kindBatch
	default:
		return false
	}
}

func decodePayload(typ byte, payload []byte, version uint8, depth int) (Record, error) {
	switch typ {
	case kindBatch:
		return decodeBatch(payload, version, depth)
	case kindPut, kindDelete:
		return decodePutOrDelete(typ, payload)
	default:
		return nil, ErrCorrupt
	}
}

func decodeBatch(payload []byte, version uint8, depth int) (Record, error) {
	// Depth is checked after the outer CRC (already verified) and before the
	// inner count, so a too-deep nest is ErrCorrupt even if the inner body is
	// truncated or has a bad inner CRC.
	if depth+1 > maxDepth {
		return nil, ErrCorrupt
	}
	if len(payload) < 4 {
		return nil, ErrCorrupt
	}
	n := uint64(binary.BigEndian.Uint32(payload[:4]))
	recs, rest, err := decodeFrames(payload[4:], version, depth+1, n)
	if err != nil {
		return nil, ErrCorrupt
	}
	if len(rest) != 0 {
		return nil, ErrCorrupt
	}
	return Batch{Records: recs}, nil
}

func decodePutOrDelete(typ byte, payload []byte) (Record, error) {
	if len(payload) < 4 {
		return nil, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[:4])
	if uint64(klen) > uint64(len(payload))-4 {
		return nil, ErrCorrupt
	}
	keyBytes := payload[4 : 4+klen]
	// Copy even when klen is 0 so a later edit cannot restore string(payload[...])
	// and alias a caller-owned decode buffer, including a nested key.
	key := string(append([]byte(nil), keyBytes...))
	tail := payload[4+klen:]
	switch typ {
	case kindDelete:
		if len(tail) != 0 {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	case kindPut:
		if len(tail) < 4 {
			return nil, ErrCorrupt
		}
		vlen := binary.BigEndian.Uint32(tail[:4])
		if uint64(vlen) != uint64(len(tail))-4 {
			return nil, ErrCorrupt
		}
		var value []byte
		if vlen > 0 {
			value = append([]byte(nil), tail[4:]...)
		}
		return Put{Key: key, Value: value}, nil
	default:
		return nil, ErrCorrupt
	}
}
