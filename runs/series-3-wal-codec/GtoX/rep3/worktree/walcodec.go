package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ErrCorrupt is returned for any stream Decode cannot accept: a missing or
// unknown version, a truncated frame, an oversize length, a CRC mismatch, an
// unknown type tag, a payload that does not match its length prefixes, or a
// Batch nested past the depth cap.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1
	versionV2 = 2

	kindPut    = 1
	kindDelete = 2
	kindBatch  = 3

	// maxFrameLen is the inclusive cap on the frame len field (type + payload).
	maxFrameLen = 16 << 20 // 16 MiB

	// maxDepth is the deepest legal framed sub-stream. The top-level stream is
	// depth 0; a Batch payload is decoded one level deeper than the Batch frame.
	// A sub-stream at depth greater than 8 is rejected.
	maxDepth = 8

	lenSize = 4
	crcSize = 4

	// minFrameSize is the smallest well-formed frame: a 4-byte len, a 1-byte
	// type, and a 4-byte CRC. A Batch count larger than the window can hold
	// at this size is corrupt and must not size an allocation.
	minFrameSize = lenSize + 1 + crcSize

	// minEncodedFrame is the smallest frame Encode emits: an empty Delete or
	// an empty Batch (len + type + 4-byte payload + crc).
	minEncodedFrame = lenSize + 1 + lenSize + crcSize

	// maxBatchInner is the most bytes of inner frames a Batch payload can
	// hold. The frame len counts the type byte, the uint32 count, and the
	// inner frames, and is capped at maxFrameLen.
	maxBatchInner = maxFrameLen - 1 - lenSize

	// maxFrameWire is the largest on-wire frame: the len prefix, a body at
	// the cap, and the CRC.
	maxFrameWire = lenSize + maxFrameLen + crcSize

	// maxInt is the largest int. Used to saturate a top-level size budget
	// without overflowing a product of count and per-frame size.
	maxInt = int(^uint(0) >> 1)
)

// Record is one write-ahead-log entry. Concrete types return their wire tag
// from Kind.
type Record interface {
	Kind() uint8
}

// Put is a key/value write. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes a key. Kind is 2.
type Delete struct {
	Key string
}

// Batch is a nested framed sub-stream of records. Kind is 3.
type Batch struct {
	Records []Record
}

// Kind returns the Put wire tag. A value receiver so both Put and *Put
// satisfy Record.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete wire tag. A value receiver so both Delete and
// *Delete satisfy Record.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch wire tag. A value receiver so both Batch and *Batch
// satisfy Record.
func (Batch) Kind() uint8 { return kindBatch }

// Encode writes a version-2 stream: a version byte, then one frame per record
// in slice order. A nil or empty slice encodes as the version byte alone.
//
// A nil element, an unknown concrete type, a length that does not fit in
// uint32, a frame that would exceed maxFrameLen, or a Batch nested past the
// depth cap returns (nil, error). Those errors are ordinary errors, not
// ErrCorrupt. The encoder refuses a stream it would itself reject.
func Encode(recs []Record) ([]byte, error) {
	// Size the stream before allocating. An oversized Batch, an over-deep
	// nest, a nil element, or an unknown type returns here with no output
	// buffer and no payload copy.
	size, err := measureFrames(recs, 0, maxInt)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+size)
	out = append(out, versionV2)
	return encodeFrames(out, recs, 0)
}

