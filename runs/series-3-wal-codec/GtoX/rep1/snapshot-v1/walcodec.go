package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Record is a log entry. Concrete types return a stable type tag from Kind.
type Record interface {
	Kind() uint8
}

// Put is a key/value write. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete is a key removal. Kind is 2.
type Delete struct {
	Key string
}

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is the only error the codec returns for a bad stream or a bad encode input.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1

	kindPut    uint8 = 1
	kindDelete uint8 = 2

	// maxFrameLen caps type+payload. The trailing CRC is outside this length.
	maxFrameLen = 16 << 20

	crcLen = 4
	u32Len = 4
)

// Encode writes a version-1 stream. recs nil and recs empty are the same.
// Unknown record types and frames over the 16 MiB cap return ErrCorrupt.
// The frame length is checked before the payload is copied, so a rejected
// Put or Delete does not allocate proportional to its key or value.
func Encode(recs []Record) ([]byte, error) {
	buf := make([]byte, 0, 1+len(recs)*16)
	buf = append(buf, versionV1)
	for _, rec := range recs {
		var err error
		buf, err = appendFrame(buf, rec)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// appendFrame writes one frame. It rejects an oversize body before copying
// key or value bytes into the output.
func appendFrame(dst []byte, rec Record) ([]byte, error) {
	if rec == nil {
		return nil, ErrCorrupt
	}
	switch r := rec.(type) {
	case Put:
		return appendPutFrame(dst, r)
	case Delete:
		return appendDeleteFrame(dst, r)
	default:
		return nil, ErrCorrupt
	}
}

func appendPutFrame(dst []byte, r Put) ([]byte, error) {
	// type(1) + klen(4) + key + vlen(4) + value
	frameBodyLen, ok := frameLen(1+u32Len+u32Len, len(r.Key), len(r.Value))
	if !ok {
		return nil, ErrCorrupt
	}
	dst = appendU32(dst, uint32(frameBodyLen))
	start := len(dst)
	dst = append(dst, kindPut)
	dst = appendCounted(dst, r.Key)
	dst = appendCountedBytes(dst, r.Value)
	dst = appendU32(dst, crc32.ChecksumIEEE(dst[start:]))
	return dst, nil
}

func appendDeleteFrame(dst []byte, r Delete) ([]byte, error) {
	// type(1) + klen(4) + key
	frameBodyLen, ok := frameLen(1+u32Len, len(r.Key))
	if !ok {
		return nil, ErrCorrupt
	}
	dst = appendU32(dst, uint32(frameBodyLen))
	start := len(dst)
	dst = append(dst, kindDelete)
	dst = appendCounted(dst, r.Key)
	dst = appendU32(dst, crc32.ChecksumIEEE(dst[start:]))
	return dst, nil
}

// frameLen returns  header + sum(parts) when that total is in (0, maxFrameLen].
// Overflow or a total above the cap returns ok == false without allocating.
func frameLen(header int, parts ...int) (int, bool) {
	n := header
	if n < 1 || n > maxFrameLen {
		return 0, false
	}
	for _, p := range parts {
		if p < 0 || p > maxFrameLen-n {
			return 0, false
		}
		n += p
	}
	return n, true
}

func appendCounted(dst []byte, s string) []byte {
	dst = appendU32(dst, uint32(len(s)))
	return append(dst, s...)
}

func appendCountedBytes(dst []byte, b []byte) []byte {
	dst = appendU32(dst, uint32(len(b)))
	return append(dst, b...)
}

// Decode reads a version-1 stream. Order is stream order.
// A missing version byte, an unknown version, a bad frame, or a bad payload
// returns ErrCorrupt. Oversize lengths are rejected before any body allocation.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	if b[0] != versionV1 {
		return nil, ErrCorrupt
	}

	recs := make([]Record, 0)
	off := 1
	for off < len(b) {
		if len(b)-off < u32Len {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(b[off : off+u32Len])
		if n < 1 || n > maxFrameLen {
			return nil, ErrCorrupt
		}
		bodyEnd := off + u32Len + int(n)
		crcEnd := bodyEnd + crcLen
		if crcEnd < off || crcEnd > len(b) {
			return nil, ErrCorrupt
		}
		body := b[off+u32Len : bodyEnd]
		got := crc32.ChecksumIEEE(body)
		want := binary.BigEndian.Uint32(b[bodyEnd:crcEnd])
		if got != want {
			return nil, ErrCorrupt
		}

		rec, err := decodeRecord(body[0], body[1:])
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off = crcEnd
	}
	return recs, nil
}

func decodeRecord(typ uint8, payload []byte) (Record, error) {
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
	key, rest, err := readCounted(payload)
	if err != nil {
		return Put{}, err
	}
	val, rest, err := readCounted(rest)
	if err != nil {
		return Put{}, err
	}
	if len(rest) != 0 {
		return Put{}, ErrCorrupt
	}
	return Put{Key: string(key), Value: val}, nil
}

func decodeDelete(payload []byte) (Delete, error) {
	key, rest, err := readCounted(payload)
	if err != nil {
		return Delete{}, err
	}
	if len(rest) != 0 {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: string(key)}, nil
}

// readCounted reads a big-endian length and that many bytes.
// A zero length returns a non-nil empty slice so nil and empty values decode equal.
func readCounted(b []byte) (data, rest []byte, err error) {
	if len(b) < u32Len {
		return nil, nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b[:u32Len])
	rest = b[u32Len:]
	if uint64(n) > uint64(len(rest)) {
		return nil, nil, ErrCorrupt
	}
	raw := rest[:n]
	rest = rest[n:]
	data = make([]byte, n)
	copy(data, raw)
	return data, rest, nil
}

func appendU32(dst []byte, v uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	return append(dst, tmp[:]...)
}
