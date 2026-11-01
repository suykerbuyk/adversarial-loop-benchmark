// Package walcodec encodes versioned write-ahead log records as CRC-protected
// frames. Each frame has a big-endian body length, a type and payload, and a
// CRC32-IEEE; each type-plus-payload body is limited to 16 MiB.
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	version1     = 1
	version2     = 2
	kindPut      = 1
	kindDelete   = 2
	kindBatch    = 3
	maxFrameBody = 16 << 20
	maxDepth     = 8
)

var (
	// ErrCorrupt identifies malformed or unsupported encoded streams.
	ErrCorrupt = errors.New("walcodec: corrupt stream")
	// ErrUnsupportedRecord identifies a Record implementation Encode cannot encode.
	ErrUnsupportedRecord = errors.New("walcodec: unsupported record")
	// ErrRecordTooLarge identifies a frame body larger than the format limit.
	ErrRecordTooLarge = errors.New("walcodec: record too large")
)

// Record is a WAL record that can be encoded by this package.
type Record interface{ Kind() uint8 }

// Put stores a value under a key.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put record kind.
func (Put) Kind() uint8 { return kindPut }

// Delete removes a key.
type Delete struct{ Key string }

// Kind returns the Delete record kind.
func (Delete) Kind() uint8 { return kindDelete }

// Batch groups records in a nested framed sub-stream. Decoding an empty Batch
// produces a non-nil empty Records slice.
type Batch struct{ Records []Record }

// Kind returns the Batch record kind.
func (Batch) Kind() uint8 { return kindBatch }

func corrupt(offset int, why string) error {
	return fmt.Errorf("%w at byte %d: %s", ErrCorrupt, offset, why)
}
func tooLarge() error {
	return fmt.Errorf("%w: frame body exceeds %d bytes", ErrRecordTooLarge, maxFrameBody)
}

// Encode writes a version-2 stream containing Put, Delete, and nested Batch
// records in order. Batch nesting is limited to eight levels. Unsupported
// records, oversized frame bodies, and excessive nesting return errors; the
// nesting error also matches ErrCorrupt.
func Encode(records []Record) ([]byte, error) {
	total := 1
	for i, r := range records {
		s, err := frameBodySize(r, 0)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		if total > int(^uint(0)>>1)-(s+8) {
			return nil, tooLarge()
		}
		total += s + 8
	}
	out := make([]byte, 1, total)
	out[0] = version2
	for _, r := range records {
		out = appendFrame(out, r)
	}
	return out, nil
}

func frameBodySize(r Record, depth int) (int, error) {
	switch v := r.(type) {
	case Put:
		n := uint64(len(v.Key)) + uint64(len(v.Value)) + 9
		if n > maxFrameBody {
			return 0, tooLarge()
		}
		return int(n), nil
	case Delete:
		n := uint64(len(v.Key)) + 5
		if n > maxFrameBody {
			return 0, tooLarge()
		}
		return int(n), nil
	case Batch:
		if depth+1 > maxDepth {
			return 0, fmt.Errorf("%w: batch nesting exceeds %d", ErrCorrupt, maxDepth)
		}
		if uint64(len(v.Records)) > uint64(^uint32(0)) {
			return 0, tooLarge()
		}
		n := uint64(5) // type byte and uint32 record count
		for i, child := range v.Records {
			s, err := frameBodySize(child, depth+1)
			if err != nil {
				return 0, fmt.Errorf("record %d: %w", i, err)
			}
			n += uint64(s) + 8
			if n > maxFrameBody {
				return 0, tooLarge()
			}
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%w: %T", ErrUnsupportedRecord, r)
	}
}

func appendFrame(dst []byte, r Record) []byte {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	bodyStart := len(dst)
	switch v := r.(type) {
	case Put:
		dst = append(dst, kindPut)
		dst = appendU32(dst, uint32(len(v.Key)))
		dst = append(dst, v.Key...)
		dst = appendU32(dst, uint32(len(v.Value)))
		dst = append(dst, v.Value...)
	case Delete:
		dst = append(dst, kindDelete)
		dst = appendU32(dst, uint32(len(v.Key)))
		dst = append(dst, v.Key...)
	case Batch:
		dst = append(dst, kindBatch)
		dst = appendU32(dst, uint32(len(v.Records)))
		for _, child := range v.Records {
			dst = appendFrame(dst, child)
		}
	}
	bodyLen := len(dst) - bodyStart
	binary.BigEndian.PutUint32(dst[start:start+4], uint32(bodyLen))
	dst = appendU32(dst, crc32.ChecksumIEEE(dst[bodyStart:]))
	return dst
}

func appendU32(b []byte, n uint32) []byte {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], n)
	return append(b, x[:]...)
}