// encodeFrames appends one frame per record. depth is the depth of this
// sub-stream: 0 at the top level, one greater inside each Batch. Callers
// measure first, so this walk only runs for a stream that fits.
func encodeFrames(dst []byte, recs []Record, depth int) ([]byte, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("walcodec: batch nesting exceeds depth %d", maxDepth)
	}
	for i, rec := range recs {
		var err error
		dst, err = encodeOne(dst, rec, i, depth)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

func encodeOne(dst []byte, rec Record, index, depth int) ([]byte, error) {
	if rec == nil {
		return nil, fmt.Errorf("walcodec: nil record at index %d", index)
	}
	switch r := rec.(type) {
	case Put:
		return appendPut(dst, r)
	case *Put:
		if r == nil {
			return nil, fmt.Errorf("walcodec: nil record at index %d", index)
		}
		return appendPut(dst, *r)
	case Delete:
		return appendDelete(dst, r)
	case *Delete:
		if r == nil {
			return nil, fmt.Errorf("walcodec: nil record at index %d", index)
		}
		return appendDelete(dst, *r)
	case Batch:
		return appendBatch(dst, r, depth)
	case *Batch:
		if r == nil {
			return nil, fmt.Errorf("walcodec: nil record at index %d", index)
		}
		return appendBatch(dst, *r, depth)
	default:
		return nil, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
}

func appendPut(dst []byte, r Put) ([]byte, error) {
	n, err := putPayloadLen(r)
	if err != nil {
		return nil, err
	}
	if err := frameCapError(n); err != nil {
		return nil, err
	}
	payload := make([]byte, 0, n)
	payload = appendU32(payload, uint32(len(r.Key)))
	payload = append(payload, r.Key...)
	payload = appendU32(payload, uint32(len(r.Value))) // nil and empty both write 0
	payload = append(payload, r.Value...)
	return appendFrame(dst, kindPut, payload)
}

func appendDelete(dst []byte, r Delete) ([]byte, error) {
	n, err := deletePayloadLen(r)
	if err != nil {
		return nil, err
	}
	if err := frameCapError(n); err != nil {
		return nil, err
	}
	payload := make([]byte, 0, n)
	payload = appendU32(payload, uint32(len(r.Key)))
	payload = append(payload, r.Key...)
	return appendFrame(dst, kindDelete, payload)
}

// appendBatch writes a Batch frame. The inner sub-stream is one level deeper
// than the Batch frame. The inner size is measured against maxBatchInner
// before any of those bytes are appended, so an aggregate past the 16 MiB
// cap does not allocate a payload.
func appendBatch(dst []byte, r Batch, depth int) ([]byte, error) {
	n := len(r.Records)
	if !fitsU32(n) {
		return nil, fmt.Errorf("walcodec: batch record count exceeds uint32")
	}
	innerSize, err := measureFrames(r.Records, depth+1, maxBatchInner)
	if err != nil {
		return nil, err
	}
	payloadLen, ok := addInt(lenSize, innerSize)
	if !ok {
		return nil, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	if err := frameCapError(payloadLen); err != nil {
		return nil, err
	}
	bodyLen := 1 + payloadLen
	dst = appendU32(dst, uint32(bodyLen))
	bodyStart := len(dst)
	dst = append(dst, kindBatch)
	dst = appendU32(dst, uint32(n))
	innerStart := len(dst)
	dst, err = encodeFrames(dst, r.Records, depth+1)
	if err != nil {
		return nil, err
	}
	if len(dst)-innerStart != innerSize {
		return nil, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	sum := crc32.ChecksumIEEE(dst[bodyStart:])
	return appendU32(dst, sum), nil
}

func putPayloadLen(r Put) (int, error) {
	keyLen := len(r.Key)
	valLen := len(r.Value) // nil and empty both report 0
	if !fitsU32(keyLen) || !fitsU32(valLen) {
		return 0, fmt.Errorf("walcodec: key or value length exceeds uint32")
	}
	n, ok := addInt(lenSize, keyLen)
	if !ok {
		return 0, fmt.Errorf("walcodec: key or value length exceeds uint32")
	}
	n, ok = addInt(n, lenSize)
	if !ok {
		return 0, fmt.Errorf("walcodec: key or value length exceeds uint32")
	}
	n, ok = addInt(n, valLen)
	if !ok {
		return 0, fmt.Errorf("walcodec: key or value length exceeds uint32")
	}
	return n, nil
}

func deletePayloadLen(r Delete) (int, error) {
	keyLen := len(r.Key)
	if !fitsU32(keyLen) {
		return 0, fmt.Errorf("walcodec: key length exceeds uint32")
	}
	n, ok := addInt(lenSize, keyLen)
	if !ok {
		return 0, fmt.Errorf("walcodec: key length exceeds uint32")
	}
	return n, nil
}

// measureFrames reports the on-wire size of recs encoded at depth, or an
// error if that encoding would be rejected. budget is the maximum total
// on-wire size of those frames. The walk does not allocate.
func measureFrames(recs []Record, depth, budget int) (int, error) {
	if depth > maxDepth {
		return 0, fmt.Errorf("walcodec: batch nesting exceeds depth %d", maxDepth)
	}
	if budget < 0 {
		return 0, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	// Each encoded frame is at least minEncodedFrame bytes. A count that
	// cannot fit in the enclosing budget is rejected before the records are
	// visited, so a huge slice cannot be walked only to fail the cap.
	if len(recs) > 0 && (budget < minEncodedFrame || len(recs) > budget/minEncodedFrame) {
		return 0, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	total := 0
	for i, rec := range recs {
		sz, err := measureFrame(rec, i, depth, budget-total)
		if err != nil {
			return 0, err
		}
		next, ok := addInt(total, sz)
		if !ok {
			return 0, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
		}
		total = next
	}
	return total, nil
}

// measureFrame reports the on-wire size of one record. wireLimit is the
// maximum on-wire size the enclosing payload still has room for.
func measureFrame(rec Record, index, depth, wireLimit int) (int, error) {
	if rec == nil {
		return 0, fmt.Errorf("walcodec: nil record at index %d", index)
	}
	switch r := rec.(type) {
	case Put:
		return measurePut(r, wireLimit)
	case *Put:
		if r == nil {
			return 0, fmt.Errorf("walcodec: nil record at index %d", index)
		}
		return measurePut(*r, wireLimit)
	case Delete:
		return measureDelete(r, wireLimit)
	case *Delete:
		if r == nil {
			return 0, fmt.Errorf("walcodec: nil record at index %d", index)
		}
		return measureDelete(*r, wireLimit)
	case Batch:
		return measureBatch(r, depth, wireLimit)
	case *Batch:
		if r == nil {
			return 0, fmt.Errorf("walcodec: nil record at index %d", index)
		}
		return measureBatch(*r, depth, wireLimit)
	default:
		return 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
}

func measurePut(r Put, wireLimit int) (int, error) {
	n, err := putPayloadLen(r)
	if err != nil {
		return 0, err
	}
	return wireLenIfFits(n, wireLimit)
}

func measureDelete(r Delete, wireLimit int) (int, error) {
	n, err := deletePayloadLen(r)
	if err != nil {
		return 0, err
	}
	return wireLenIfFits(n, wireLimit)
}

func measureBatch(r Batch, depth, wireLimit int) (int, error) {
	n := len(r.Records)
	if !fitsU32(n) {
		return 0, fmt.Errorf("walcodec: batch record count exceeds uint32")
	}
	// On-wire size is the empty-batch frame plus inner frames. Inner frames
	// are also capped by maxBatchInner, so a nested Batch cannot exceed its
	// own frame cap even when the caller still has room.
	if wireLimit < minEncodedFrame {
		return 0, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	innerBudget := maxBatchInner
	if room := wireLimit - minEncodedFrame; room < innerBudget {
		innerBudget = room
	}
	inner, err := measureFrames(r.Records, depth+1, innerBudget)
	if err != nil {
		return 0, err
	}
	payloadLen, ok := addInt(lenSize, inner)
	if !ok {
		return 0, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	return wireLenIfFits(payloadLen, wireLimit)
}

// frameCapError reports whether a payload of payloadLen bytes would make the
// frame len field exceed maxFrameLen.
func frameCapError(payloadLen int) error {
	bodyLen, ok := addInt(1, payloadLen)
	if !ok || bodyLen > maxFrameLen || bodyLen < 1 {
		return fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	return nil
}

// wireLenIfFits returns the on-wire size of a frame with the given payload
// when that frame fits both maxFrameLen and wireLimit.
func wireLenIfFits(payloadLen, wireLimit int) (int, error) {
	if err := frameCapError(payloadLen); err != nil {
		return 0, err
	}
	bodyLen := 1 + payloadLen
	wire, ok := addInt(bodyLen, lenSize+crcSize)
	if !ok || wire > wireLimit || wire > maxFrameWire {
		return 0, fmt.Errorf("walcodec: frame length exceeds %d", maxFrameLen)
	}
	return wire, nil
}

// appendFrame writes len || type || payload || crc.
// len counts type+payload only; crc is CRC32-IEEE over type+payload, outside len.
func appendFrame(dst []byte, typ byte, payload []byte) ([]byte, error) {
	// len = 1 (type) + payload. Reject before writing a frame Decode must refuse.
	bodyLen := 1 + len(payload)
	if bodyLen > maxFrameLen || bodyLen < 1 {
		return nil, fmt.Errorf("walcodec: frame length %d exceeds %d", bodyLen, maxFrameLen)
	}
	dst = appendU32(dst, uint32(bodyLen))
	bodyStart := len(dst)
	dst = append(dst, typ)
	dst = append(dst, payload...)
	sum := crc32.ChecksumIEEE(dst[bodyStart:])
	dst = appendU32(dst, sum)
	return dst, nil
}

func appendU32(dst []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

// Decode reads a version-1 or version-2 stream into records in frame order.
// Version 1 accepts Put and Delete only. Version 2 also accepts Batch, nested
// up to depth 8. Duplicate keys are kept. On any corruption it returns
// (nil, ErrCorrupt) with no partial list.
//
// A nil value and an empty value both decode to a non-nil length-0 slice.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	// Version is validated before any frame is read or allocated.
	allowBatch := false
	switch b[0] {
	case versionV1:
		allowBatch = false
	case versionV2:
		allowBatch = true
	default:
		return nil, ErrCorrupt
	}
	return decodeFrames(b, 1, len(b), 0, allowBatch)
}

// decodeFrames reads every frame in b[off:end). depth is the depth of this
// sub-stream. Every read stays inside the window, so an inner length that
// would spill into a sibling or an outer CRC is truncated ErrCorrupt.
func decodeFrames(b []byte, off, end, depth int, allowBatch bool) ([]Record, error) {
	if depth > maxDepth {
		return nil, ErrCorrupt
	}
	if off < 0 || end < off || end > len(b) {
		return nil, ErrCorrupt
	}
	recs := make([]Record, 0)
	for off < end {
		rec, next, err := decodeOne(b, off, end, depth, allowBatch)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off = next
	}
	return recs, nil
}

// decodeBatch reads n and exactly n inner frames. The n frames must consume
// the payload exactly. The inner sub-stream is one level deeper than the
// Batch frame that owns this payload.
func decodeBatch(payload []byte, depth int, allowBatch bool) (Batch, error) {
	if len(payload) < lenSize {
		return Batch{}, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[:lenSize])
	// Bound the window to this payload. decodeOne never reads past end,
	// so an inner len cannot reach an outer frame's CRC.
	inner, err := decodeExactly(payload, lenSize, len(payload), depth+1, allowBatch, n)
	if err != nil {
		return Batch{}, err
	}
	if inner == nil {
		inner = []Record{}
	}
	return Batch{Records: inner}, nil
}

// decodeExactly reads want frames from b[off:end) and requires those frames
// to consume the window with nothing left over.
func decodeExactly(b []byte, off, end, depth int, allowBatch bool, want uint32) ([]Record, error) {
	if depth > maxDepth {
		return nil, ErrCorrupt
	}
	if off < 0 || end < off || end > len(b) {
		return nil, ErrCorrupt
	}
	// Each inner frame is at least minFrameSize bytes. A claimed count that
	// cannot fit in the window is corrupt. Refuse it before any allocation
	// sized from that count; a small payload claiming millions of records
	// must not grow a slice of interface slots.
	if uint64(want) > uint64(end-off)/uint64(minFrameSize) {
		return nil, ErrCorrupt
	}
	recs := make([]Record, 0, want)
	for i := uint32(0); i < want; i++ {
		if off >= end {
			return nil, ErrCorrupt
		}
		rec, next, err := decodeOne(b, off, end, depth, allowBatch)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off = next
	}
	if off != end {
		return nil, ErrCorrupt
	}
	return recs, nil
}

// decodeOne reads a single frame from b[off:end).
func decodeOne(b []byte, off, end, depth int, allowBatch bool) (Record, int, error) {
	if depth > maxDepth {
		return nil, off, ErrCorrupt
	}
	if off < 0 || end < off || end > len(b) || end-off < lenSize {
		return nil, off, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b[off : off+lenSize])
	off += lenSize

	// Cap check before any allocation sized from n. The body, when present,
	// is a subslice of the caller buffer, not a make of size n.
	if n > maxFrameLen || n == 0 {
		return nil, off, ErrCorrupt
	}
	need := int(n) + crcSize
	if end-off < need {
		return nil, off, ErrCorrupt
	}
	body := b[off : off+int(n)]
	off += int(n)
	gotCRC := binary.BigEndian.Uint32(b[off : off+crcSize])
	off += crcSize
	if crc32.ChecksumIEEE(body) != gotCRC {
		return nil, off, ErrCorrupt
	}

	typ := body[0]
	payload := body[1:]
	switch typ {
	case kindPut:
		rec, err := decodePut(payload)
		if err != nil {
			return nil, off, err
		}
		return rec, off, nil
	case kindDelete:
		rec, err := decodeDelete(payload)
		if err != nil {
			return nil, off, err
		}
		return rec, off, nil
	case kindBatch:
		if !allowBatch {
			return nil, off, ErrCorrupt
		}
		rec, err := decodeBatch(payload, depth, allowBatch)
		if err != nil {
			return nil, off, err
		}
		return rec, off, nil
	default:
		return nil, off, ErrCorrupt
	}
}

func decodePut(payload []byte) (Put, error) {
	key, off, ok := readKey(payload, 0)
	if !ok {
		return Put{}, ErrCorrupt
	}
	val, off, ok := readBytes(payload, off)
	if !ok || off != len(payload) {
		return Put{}, ErrCorrupt
	}
	return Put{Key: key, Value: val}, nil
}

func decodeDelete(payload []byte) (Delete, error) {
	key, off, ok := readKey(payload, 0)
	if !ok || off != len(payload) {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: key}, nil
}

// readKey copies a length-prefixed string. The conversion to string copies,
// so the result does not alias the input buffer.
func readKey(b []byte, off int) (string, int, bool) {
	raw, next, ok := readRaw(b, off)
	if !ok {
		return "", off, false
	}
	return string(raw), next, true
}

// readBytes copies a length-prefixed byte slice. A zero length returns a
// non-nil empty slice so a decoded nil value and a decoded empty value compare
// equal under bytes.Equal and both have length 0.
func readBytes(b []byte, off int) ([]byte, int, bool) {
	raw, next, ok := readRaw(b, off)
	if !ok {
		return nil, off, false
	}
	if len(raw) == 0 {
		return []byte{}, next, true
	}
	return append([]byte(nil), raw...), next, true
}

// readRaw returns the length-prefixed span at off. It does not allocate and
// does not accept a length that runs past b.
func readRaw(b []byte, off int) ([]byte, int, bool) {
	if off < 0 || len(b)-off < lenSize {
		return nil, off, false
	}
	n := binary.BigEndian.Uint32(b[off : off+lenSize])
	off += lenSize
	// n is bounded by the already-accepted frame; reject a claim past this payload
	// before any copy.
	if uint64(off)+uint64(n) > uint64(len(b)) {
		return nil, off, false
	}
	end := off + int(n)
	return b[off:end], end, true
}

// fitsU32 reports whether n can be stored in a uint32 length prefix.
func fitsU32(n int) bool {
	return n >= 0 && uint64(n) <= uint64(^uint32(0))
}

// addInt adds a and b, reporting overflow.
func addInt(a, b int) (int, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	c := a + b
	if c < a {
		return 0, false
	}
	return c, true
}
