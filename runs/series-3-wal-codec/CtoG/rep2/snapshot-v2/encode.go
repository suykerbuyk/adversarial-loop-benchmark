package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Encode writes recs, in order, as a version-2 stream.
//
// Accepted records are Put, Delete and Batch, and non-nil pointers to them.
// Encode never panics on bad input. It returns (nil, err) if any record at
// any depth is a nil interface, a nil *Put, *Delete or *Batch, an unsupported
// type, a Batch nested deeper than 8 levels, or would produce a frame (at any
// depth) whose len exceeds 16 MiB. These errors describe caller input, not
// stream corruption, so they do not wrap ErrCorrupt. Encode never emits a
// stream that Decode would reject.
//
// Each record is read exactly once. The caller must not modify recs, or
// anything they point to, while Encode runs.
func Encode(recs []Record) ([]byte, error) {
	out := make([]byte, 1, 1+min(len(recs), 1<<16)*32)
	out[0] = version2
	for i, r := range recs {
		grown, err := appendRecord(out, r, 0) // 0 = depth of the top-level stream
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		out = grown
	}
	return out, nil
}

// appendRecord appends r's full frame to out. depth is the depth of the
// stream r sits in (top level = 0). On error, callers must discard the
// returned buffer and use only err.
func appendRecord(out []byte, r Record, depth int) ([]byte, error) {
	switch x := r.(type) {
	case Put:
		return appendLeaf(out, kindPut, x.Key, x.Value)
	case *Put:
		if x == nil {
			return nil, errors.New("nil *Put")
		}
		return appendLeaf(out, kindPut, x.Key, x.Value)
	case Delete:
		return appendLeaf(out, kindDelete, x.Key, nil)
	case *Delete:
		if x == nil {
			return nil, errors.New("nil *Delete")
		}
		return appendLeaf(out, kindDelete, x.Key, nil)
	case Batch:
		return appendBatch(out, x.Records, depth)
	case *Batch:
		if x == nil {
			return nil, errors.New("nil *Batch")
		}
		return appendBatch(out, x.Records, depth)
	case nil:
		return nil, errors.New("nil record")
	default:
		return nil, fmt.Errorf("unsupported record type %T", r)
	}
}

// appendLeaf appends a Put or Delete frame, checking the cap before appending.
func appendLeaf(out []byte, kind uint8, key string, val []byte) ([]byte, error) {
	p := payloadLen(kind, uint64(len(key)), uint64(len(val)))
	if n := frameLenField(p); n > uint64(maxFrameLen) {
		return nil, fmt.Errorf("frame length %d exceeds cap %d", n, maxFrameLen)
	}
	return appendFrame(out, kind, key, val, p), nil
}

// appendBatch appends a Batch frame holding recs. depth is the depth of the
// stream the Batch sits in; its sub-stream is at depth+1.
func appendBatch(out []byte, recs []Record, depth int) ([]byte, error) {
	sub := depth + 1
	if sub > maxDepth {
		return nil, fmt.Errorf("batch nesting exceeds depth %d", maxDepth)
	}
	// Necessary condition only: every child occupies at least minFrameWire
	// bytes of this Batch's payload. This also bounds len(recs) well below
	// 2^32, so the uint32 count below never truncates.
	if uint64(len(recs)) > (uint64(maxFrameLen)-typeSize-u32Size)/minFrameWire {
		return nil, fmt.Errorf("batch of %d records cannot fit in a 16 MiB frame", len(recs))
	}
	lenAt := len(out)
	out = append(out, 0, 0, 0, 0) // len placeholder, filled in below
	body := len(out)
	out = append(out, kindBatch)
	out = appendU32(out, uint32(len(recs)))
	for j, r := range recs {
		grown, err := appendRecord(out, r, sub)
		// err is checked before grown is used: a failed child's buffer is
		// never inspected, so the cap check below always sees a valid out.
		if err != nil {
			return nil, fmt.Errorf("batch record %d: %w", j, err)
		}
		out = grown
		// out[body:] is type + n + complete child frames (children's crcs
		// included): exactly this Batch's len field.
		if l := uint64(len(out) - body); l > uint64(maxFrameLen) {
			return nil, fmt.Errorf("batch frame length %d exceeds cap %d", l, maxFrameLen)
		}
	}
	binary.BigEndian.PutUint32(out[lenAt:], uint32(len(out)-body))
	return appendU32(out, crc32.ChecksumIEEE(out[body:])), nil
}

// appendFrame appends one complete Put/Delete frame to out. p is the payload
// length, already checked against the cap by the caller.
func appendFrame(out []byte, kind uint8, key string, val []byte, p uint64) []byte {
	out = appendU32(out, uint32(frameLenField(p)))
	start := len(out)
	out = append(out, kind)
	out = appendBytes(out, key)
	if kind == kindPut {
		out = appendBytes(out, val)
	}
	return appendU32(out, crc32.ChecksumIEEE(out[start:]))
}

func appendU32(out []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(out, v)
}

// appendBytes appends a [u32 len][bytes] field. Lengths are bounded by the
// frame cap, so the uint32 conversion never truncates.
func appendBytes[T string | []byte](out []byte, b T) []byte {
	out = appendU32(out, uint32(len(b)))
	return append(out, b...)
}
