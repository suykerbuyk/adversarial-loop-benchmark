package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// --- encoding ---

// normalize is the only place that resolves a record's dynamic type. It
// returns Put, Delete and Batch values unchanged, dereferences non-nil
// pointers to them, and rejects everything else.
func normalize(r Record) (Record, error) {
	switch v := r.(type) {
	case Put, Delete, Batch:
		return v, nil
	case *Put:
		if v != nil {
			return *v, nil
		}
	case *Delete:
		if v != nil {
			return *v, nil
		}
	case *Batch:
		if v != nil {
			return *v, nil
		}
	}
	return nil, fmt.Errorf("unsupported record type %T", r)
}

// payloadLen returns the payload size of the normalized record r, whose frame
// sits in a stream at the given depth. Field caps are checked before summing,
// so the result cannot overflow int, even on 32-bit platforms.
func payloadLen(r Record, depth int) (int, error) {
	switch r := r.(type) {
	case Put:
		if len(r.Key) > maxFrameLen || len(r.Value) > maxFrameLen {
			return 0, fmt.Errorf("field exceeds %d-byte limit", maxFrameLen)
		}
		return u32Size + len(r.Key) + u32Size + len(r.Value), nil
	case Delete:
		if len(r.Key) > maxFrameLen {
			return 0, fmt.Errorf("field exceeds %d-byte limit", maxFrameLen)
		}
		return u32Size + len(r.Key), nil
	case Batch:
		// Checked before recursing, so cyclic input cannot overflow the stack.
		if depth+1 > maxDepth {
			return 0, errDepth
		}
		n := u32Size
		for j, inner := range r.Records {
			fl, err := frameLen(inner, depth+1)
			if err != nil {
				return 0, fmt.Errorf("batch record %d: %w", j, err)
			}
			n += lenSize + fl + crcSize
			// Each addend is at most maxFrameLen+8, so n stays far below
			// 2^31 when we stop here.
			if n > maxFrameLen {
				return 0, errFrameTooLarge
			}
		}
		return n, nil
	}
	return 0, fmt.Errorf("unsupported record type %T", r)
}

// frameLen validates r (in a stream at depth) and returns its frame len
// (type+payload).
func frameLen(r Record, depth int) (int, error) {
	nr, err := normalize(r)
	if err != nil {
		return 0, err
	}
	n, err := payloadLen(nr, depth)
	if err != nil {
		return 0, err
	}
	flen := 1 + n
	if flen > maxFrameLen {
		return 0, errFrameTooLarge
	}
	return flen, nil
}

var (
	errDepth         = fmt.Errorf("batch nesting exceeds depth %d", maxDepth)
	errFrameTooLarge = fmt.Errorf("frame exceeds %d-byte limit", maxFrameLen)
)

// appendFrame appends r's full frame (r sits in a stream at depth). The len
// field is reserved and back-patched once type+payload are written, so no
// subtree is ever re-sized. The write pass re-checks the depth and frame caps
// itself instead of trusting the sizing pass, so input that changes between
// passes (e.g. a *Batch mutated concurrently) yields an error, never a panic or
// an invalid frame.
func appendFrame(dst []byte, r Record, depth int) ([]byte, error) {
	nr, err := normalize(r)
	if err != nil {
		return nil, err
	}
	lenAt := len(dst)
	dst = append(dst, 0, 0, 0, 0) // len, back-patched below
	start := len(dst)
	dst = append(dst, nr.Kind())
	if dst, err = appendPayload(dst, nr, depth); err != nil {
		return nil, err
	}
	flen := len(dst) - start
	if flen > maxFrameLen {
		return nil, errFrameTooLarge
	}
	binary.BigEndian.PutUint32(dst[lenAt:start], uint32(flen))
	return appendU32(dst, crc32.ChecksumIEEE(dst[start:])), nil
}

// appendPayload appends the normalized record r's payload.
func appendPayload(dst []byte, r Record, depth int) ([]byte, error) {
	switch r := r.(type) {
	case Put:
		if len(r.Key) > maxFrameLen || len(r.Value) > maxFrameLen {
			return nil, errFrameTooLarge
		}
		dst = appendBytes(dst, r.Key)
		return appendBytes(dst, r.Value), nil
	case Delete:
		if len(r.Key) > maxFrameLen {
			return nil, errFrameTooLarge
		}
		return appendBytes(dst, r.Key), nil
	case Batch:
		if depth+1 > maxDepth {
			return nil, errDepth
		}
		start := len(dst)
		dst = appendU32(dst, uint32(len(r.Records)))
		for j, inner := range r.Records {
			var err error
			if dst, err = appendFrame(dst, inner, depth+1); err != nil {
				return nil, fmt.Errorf("batch record %d: %w", j, err)
			}
			if len(dst)-start > maxFrameLen {
				return nil, errFrameTooLarge
			}
		}
		return dst, nil
	}
	return nil, fmt.Errorf("unsupported record type %T", r)
}

