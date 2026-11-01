package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Encode writes recs as a version-2 stream, one frame per record in order.
// It accepts Put, Delete, Batch and their pointer forms, at any depth. It
// returns an error (not ErrCorrupt) for a nil element, a nil pointer, an
// unsupported Record implementation, a frame (at any depth) that would
// exceed the 16 MiB cap, or Batch nesting deeper than 8. Encode never
// produces a stream that Decode would reject.
func Encode(recs []Record) ([]byte, error) {
	total := uint64(1)
	for i, r := range recs {
		n, err := frameBodyLen(r, 0)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		total += lenSize + n + crcSize
	}

	out := make([]byte, 0, total)
	out = append(out, version2)
	for _, r := range recs {
		out = appendRecord(out, r)
	}
	return out, nil
}

// normalize turns pointer forms into value forms and rejects nil elements,
// nil pointers and unsupported types.
func normalize(r Record) (Record, error) {
	switch v := r.(type) {
	case Put, Delete, Batch:
		return v, nil
	case *Put:
		if v == nil {
			return nil, errors.New("nil *Put")
		}
		return *v, nil
	case *Delete:
		if v == nil {
			return nil, errors.New("nil *Delete")
		}
		return *v, nil
	case *Batch:
		if v == nil {
			return nil, errors.New("nil *Batch")
		}
		return *v, nil
	default:
		return nil, fmt.Errorf("unsupported record type %T", r)
	}
}

// frameBodyLen validates r, found in a stream at the given depth, and returns
// its len field (type+payload). It enforces the frame cap on this frame and,
// recursively, on every inner frame, as well as the depth cap.
func frameBodyLen(r Record, depth int) (uint64, error) {
	r, err := normalize(r)
	if err != nil {
		return 0, err
	}
	var n uint64
	switch v := r.(type) {
	case Put:
		n = 1 + lenSize + uint64(len(v.Key)) + lenSize + uint64(len(v.Value))
	case Delete:
		n = 1 + lenSize + uint64(len(v.Key))
	case Batch:
		if depth+1 > maxDepth {
			return 0, fmt.Errorf("batch nesting exceeds depth %d", maxDepth)
		}
		n = 1 + countSize
		for j, c := range v.Records {
			cn, err := frameBodyLen(c, depth+1)
			if err != nil {
				return 0, fmt.Errorf("batch record %d: %w", j, err)
			}
			n += lenSize + cn + crcSize
			if n > uint64(maxFrameLen) {
				// Early exit; also guarantees len(Records) fits in uint32.
				break
			}
		}
	}
	if n > uint64(maxFrameLen) {
		return 0, fmt.Errorf("encoded frame length %d exceeds %d", n, maxFrameLen)
	}
	return n, nil
}

// appendRecord appends r as [len][type][payload][crc] to out. The caller
// has already validated r with frameBodyLen.
func appendRecord(out []byte, r Record) []byte {
	r, _ = normalize(r)
	start := len(out)
	out = append(out, 0, 0, 0, 0) // len placeholder
	out = append(out, r.Kind())
	switch v := r.(type) {
	case Put:
		out = binary.BigEndian.AppendUint32(out, uint32(len(v.Key)))
		out = append(out, v.Key...)
		out = binary.BigEndian.AppendUint32(out, uint32(len(v.Value)))
		out = append(out, v.Value...)
	case Delete:
		out = binary.BigEndian.AppendUint32(out, uint32(len(v.Key)))
		out = append(out, v.Key...)
	case Batch:
		out = binary.BigEndian.AppendUint32(out, uint32(len(v.Records)))
		for _, c := range v.Records {
			out = appendRecord(out, c)
		}
	}
	body := out[start+lenSize:]
	binary.BigEndian.PutUint32(out[start:], uint32(len(body)))
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}
