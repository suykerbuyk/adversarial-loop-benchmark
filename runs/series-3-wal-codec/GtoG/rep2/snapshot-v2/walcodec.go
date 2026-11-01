package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Record is one write-ahead-log entry. Concrete types return their wire tag from Kind.
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

// Batch groups nested records. Kind is 3. Its payload is a framed sub-stream.
type Batch struct {
	Records []Record
}

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch type tag.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is returned for every rejected stream. It is not wrapped.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1   = 1
	versionV2   = 2
	kindPut     = 1
	kindDelete  = 2
	kindBatch   = 3
	maxFrameLen = 16 << 20 // inclusive cap on type+payload; CRC is outside this
	maxDepth    = 8        // a sub-stream deeper than this is rejected
)

// encodeErr is caller misuse: a nil interface, a nil *Put/*Delete/*Batch, or a foreign Record.
// It is distinct from ErrCorrupt so a bad stream is not confused with a bad record.
var encodeErr = errors.New("walcodec: unknown record")

// errTooLarge is a record whose type+payload exceeds the 16 MiB frame cap, or whose
// key, value, or batch count does not fit in uint32. It is not ErrCorrupt and is not encodeErr.
var errTooLarge = errors.New("walcodec: record exceeds size limit")

// errTooDeep is a Batch tree that would place a sub-stream past depth 8.
// It is not ErrCorrupt: the input is caller data, not a corrupt stream.
var errTooDeep = errors.New("walcodec: batch nesting exceeds depth 8")

// Encode writes a version-2 stream. Empty input is a one-byte version-only stream.
// On any error the returned slice is nil. A tree deeper than the depth cap is not emitted.
func Encode(recs []Record) ([]byte, error) {
	buf := []byte{versionV2}
	for _, rec := range recs {
		frame, err := encodeRecord(rec, 0)
		if err != nil {
			return nil, err
		}
		buf = append(buf, frame...)
	}
	return buf, nil
}

func encodeRecord(rec Record, depth int) ([]byte, error) {
	switch r := rec.(type) {
	case Put:
		return encodePutDelete(kindPut, r.Key, r.Value, true)
	case *Put:
		if r == nil {
			return nil, encodeErr
		}
		return encodePutDelete(kindPut, r.Key, r.Value, true)
	case Delete:
		return encodePutDelete(kindDelete, r.Key, nil, false)
	case *Delete:
		if r == nil {
			return nil, encodeErr
		}
		return encodePutDelete(kindDelete, r.Key, nil, false)
	case Batch:
		return encodeBatch(r.Records, depth)
	case *Batch:
		if r == nil {
			return nil, encodeErr
		}
		return encodeBatch(r.Records, depth)
	case nil:
		return nil, encodeErr
	default:
		return nil, encodeErr
	}
}

func encodePutDelete(typ byte, key string, value []byte, hasVal bool) ([]byte, error) {
	keyLen := len(key)
	valLen := 0
	if hasVal {
		valLen = len(value)
	}
	// Compare in uint64. math.MaxUint32 does not fit in int on 32-bit GOARCH,
	// and a key or value longer than uint32 must not be converted.
	if uint64(keyLen) > uint64(^uint32(0)) || uint64(valLen) > uint64(^uint32(0)) {
		return nil, errTooLarge
	}

	// Body size is type + payload. Add in uint64 so keyLen+valLen cannot wrap in int.
	body := 1 + 4 + uint64(keyLen)
	if hasVal {
		body += 4 + uint64(valLen)
	}
	if body > uint64(maxFrameLen) || body > uint64(^uint32(0)) {
		return nil, errTooLarge
	}

	// One buffer: len(4) | type(1) | payload | crc(4). body is proven <= maxFrameLen.
	out := make([]byte, 4+int(body)+4)
	binary.BigEndian.PutUint32(out[0:4], uint32(body))
	out[4] = typ
	off := 5
	binary.BigEndian.PutUint32(out[off:off+4], uint32(keyLen))
	off += 4
	off += copy(out[off:], key)
	if hasVal {
		binary.BigEndian.PutUint32(out[off:off+4], uint32(valLen))
		off += 4
		copy(out[off:], value)
	}
	binary.BigEndian.PutUint32(out[4+int(body):], crc32.ChecksumIEEE(out[4:4+int(body)]))
	return out, nil
}

