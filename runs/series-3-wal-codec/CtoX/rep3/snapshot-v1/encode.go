package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// maxInt is the platform maximum slice length/capacity.
const maxInt = int(^uint(0) >> 1)

// Encode writes recs as a version-1 stream, frames in slice order.
//
// Put, Delete, and non-nil *Put / *Delete are accepted. Any other record
// (including nil) is rejected, as is a record whose frame would exceed the
// 16 MiB cap. Encode errors do not wrap ErrCorrupt. On error no bytes are
// returned.
func Encode(recs []Record) ([]byte, error) {
	total := uint64(1) // version byte
	for i, r := range recs {
		kind, key, val, err := normalize(r)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		frameLen := frameBodyLen(kind, key, val)
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
	out = append(out, version1)
	for _, r := range recs {
		kind, key, val, _ := normalize(r) // validated above
		out = appendFrame(out, kind, key, val)
	}
	return out, nil
}

// normalize extracts the wire fields of a supported record.
func normalize(r Record) (kind uint8, key string, val []byte, err error) {
	switch x := r.(type) {
	case Put:
		return KindPut, x.Key, x.Value, nil
	case Delete:
		return KindDelete, x.Key, nil, nil
	case *Put:
		if x != nil {
			return KindPut, x.Key, x.Value, nil
		}
	case *Delete:
		if x != nil {
			return KindDelete, x.Key, nil, nil
		}
	}
	return 0, "", nil, fmt.Errorf("unsupported record type %T", r)
}

// frameBodyLen returns the frame len field (type+payload) as uint64 so it
// cannot overflow on 32-bit platforms.
func frameBodyLen(kind uint8, key string, val []byte) uint64 {
	n := uint64(1) + u32Size + uint64(len(key))
	if kind == KindPut {
		n += u32Size + uint64(len(val))
	}
	return n
}

// addSize returns total+n, or false if the sum would exceed limit. Never wraps.
func addSize(total, n, limit uint64) (uint64, bool) {
	if n > limit || total > limit-n {
		return 0, false
	}
	return total + n, true
}

// appendFrame appends one [len][type][payload][crc] frame to out.
func appendFrame(out []byte, kind uint8, key string, val []byte) []byte {
	start := len(out)
	out = append(out, 0, 0, 0, 0) // len placeholder
	out = append(out, kind)
	out = binary.BigEndian.AppendUint32(out, uint32(len(key)))
	out = append(out, key...)
	if kind == KindPut {
		out = binary.BigEndian.AppendUint32(out, uint32(len(val)))
		out = append(out, val...)
	}
	body := out[start+lenFieldSize:]
	binary.BigEndian.PutUint32(out[start:], uint32(len(body)))
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}
