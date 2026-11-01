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

// Batch groups records. Its payload is itself a framed sub-stream, and it may
// contain Put, Delete, or nested Batch values. Kind is 3.
type Batch struct {
	Records []Record
}

// Kind returns the Put wire tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete wire tag.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch wire tag.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is returned for any stream the decoder cannot accept, and for
// Encode inputs that would not decode.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1
	versionV2 = 2

	kindPut    = 1
	kindDelete = 2
	kindBatch  = 3

	// maxFrameLen caps type+payload. The trailing CRC is outside this count.
	maxFrameLen = 16 << 20

	// maxNestingDepth is the deepest legal sub-stream. The top-level stream is
	// depth 0. A Batch's inner sub-stream sits one level deeper than the stream
	// that contains the Batch frame. Depth 8 is legal; depth 9 is not. That is
	// eight nested Batch levels around a record, and a ninth is rejected.
	maxNestingDepth = 8

	headerSize = 4
	crcSize    = 4
	countSize  = 4

	// minFrameSize is the smallest structurally complete frame: len, a type
	// byte, and crc. Used only to reject an inner count that cannot fit.
	minFrameSize = headerSize + 1 + crcSize

	// batchOverhead is the encoded size of an empty Batch frame:
	// len + type + n + crc.
	batchOverhead = headerSize + 1 + countSize + crcSize

	// maxEncodedFrame is the largest frame Encode will emit at the top level,
	// where there is no parent payload budget: len + type+payload + crc.
	maxEncodedFrame = headerSize + maxFrameLen + crcSize
)

// Encode writes a version-2 stream: a version byte, then one frame per record
// in slice order. A nil or empty recs yields a one-byte, version-only stream.
// A stream that this decoder would reject is not emitted.
func Encode(recs []Record) ([]byte, error) {
	buf := []byte{versionV2}
	for _, rec := range recs {
		frame, err := encodeRecord(rec, 0, maxEncodedFrame)
		if err != nil {
			return nil, ErrCorrupt
		}
		buf = append(buf, frame...)
	}
	return buf, nil
}

// encodeRecord encodes one record at stream depth. maxEncoded is the maximum
// full-frame size (header, body, and crc) the caller can accept. Top-level
// callers pass maxEncodedFrame. A Batch passes the bytes still available in
// its payload so an over-cap child fails before any further records are encoded
// and before an over-cap Put or Delete payload is allocated.
func encodeRecord(rec Record, depth int, maxEncoded int) ([]byte, error) {
	if rec == nil {
		return nil, ErrCorrupt
	}
	switch r := rec.(type) {
	case Put:
		return encodePut(r, maxEncoded)
	case *Put:
		if r == nil {
			return nil, ErrCorrupt
		}
		return encodePut(*r, maxEncoded)
	case Delete:
		return encodeDelete(r, maxEncoded)
	case *Delete:
		if r == nil {
			return nil, ErrCorrupt
		}
		return encodeDelete(*r, maxEncoded)
	case Batch:
		return encodeBatch(r.Records, depth, maxEncoded)
	case *Batch:
		if r == nil {
			return nil, ErrCorrupt
		}
		return encodeBatch(r.Records, depth, maxEncoded)
	default:
		return nil, ErrCorrupt
	}
}

func encodePut(r Put, maxEncoded int) ([]byte, error) {
	if err := putFits(r, maxEncoded); err != nil {
		return nil, err
	}
	return wrapFrame(kindPut, encodePutPayload(r), maxEncoded)
}

func encodeDelete(r Delete, maxEncoded int) ([]byte, error) {
	if err := deleteFits(r, maxEncoded); err != nil {
		return nil, err
	}
	return wrapFrame(kindDelete, encodeDeletePayload(r), maxEncoded)
}

// encodeBatch writes n and exactly len(records) inner frames. The running
// total is checked after each child so a long Records slice stops at the first
// child that would push the enclosing payload past the frame cap, instead of
// encoding the rest of the slice first.
func encodeBatch(records []Record, depth int, maxEncoded int) ([]byte, error) {
	// Opening this Batch's sub-stream would sit at depth+1.
	if depth+1 > maxNestingDepth {
		return nil, ErrCorrupt
	}
	if maxEncoded < batchOverhead {
		return nil, ErrCorrupt
	}
	if uint64(len(records)) > uint64(^uint32(0)) {
		return nil, ErrCorrupt
	}

	// Inner frames occupy the Batch payload after the 4-byte count. The
	// enclosing frame's type+payload must also stay within maxFrameLen, and
	// the whole frame must stay within the caller's budget.
	innerBudget := maxFrameLen - 1 - countSize
	if remain := maxEncoded - batchOverhead; remain < innerBudget {
		innerBudget = remain
	}

	// Grow with the encoded children. Reserving innerBudget here would
	// allocate nearly 16 MiB for every Batch, including an empty one.
	// childMax is the bytes still available under the frame cap, so a
	// child that would pass that cap is rejected before it is encoded
	// and this buffer never grows past the cap.
	var frames []byte
	for _, rec := range records {
		childMax := innerBudget - len(frames)
		frame, err := encodeRecord(rec, depth+1, childMax)
		if err != nil {
			return nil, err
		}
		if len(frame) > childMax {
			return nil, ErrCorrupt
		}
		frames = append(frames, frame...)
	}

	payload := make([]byte, countSize+len(frames))
	binary.BigEndian.PutUint32(payload[:countSize], uint32(len(records)))
	copy(payload[countSize:], frames)
	return wrapFrame(kindBatch, payload, maxEncoded)
}

