package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
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

// Batch is a nested framed sub-stream. Kind is 3.
type Batch struct {
	Records []Record
}

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch type tag.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is the only error the codec returns for a bad stream or a bad encode input.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1
	versionV2 = 2

	kindPut    uint8 = 1
	kindDelete uint8 = 2
	kindBatch  uint8 = 3

	// maxFrameLen caps type+payload. The trailing CRC is outside this length.
	maxFrameLen = 16 << 20

	// maxNestDepth is the deepest legal sub-stream. The top-level stream is depth 0.
	// A Batch's inner sub-stream sits at depth+1. Depth 9 is rejected.
	maxNestDepth = 8

	crcLen = 4
	u32Len = 4

	// A complete frame is len(4) + body + crc(4). Top-level calls may emit one
	// legal frame, so their budget is the largest such frame.
	topFrameBudget = maxFrameLen + u32Len + crcLen

	// Batch outer overhead that is not a child frame: type byte, count prefix,
	// len prefix, and CRC. The payload itself is count(4) + child frames, and
	// the body is type(1) + payload, which must stay within maxFrameLen.
	batchOverhead = 1 + u32Len + u32Len + crcLen
)

// Encode writes a version-2 stream. recs nil and recs empty are the same.
// Unknown record types, nesting past depth 8, and frames over the 16 MiB cap
// return ErrCorrupt. Length is checked before payload bytes are copied.
func Encode(recs []Record) ([]byte, error) {
	buf := make([]byte, 0, 1+len(recs)*16)
	buf = append(buf, versionV2)
	var err error
	buf, err = appendRecords(buf, recs, 0, topFrameBudget)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// appendRecords writes each record as a sibling frame. budget is the maximum
// size of one complete frame (len prefix + body + crc).
func appendRecords(dst []byte, recs []Record, depth int, budget int) ([]byte, error) {
	for _, rec := range recs {
		var err error
		dst, err = appendFrame(dst, rec, depth, budget)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// appendFrame writes one frame. It rejects an oversize body before copying
// key, value, or nested frame bytes into the output.
func appendFrame(dst []byte, rec Record, depth int, budget int) ([]byte, error) {
	if rec == nil {
		return nil, ErrCorrupt
	}
	switch r := rec.(type) {
	case Put:
		return appendPutFrame(dst, r, budget)
	case Delete:
		return appendDeleteFrame(dst, r, budget)
	case Batch:
		return appendBatchFrame(dst, r, depth, budget)
	default:
		return nil, ErrCorrupt
	}
}

func appendPutFrame(dst []byte, r Put, budget int) ([]byte, error) {
	// type(1) + klen(4) + key + vlen(4) + value
	frameBodyLen, ok := frameLen(1+u32Len+u32Len, len(r.Key), len(r.Value))
	if !ok || !frameFits(frameBodyLen, budget) {
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

func appendDeleteFrame(dst []byte, r Delete, budget int) ([]byte, error) {
	// type(1) + klen(4) + key
	frameBodyLen, ok := frameLen(1+u32Len, len(r.Key))
	if !ok || !frameFits(frameBodyLen, budget) {
		return nil, ErrCorrupt
	}
	dst = appendU32(dst, uint32(frameBodyLen))
	start := len(dst)
	dst = append(dst, kindDelete)
	dst = appendCounted(dst, r.Key)
	dst = appendU32(dst, crc32.ChecksumIEEE(dst[start:]))
	return dst, nil
}

func appendBatchFrame(dst []byte, r Batch, depth int, budget int) ([]byte, error) {
	if depth+1 > maxNestDepth {
		return nil, ErrCorrupt
	}
	if !countFitsU32(len(r.Records)) {
		return nil, ErrCorrupt
	}
	// type(1) + n(4) already exceeds the body cap, or the enclosing frame
	// cannot hold this Batch's fixed overhead.
	if budget < batchOverhead || 1+u32Len > maxFrameLen {
		return nil, ErrCorrupt
	}
	remain := budget - batchOverhead
	if cap := maxFrameLen - (1 + u32Len); cap < remain {
		remain = cap
	}

	scratch := make([]byte, 0)
	for _, child := range r.Records {
		childSize, ok := frameSize(child, depth+1, remain)
		if !ok || childSize > remain {
			return nil, ErrCorrupt
		}
		var err error
		scratch, err = appendFrame(scratch, child, depth+1, remain)
		if err != nil {
			return nil, err
		}
		remain -= childSize
	}

	payloadLen := u32Len + len(scratch)
	bodyLen := 1 + payloadLen
	if bodyLen > maxFrameLen || !frameFits(bodyLen, budget) {
		return nil, ErrCorrupt
	}
	dst = appendU32(dst, uint32(bodyLen))
	start := len(dst)
	dst = append(dst, kindBatch)
	dst = appendU32(dst, uint32(len(r.Records)))
	dst = append(dst, scratch...)
	dst = appendU32(dst, crc32.ChecksumIEEE(dst[start:]))
	return dst, nil
}

// countFitsU32 reports whether n fits in a uint32 count prefix.
// The comparison runs in uint64: on 32-bit targets len returns int, and an
// untyped math.MaxUint32 constant does not fit in that int.
func countFitsU32(n int) bool {
	return uint64(n) <= uint64(math.MaxUint32)
}

// frameFits reports whether a body of bodyLen plus its len prefix and CRC
// stays inside the enclosing budget.
func frameFits(bodyLen, budget int) bool {
	if bodyLen < 1 || budget < u32Len+crcLen {
		return false
	}
	return bodyLen <= budget-(u32Len+crcLen)
}

// frameSize is the complete frame size (len prefix + body + crc) of rec when
// encoded at depth, or ok == false if rec cannot be encoded within budget.
// It does not copy key, value, or nested frame bytes.
func frameSize(rec Record, depth, budget int) (int, bool) {
	if rec == nil || budget < u32Len+crcLen {
		return 0, false
	}
	switch r := rec.(type) {
	case Put:
		body, ok := frameLen(1+u32Len+u32Len, len(r.Key), len(r.Value))
		if !ok || !frameFits(body, budget) {
			return 0, false
		}
		return body + u32Len + crcLen, true
	case Delete:
		body, ok := frameLen(1+u32Len, len(r.Key))
		if !ok || !frameFits(body, budget) {
			return 0, false
		}
		return body + u32Len + crcLen, true
	case Batch:
		return batchFrameSize(r, depth, budget)
	default:
		return 0, false
	}
}

func batchFrameSize(r Batch, depth, budget int) (int, bool) {
	if depth+1 > maxNestDepth || !countFitsU32(len(r.Records)) {
		return 0, false
	}
	if budget < batchOverhead || 1+u32Len > maxFrameLen {
		return 0, false
	}
	remain := budget - batchOverhead
	if cap := maxFrameLen - (1 + u32Len); cap < remain {
		remain = cap
	}
	payload := u32Len
	for _, child := range r.Records {
		childSize, ok := frameSize(child, depth+1, remain)
		if !ok || childSize > remain {
			return 0, false
		}
		payload += childSize
		remain -= childSize
	}
	body := 1 + payload
	if body > maxFrameLen || !frameFits(body, budget) {
		return 0, false
	}
	return body + u32Len + crcLen, true
}

// frameLen returns header + sum(parts) when that total is in (0, maxFrameLen].
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

// Decode reads a version-1 or version-2 stream. Order is stream order.
// A missing version byte, an unknown version, a bad frame, or a bad payload
// returns ErrCorrupt. Oversize lengths are rejected before any body allocation.
// Version 1 accepts Put and Delete only. Version 2 also accepts Batch.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	ver := b[0]
	if ver != versionV1 && ver != versionV2 {
		return nil, ErrCorrupt
	}
	return decodeStream(b[1:], 0, ver == versionV2)
}

// decodeStream reads frames until b is exhausted. depth is the depth of this
// sub-stream. allowBatch is false for an entire version-1 walk.
func decodeStream(b []byte, depth int, allowBatch bool) ([]Record, error) {
	if depth > maxNestDepth {
		return nil, ErrCorrupt
	}
	recs := make([]Record, 0)
	off := 0
	for off < len(b) {
		rec, next, err := decodeFrame(b, off, depth, allowBatch)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off = next
	}
	return recs, nil
}

func decodeFrame(b []byte, off, depth int, allowBatch bool) (Record, int, error) {
	if len(b)-off < u32Len {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b[off : off+u32Len])
	if n < 1 || n > maxFrameLen {
		return nil, 0, ErrCorrupt
	}
	bodyEnd := off + u32Len + int(n)
	crcEnd := bodyEnd + crcLen
	if bodyEnd < off || crcEnd < bodyEnd || crcEnd > len(b) {
		return nil, 0, ErrCorrupt
	}
	body := b[off+u32Len : bodyEnd]
	got := crc32.ChecksumIEEE(body)
	want := binary.BigEndian.Uint32(b[bodyEnd:crcEnd])
	if got != want {
		return nil, 0, ErrCorrupt
	}
	rec, err := decodeRecord(body[0], body[1:], depth, allowBatch)
	if err != nil {
		return nil, 0, err
	}
	return rec, crcEnd, nil
}

func decodeRecord(typ uint8, payload []byte, depth int, allowBatch bool) (Record, error) {
	switch typ {
	case kindPut:
		return decodePut(payload)
	case kindDelete:
		return decodeDelete(payload)
	case kindBatch:
		if !allowBatch {
			return nil, ErrCorrupt
		}
		return decodeBatch(payload, depth, allowBatch)
	default:
		return nil, ErrCorrupt
	}
}

func decodeBatch(payload []byte, depth int, allowBatch bool) (Batch, error) {
	if len(payload) < u32Len {
		return Batch{}, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[:u32Len])
	inner, err := decodeStream(payload[u32Len:], depth+1, allowBatch)
	if err != nil {
		return Batch{}, err
	}
	if uint64(len(inner)) != uint64(n) {
		return Batch{}, ErrCorrupt
	}
	if inner == nil {
		inner = make([]Record, 0)
	}
	return Batch{Records: inner}, nil
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