// Decode accepts version-1 Put/Delete streams and version-2 streams containing
// Put, Delete, and nested Batch records.
func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 {
		return nil, corrupt(0, "missing version")
	}
	version := data[0]
	if version != version1 && version != version2 {
		return nil, corrupt(0, "unsupported version")
	}
	recs, err := decodeStream(data[1:], 1, 0, version)
	if err != nil {
		return nil, err
	}
	return recs, nil
}

func decodeStream(data []byte, base, depth int, version byte) ([]Record, error) {
	recs := make([]Record, 0)
	for pos := 0; pos < len(data); {
		r, consumed, err := decodeFrame(data[pos:], base+pos, depth, version)
		if err != nil {
			return nil, err
		}
		recs = append(recs, r)
		pos += consumed
	}
	return recs, nil
}

func decodeFrame(data []byte, base, depth int, version byte) (Record, int, error) {
	if len(data) < 4 {
		return nil, 0, corrupt(base, "truncated frame length")
	}
	n := uint64(binary.BigEndian.Uint32(data[:4]))
	if n == 0 || n > maxFrameBody {
		return nil, 0, corrupt(base, "invalid frame length")
	}
	if n+8 > uint64(len(data)) {
		return nil, 0, corrupt(base, "truncated frame")
	}
	body := data[4 : 4+int(n)]
	want := binary.BigEndian.Uint32(data[4+int(n) : 8+int(n)])
	if crc32.ChecksumIEEE(body) != want {
		return nil, 0, corrupt(base, "crc mismatch")
	}
	r, err := decodeBody(body, base+4, depth, version)
	if err != nil {
		return nil, 0, err
	}
	return r, int(n) + 8, nil
}

func allowedKind(version, kind byte) bool {
	switch version {
	case version1:
		return kind == kindPut || kind == kindDelete
	case version2:
		return kind == kindPut || kind == kindDelete || kind == kindBatch
	default:
		return false
	}
}

func decodeBody(b []byte, base, depth int, version byte) (Record, error) {
	if len(b) == 0 {
		return nil, corrupt(base, "empty frame body")
	}
	if !allowedKind(version, b[0]) {
		return nil, corrupt(base, "record kind is not allowed for version")
	}
	switch b[0] {
	case kindPut:
		key, val, ok := parsePut(b[1:])
		if !ok {
			return nil, corrupt(base, "invalid Put payload")
		}
		return Put{Key: key, Value: val}, nil
	case kindDelete:
		key, ok := parseString(b[1:])
		if !ok {
			return nil, corrupt(base, "invalid Delete payload")
		}
		return Delete{Key: key}, nil
	case kindBatch:
		if depth+1 > maxDepth {
			return nil, corrupt(base, "batch nesting exceeds limit")
		}
		p := b[1:]
		if len(p) < 4 {
			return nil, corrupt(base, "truncated Batch count")
		}
		count := binary.BigEndian.Uint32(p[:4])
		payload := p[4:]
		if uint64(count) > uint64(len(payload))/9 {
			return nil, corrupt(base, "Batch count exceeds payload")
		}
		recs := make([]Record, 0)
		pos := 0
		for i := uint32(0); i < count; i++ {
			r, consumed, err := decodeFrame(payload[pos:], base+5+pos, depth+1, version)
			if err != nil {
				return nil, err
			}
			recs = append(recs, r)
			pos += consumed
		}
		if pos != len(payload) {
			return nil, corrupt(base+5+pos, "trailing Batch payload")
		}
		return Batch{Records: recs}, nil
	default:
		return nil, corrupt(base, "unknown record type")
	}
}

func parseString(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	n := uint64(binary.BigEndian.Uint32(b[:4]))
	if n != uint64(len(b)-4) {
		return "", false
	}
	return string(b[4:]), true
}
func parsePut(b []byte) (string, []byte, bool) {
	if len(b) < 8 {
		return "", nil, false
	}
	kn := uint64(binary.BigEndian.Uint32(b[:4]))
	if kn > uint64(len(b)-8) {
		return "", nil, false
	}
	i := 4 + int(kn)
	vn := uint64(binary.BigEndian.Uint32(b[i : i+4]))
	if vn != uint64(len(b)-i-4) {
		return "", nil, false
	}
	return string(b[4:i]), append([]byte{}, b[i+4:]...), true
}