func encodeBatch(recs []Record, depth int) ([]byte, error) {
	// Opening this Batch's payload is one level deeper than the Batch frame itself.
	if depth+1 > maxDepth {
		return nil, errTooDeep
	}
	n := len(recs)
	if uint64(n) > uint64(^uint32(0)) {
		return nil, errTooLarge
	}

	// Encode inners first. Any inner error aborts before a partial outer frame is built.
	inner := make([]byte, 0)
	var innerLen uint64
	for _, rec := range recs {
		frame, err := encodeRecord(rec, depth+1)
		if err != nil {
			return nil, err
		}
		innerLen += uint64(len(frame))
		if innerLen > uint64(maxFrameLen) {
			return nil, errTooLarge
		}
		inner = append(inner, frame...)
	}

	// Body is type(1) + n(4) + inner frames. Reject before allocation if it cannot fit.
	body := 1 + 4 + innerLen
	if body > uint64(maxFrameLen) || body > uint64(^uint32(0)) {
		return nil, errTooLarge
	}

	out := make([]byte, 4+int(body)+4)
	binary.BigEndian.PutUint32(out[0:4], uint32(body))
	out[4] = kindBatch
	binary.BigEndian.PutUint32(out[5:9], uint32(n))
	copy(out[9:9+len(inner)], inner)
	binary.BigEndian.PutUint32(out[4+int(body):], crc32.ChecksumIEEE(out[4:4+int(body)]))
	return out, nil
}

// Decode parses a version-1 or version-2 stream. Every rejection returns (nil, ErrCorrupt).
// A version-only stream returns a non-nil empty slice.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	ver := b[0]
	if ver != versionV1 && ver != versionV2 {
		return nil, ErrCorrupt
	}
	return decodeFrames(b[1:], 0, ver)
}

// decodeFrames walks a framed sub-stream at the given depth. The buffer is only the
// enclosing payload (or the top-level tail), so an inner len cannot see a sibling or outer frame.
func decodeFrames(buf []byte, depth int, ver byte) ([]Record, error) {
	if depth > maxDepth {
		return nil, ErrCorrupt
	}
	out := make([]Record, 0)
	i := 0
	for i < len(buf) {
		if len(buf)-i < 4 {
			return nil, ErrCorrupt
		}
		frameLen := binary.BigEndian.Uint32(buf[i : i+4])
		// Cap and minimum length are checked before any allocation sized from frameLen.
		if frameLen > maxFrameLen || frameLen < 1 {
			return nil, ErrCorrupt
		}
		remaining := len(buf) - (i + 4)
		if uint64(remaining) < uint64(frameLen)+4 {
			return nil, ErrCorrupt
		}
		// i stays an int. frameLen+4 fits in int after the cap and remaining-length
		// checks, so the slice end and the cursor step are added in int.
		bodyEnd := i + 4 + int(frameLen)
		body := buf[i+4 : bodyEnd]
		wantCRC := binary.BigEndian.Uint32(buf[bodyEnd : bodyEnd+4])
		if crc32.ChecksumIEEE(body) != wantCRC {
			return nil, ErrCorrupt
		}
		rec, err := decodePayload(body[0], body[1:], depth, ver)
		if err != nil {
			return nil, ErrCorrupt
		}
		out = append(out, rec)
		i = bodyEnd + 4
	}
	return out, nil
}

// decodePayload requires the payload to be consumed exactly. Unknown types are corrupt.
func decodePayload(typ byte, payload []byte, depth int, ver byte) (Record, error) {
	switch typ {
	case kindPut:
		key, tail, ok := takeString(payload)
		if !ok {
			return nil, ErrCorrupt
		}
		val, tail2, ok := takeBytes(tail)
		if !ok || len(tail2) != 0 {
			return nil, ErrCorrupt
		}
		return Put{Key: key, Value: val}, nil
	case kindDelete:
		key, tail, ok := takeString(payload)
		if !ok || len(tail) != 0 {
			return nil, ErrCorrupt
		}
		return Delete{Key: key}, nil
	case kindBatch:
		if ver != versionV2 {
			return nil, ErrCorrupt
		}
		if len(payload) < 4 {
			return nil, ErrCorrupt
		}
		// A Batch frame at depth 8 is legal; opening its sub-stream would be depth 9.
		if depth+1 > maxDepth {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(payload[:4])
		inner, err := decodeFrames(payload[4:], depth+1, ver)
		if err != nil {
			return nil, ErrCorrupt
		}
		if uint64(len(inner)) != uint64(n) {
			return nil, ErrCorrupt
		}
		return Batch{Records: inner}, nil
	default:
		return nil, ErrCorrupt
	}
}

func takeString(buf []byte) (string, []byte, bool) {
	raw, tail, _, ok := takeLen(buf)
	if !ok {
		return "", nil, false
	}
	return string(raw), tail, true
}

func takeBytes(buf []byte) ([]byte, []byte, bool) {
	raw, tail, n, ok := takeLen(buf)
	if !ok {
		return nil, nil, false
	}
	if n == 0 {
		return []byte{}, tail, true
	}
	return append([]byte(nil), raw...), tail, true
}

// takeLen reads a uint32 length and the following bytes. n is compared in uint64
// so a length above MaxInt32 cannot pass a wrapped int comparison.
func takeLen(buf []byte) (raw []byte, tail []byte, n uint32, ok bool) {
	if len(buf) < 4 {
		return nil, nil, 0, false
	}
	n = binary.BigEndian.Uint32(buf[:4])
	rest := buf[4:]
	if uint64(n) > uint64(len(rest)) {
		return nil, nil, 0, false
	}
	end := int(n)
	return rest[:end], rest[end:], n, true
}
