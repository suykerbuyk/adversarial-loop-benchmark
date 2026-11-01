package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// maxInt is the platform maximum slice length/capacity.
const maxInt = int(^uint(0) >> 1)

// Encode writes recs as a version-2 stream, frames in slice order. The
// stream may include Batches.
//
// Put, Delete, Batch, and non-nil *Put / *Delete / *Batch are accepted, at
// any depth. Any other record (including nil) is rejected, as is a record
// whose frame (at any depth) would exceed the 16 MiB cap, and a Batch whose
// sub-stream would sit deeper than 8. Encode never emits a stream Decode
// would reject. Encode errors do not wrap ErrCorrupt. On error no bytes are
// returned.
func Encode(recs []Record) ([]byte, error) {
	total := uint64(1) // version byte
	for i, r := range recs {
		frameLen, err := frameBodyLen(r, 0)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		if frameLen > uint64(maxFrameLen) {
			return nil, fmt.Errorf("walcodec: record %d: frame length %d exceeds 16 MiB cap", i, frameLen)
		}
		var ok bool
		total, ok = addSize(total, lenFieldSize+frameLen+crcFieldSize, uint64(maxInt))
		if !ok {
			return nil, fmt.Errorf("walcodec: record %d: encoded stream size exceeds maximum slice length", i)
		}
	}

	out := make([]byte, 0, int(total))
	out = append(out, version2)
	for _, r := range recs {
		out = appendRecord(out, r) // validated above
	}
	return out, nil
}

// normalize extracts the wire fields of a supported record. For a Batch,
// kind is kindBatch and children holds its records; key and val are unused.
func normalize(r Record) (kind uint8, key string, val []byte, children []Record, err error) {
	switch x := r.(type) {
	case Put:
		return KindPut, x.Key, x.Value, nil, nil
	case Delete:
		return KindDelete, x.Key, nil, nil, nil
	case Batch:
		return kindBatch, "", nil, x.Records, nil
	case *Put:
		if x != nil {
			return KindPut, x.Key, x.Value, nil, nil
		}
	case *Delete:
		if x != nil {
			return KindDelete, x.Key, nil, nil, nil
		}
	case *Batch:
		if x != nil {
			return kindBatch, "", nil, x.Records, nil
		}
	}
	return 0, "", nil, nil, fmt.Errorf("unsupported record type %T", r)
}

// frameBodyLen validates r as a record in a sub-stream at depth and returns
// its len field (type+payload) as uint64, so it cannot overflow on 32-bit
// platforms. For a Batch the running sum is capped at maxFrameLen, so the
// pass stops as soon as the batch is too big and never overflows; with the
// depth cap this also bounds work on shared or self-referential *Batch
// graphs. Errors do not wrap ErrCorrupt.
func frameBodyLen(r Record, depth int) (uint64, error) {
	kind, key, val, children, err := normalize(r)
	if err != nil {
		return 0, err
	}
	switch kind {
	case KindPut:
		return uint64(1) + u32Size + uint64(len(key)) + u32Size + uint64(len(val)), nil
	case KindDelete:
		return uint64(1) + u32Size + uint64(len(key)), nil
	}
	// Batch.
	if depth+1 > maxDepth {
		return 0, fmt.Errorf("batch nesting exceeds depth %d", maxDepth)
	}
	if uint64(len(children)) > math.MaxUint32 {
		return 0, fmt.Errorf("batch record count %d exceeds uint32", len(children))
	}
	n := uint64(1) + batchCountSize
	for i, c := range children {
		cl, err := frameBodyLen(c, depth+1)
		if err != nil {
			return 0, fmt.Errorf("batch record %d: %w", i, err)
		}
		if cl > uint64(maxFrameLen) {
			return 0, fmt.Errorf("batch record %d: frame length %d exceeds 16 MiB cap", i, cl)
		}
		var ok bool
		n, ok = addSize(n, lenFieldSize+cl+crcFieldSize, uint64(maxFrameLen))
		if !ok {
			return 0, fmt.Errorf("batch record %d: batch frame length exceeds 16 MiB cap", i)
		}
	}
	return n, nil
}

// addSize returns total+n, or false if the sum would exceed limit. Never wraps.
func addSize(total, n, limit uint64) (uint64, bool) {
	if n > limit || total > limit-n {
		return 0, false
	}
	return total + n, true
}

// appendRecord appends one [len][type][payload][crc] frame for r to out.
// r must already have been accepted by frameBodyLen. Child frames of a Batch
// are written inside the parent body before the parent's len and crc are
// back-filled.
func appendRecord(out []byte, r Record) []byte {
	kind, key, val, children, _ := normalize(r)
	start := len(out)
	out = append(out, 0, 0, 0, 0) // len placeholder
	out = append(out, kind)
	switch kind {
	case KindPut:
		out = binary.BigEndian.AppendUint32(out, uint32(len(key)))
		out = append(out, key...)
		out = binary.BigEndian.AppendUint32(out, uint32(len(val)))
		out = append(out, val...)
	case KindDelete:
		out = binary.BigEndian.AppendUint32(out, uint32(len(key)))
		out = append(out, key...)
	case kindBatch:
		out = binary.BigEndian.AppendUint32(out, uint32(len(children)))
		for _, c := range children {
			out = appendRecord(out, c)
		}
	}
	body := out[start+lenFieldSize:]
	binary.BigEndian.PutUint32(out[start:], uint32(len(body)))
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}