// appendBytes appends s as [uint32 len][bytes].
func appendBytes[T string | []byte](dst []byte, s T) []byte {
	dst = appendU32(dst, uint32(len(s)))
	return append(dst, s...)
}

func appendU32(dst []byte, x uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, x)
}

// --- decoding ---

// byteReader is a bounds-checked cursor over a byte slice. It never panics.
type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) remaining() int { return len(r.b) - r.off }

// take returns the next n bytes as a sub-slice of the input, or false if
// fewer than n bytes remain.
func (r *byteReader) take(n uint32) ([]byte, bool) {
	if uint64(n) > uint64(r.remaining()) {
		return nil, false
	}
	s := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return s, true
}

func (r *byteReader) u32() (uint32, bool) {
	s, ok := r.take(4)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint32(s), true
}

// readFrame reads one crc-checked frame from r and returns its type and
// payload as sub-slices of r's input. Because r wraps only the enclosing
// (sub-)stream, a frame can never extend into a sibling or outer frame.
func readFrame(r *byteReader) (uint8, []byte, bool) {
	n, ok := r.u32()
	if !ok {
		return 0, nil, false // truncated header
	}
	if n == 0 || n > maxFrameLen {
		return 0, nil, false // no type byte, or oversize: rejected before reading
	}
	body, ok := r.take(n)
	if !ok {
		return 0, nil, false
	}
	sum, ok := r.u32()
	if !ok || crc32.ChecksumIEEE(body) != sum {
		return 0, nil, false
	}
	return body[0], body[1:], true
}

// kindAllowed is the single place that states which record kinds each stream
// version may contain. A Batch's sub-stream inherits its stream's version.
func kindAllowed(ver, kind uint8) bool {
	switch ver {
	case version1:
		return kind == kindPut || kind == kindDelete
	case version2:
		return kind == kindPut || kind == kindDelete || kind == kindBatch
	}
	return false
}

// decodeRecord is the only decode dispatch. depth is the depth of the stream
// containing the frame; ver is the stream's version.
func decodeRecord(ver, kind uint8, payload []byte, depth int) (Record, bool) {
	if !kindAllowed(ver, kind) {
		return nil, false
	}
	switch kind {
	case kindPut:
		return decodePut(payload)
	case kindDelete:
		return decodeDelete(payload)
	case kindBatch:
		return decodeBatch(ver, payload, depth+1)
	}
	return nil, false
}

// decodeStream decodes the top-level (depth 0) frames that follow the version
// byte ver.
func decodeStream(ver uint8, p []byte) ([]Record, error) {
	r := byteReader{b: p}
	var recs []Record
	for r.remaining() > 0 {
		kind, pl, ok := readFrame(&r)
		if !ok {
			return nil, ErrCorrupt
		}
		rec, ok := decodeRecord(ver, kind, pl, 0)
		if !ok {
			return nil, ErrCorrupt
		}
		recs = append(recs, rec)
	}
	if recs == nil {
		recs = []Record{}
	}
	return recs, nil
}

// decodeBatch decodes a Batch payload whose inner sub-stream sits at depth.
func decodeBatch(ver uint8, p []byte, depth int) (Record, bool) {
	if depth > maxDepth {
		return nil, false
	}
	r := byteReader{b: p}
	n, ok := r.u32()
	if !ok {
		return nil, false
	}
	// No preallocation: nothing is allocated until a frame has been fully
	// validated, so an oversize inner len is rejected with zero allocations
	// and hostile counts cannot reserve memory at any depth.
	var recs []Record
	for i := uint32(0); i < n; i++ {
		kind, pl, ok := readFrame(&r)
		if !ok {
			return nil, false // fewer than n frames, or a bad frame
		}
		rec, ok := decodeRecord(ver, kind, pl, depth)
		if !ok {
			return nil, false
		}
		recs = append(recs, rec)
	}
	if r.remaining() != 0 {
		return nil, false // extra frames or trailing bytes
	}
	if recs == nil {
		recs = []Record{} // n == 0: non-nil, like top-level Decode
	}
	return Batch{Records: recs}, true
}

// readField reads a [uint32 len][bytes] field.
func readField(r *byteReader) ([]byte, bool) {
	n, ok := r.u32()
	if !ok {
		return nil, false
	}
	return r.take(n)
}

// decodePut decodes a Put payload, which must be consumed exactly.
func decodePut(p []byte) (Record, bool) {
	r := byteReader{b: p}
	k, ok := readField(&r)
	if !ok {
		return nil, false
	}
	v, ok := readField(&r)
	if !ok || r.remaining() != 0 {
		return nil, false
	}
	return Put{Key: string(k), Value: append(make([]byte, 0, len(v)), v...)}, true
}

// decodeDelete decodes a Delete payload, which must be consumed exactly.
func decodeDelete(p []byte) (Record, bool) {
	r := byteReader{b: p}
	k, ok := readField(&r)
	if !ok || r.remaining() != 0 {
		return nil, false
	}
	return Delete{Key: string(k)}, true
}
