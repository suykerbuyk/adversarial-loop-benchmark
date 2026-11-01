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

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned for every rejected stream. It is not wrapped.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1   = 1
	kindPut     = 1
	kindDelete  = 2
	maxFrameLen = 16 << 20 // inclusive cap on type+payload; CRC is outside this
)

// encodeErr is caller misuse: a nil interface, a nil *Put/*Delete, or a foreign Record.
// It is distinct from ErrCorrupt so a bad stream is not confused with a bad record.
var encodeErr = errors.New("walcodec: unknown record")

// errTooLarge is a record whose type+payload exceeds the 16 MiB frame cap, or whose
// key or value length does not fit in uint32. It is not ErrCorrupt and is not encodeErr.
var errTooLarge = errors.New("walcodec: record exceeds size limit")

// Encode writes a version-1 stream. Empty input is a one-byte version-only stream.
// On any error the returned slice is nil.
func Encode(recs []Record) ([]byte, error) {
	buf := []byte{versionV1}
	for _, rec := range recs {
		frame, err := encodeRecord(rec)
		if err != nil {
			return nil, err
		}
		buf = append(buf, frame...)
	}
	return buf, nil
}

func encodeRecord(rec Record) ([]byte, error) {
	var (
		typ    byte
		key    string
		value  []byte
		hasVal bool
	)
	switch r := rec.(type) {
	case Put:
		typ = kindPut
		key = r.Key
		value = r.Value
		hasVal = true
	case *Put:
		if r == nil {
			return nil, encodeErr
		}
		typ = kindPut
		key = r.Key
		value = r.Value
		hasVal = true
	case Delete:
		typ = kindDelete
		key = r.Key
	case *Delete:
		if r == nil {
			return nil, encodeErr
		}
		typ = kindDelete
		key = r.Key
	case nil:
		return nil, encodeErr
	default:
		return nil, encodeErr
	}

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

// Decode parses a version-1 stream. Every rejection returns (nil, ErrCorrupt).
// A version-only stream returns a non-nil empty slice.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	if b[0] != versionV1 {
		return nil, ErrCorrupt
	}

	out := make([]Record, 0)
	i := 1
	for i < len(b) {
		if len(b)-i < 4 {
			return nil, ErrCorrupt
		}
		frameLen := binary.BigEndian.Uint32(b[i : i+4])
		// Cap and minimum length are checked before any allocation sized from frameLen.
		if frameLen > maxFrameLen || frameLen < 1 {
			return nil, ErrCorrupt
		}
		remaining := len(b) - (i + 4)
		if uint64(remaining) < uint64(frameLen)+4 {
			return nil, ErrCorrupt
		}
		// i stays an int. frameLen+4 fits in int after the cap and remaining-length
		// checks, so the slice end and the cursor step are added in int.
		bodyEnd := i + 4 + int(frameLen)
		body := b[i+4 : bodyEnd]
		wantCRC := binary.BigEndian.Uint32(b[bodyEnd : bodyEnd+4])
		if crc32.ChecksumIEEE(body) != wantCRC {
			return nil, ErrCorrupt
		}
		rec, err := decodePayload(body[0], body[1:])
		if err != nil {
			return nil, ErrCorrupt
		}
		out = append(out, rec)
		i = bodyEnd + 4
	}
	return out, nil
}

// decodePayload requires the payload to be consumed exactly. Unknown types are corrupt.
func decodePayload(typ byte, payload []byte) (Record, error) {
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