func wrapFrame(typ byte, payload []byte, maxEncoded int) ([]byte, error) {
	n64 := uint64(1) + uint64(len(payload))
	if n64 > uint64(maxFrameLen) {
		return nil, ErrCorrupt
	}
	n := int(n64)
	total := headerSize + n + crcSize
	if total > maxEncoded {
		return nil, ErrCorrupt
	}

	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[:headerSize], uint32(n))
	frame[headerSize] = typ
	copy(frame[headerSize+1:], payload)
	binary.BigEndian.PutUint32(frame[headerSize+n:], crc32.ChecksumIEEE(frame[headerSize:headerSize+n]))
	return frame, nil
}

// putFits reports whether a Put's encoded frame stays within maxFrameLen and
// maxEncoded, and each length prefix fits in uint32. It inspects lengths only,
// so an over-cap record is rejected before any payload allocation.
func putFits(r Put, maxEncoded int) error {
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
	total := uint64(headerSize) + n64 + uint64(crcSize)
	if total > uint64(maxEncoded) {
		return ErrCorrupt
	}
	return nil
}

// deleteFits reports whether a Delete's encoded frame stays within maxFrameLen
// and maxEncoded, and the key length fits in uint32, without allocating the payload.
func deleteFits(r Delete, maxEncoded int) error {
	klen := len(r.Key)
	if uint64(klen) > uint64(^uint32(0)) {
		return ErrCorrupt
	}
	// type(1) + klen(4) + key
	n64 := uint64(1+4) + uint64(klen)
	if n64 > uint64(maxFrameLen) {
		return ErrCorrupt
	}
	total := uint64(headerSize) + n64 + uint64(crcSize)
	if total > uint64(maxEncoded) {
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

// Decode reads a version-1 or version-2 stream. Version 1 accepts Put and
// Delete only. Version 2 also accepts nested Batch records. On any failure it
// returns nil, ErrCorrupt. Key strings and value slices own their bytes.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	var allowBatch bool
	switch b[0] {
	case versionV1:
		allowBatch = false
	case versionV2:
		allowBatch = true
	default:
		return nil, ErrCorrupt
	}
	recs, err := decodeStream(b[1:], 0, allowBatch)
	if err != nil {
		return nil, ErrCorrupt
	}
	return recs, nil
}

// decodeStream reads frames until window is exhausted. depth is the nesting
// depth of this sub-stream. The window is the only byte range frames may use.
func decodeStream(window []byte, depth int, allowBatch bool) ([]Record, error) {
	if depth > maxNestingDepth {
		return nil, ErrCorrupt
	}
	var out []Record
	for len(window) > 0 {
		tp, next, err := readFrame(window)
		if err != nil {
			return nil, err
		}
		rec, err := decodeTP(tp, depth, allowBatch)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		window = next
	}
	return out, nil
}

// readFrame parses one frame from window. It returns ErrCorrupt when the
// header is short, len is 0 or above 16 MiB, or the window does not contain
// len plus the crc. It does not allocate a buffer of the claimed size, and it
// does not read past window.
func readFrame(window []byte) (tp, rest []byte, err error) {
	if len(window) < headerSize {
		return nil, nil, ErrCorrupt
	}
	n32 := binary.BigEndian.Uint32(window[:headerSize])
	if n32 < 1 || n32 > maxFrameLen {
		return nil, nil, ErrCorrupt
	}
	n := int(n32)
	need := headerSize + n + crcSize
	if len(window) < need {
		return nil, nil, ErrCorrupt
	}
	tp = window[headerSize : headerSize+n]
	gotCRC := binary.BigEndian.Uint32(window[headerSize+n : need])
	if crc32.ChecksumIEEE(tp) != gotCRC {
		return nil, nil, ErrCorrupt
	}
	return tp, window[need:], nil
}

func decodeTP(tp []byte, depth int, allowBatch bool) (Record, error) {
	if len(tp) < 1 {
		return nil, ErrCorrupt
	}
	typ := tp[0]
	payload := tp[1:]
	switch typ {
	case kindPut:
		return decodePut(payload)
	case kindDelete:
		return decodeDelete(payload)
	case kindBatch:
		if !allowBatch {
			return nil, ErrCorrupt
		}
		return decodeBatch(payload, depth+1, allowBatch)
	default:
		return nil, ErrCorrupt
	}
}

// decodeBatch reads n, then exactly n frames from the remainder of payload.
// streamDepth is the depth of this Batch's inner sub-stream. A count that
// cannot fit in the payload is rejected before any record slice of that count
// is allocated. Frames are parsed only from the enclosing payload.
func decodeBatch(payload []byte, streamDepth int, allowBatch bool) (Batch, error) {
	if streamDepth > maxNestingDepth {
		return Batch{}, ErrCorrupt
	}
	if len(payload) < countSize {
		return Batch{}, ErrCorrupt
	}
	n32 := binary.BigEndian.Uint32(payload[:countSize])
	rest := payload[countSize:]
	// A complete frame is at least minFrameSize bytes. A larger count cannot
	// be satisfied from this payload, so reject it without allocating n slots
	// and without looping on the claimed count.
	if uint64(n32) > uint64(len(rest))/uint64(minFrameSize) {
		return Batch{}, ErrCorrupt
	}
	out := make([]Record, 0)
	for i := uint32(0); i < n32; i++ {
		tp, next, err := readFrame(rest)
		if err != nil {
			return Batch{}, err
		}
		rec, err := decodeTP(tp, streamDepth, allowBatch)
		if err != nil {
			return Batch{}, err
		}
		out = append(out, rec)
		rest = next
	}
	if len(rest) != 0 {
		return Batch{}, ErrCorrupt
	}
	return Batch{Records: out}, nil
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
