package walcodec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// Encode writes a version-1 stream containing recs in order. Records must be
// Put or Delete values; non-nil *Put / *Delete are accepted as a convenience
// and encoded exactly like the values they point to.
//
// On failure Encode returns a nil slice and a non-nil error that is never
// ErrCorrupt; failures arise only from caller mistakes (an unsupported or nil
// record, a record whose frame would exceed the 16 MiB cap, or a total output
// larger than the platform can address). Callers should treat any non-nil
// error as fatal for that call; the error text is informational and not part
// of the API.
func Encode(recs []Record) ([]byte, error) {
	total := uint64(1) // version byte
	for i, r := range recs {
		body, err := bodyLen(i, r)
		if err != nil {
			return nil, err
		}
		var ok bool
		if total, ok = addFrameSize(total, body); !ok {
			return nil, fmt.Errorf("%w: at record %d", errOutputTooLarge, i)
		}
	}

	out := make([]byte, 0, int(total))
	out = append(out, version1)
	for _, r := range recs {
		c, _ := asConcrete(r) // validated by the sizing pass
		out = binary.BigEndian.AppendUint32(out, uint32(concreteBodyLen(c)))
		start := len(out)
		switch x := c.(type) {
		case Put:
			out = append(out, kindPut)
			out = appendBytes(out, x.Key)
			out = appendBytes(out, x.Value)
		case Delete:
			out = append(out, kindDelete)
			out = appendBytes(out, x.Key)
		}
		out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start:]))
	}
	return out, nil
}

// addFrameSize adds the on-wire size of a frame with the given body length to
// total. It reports false if the result would exceed math.MaxInt, the largest
// buffer the platform can address.
func addFrameSize(total, body uint64) (uint64, bool) {
	frame := lenSize + body + crcSize
	if total > uint64(math.MaxInt) || frame > uint64(math.MaxInt)-total {
		return 0, false
	}
	return total + frame, true
}

// asConcrete normalizes r to a Put or Delete value. It reports false for a nil
// record, a nil *Put / *Delete, or any other type.
func asConcrete(r Record) (Record, bool) {
	switch x := r.(type) {
	case Put, Delete:
		return x, true
	case *Put:
		if x != nil {
			return *x, true
		}
	case *Delete:
		if x != nil {
			return *x, true
		}
	}
	return nil, false
}

// concreteBodyLen returns the frame len (type+payload) of a Put or Delete.
func concreteBodyLen(c Record) uint64 {
	switch x := c.(type) {
	case Put:
		return 1 + u32Size + uint64(len(x.Key)) + u32Size + uint64(len(x.Value))
	case Delete:
		return 1 + u32Size + uint64(len(x.Key))
	}
	return 0
}

// bodyLen validates record i and returns its frame len. Because the cap is far
// below math.MaxUint32, a body within the cap also guarantees that every
// length field fits in a uint32.
func bodyLen(i int, r Record) (uint64, error) {
	c, ok := asConcrete(r)
	if !ok {
		return 0, fmt.Errorf("%w: record %d (%T)", errUnsupportedRecord, i, r)
	}
	n := concreteBodyLen(c)
	if n > maxFrameLen {
		return 0, fmt.Errorf("%w: record %d (%T) has frame len %d > %d",
			errRecordTooLarge, i, r, n, maxFrameLen)
	}
	return n, nil
}

// appendBytes appends a [uint32 len][bytes] field.
func appendBytes[T string | []byte](out []byte, s T) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(s)))
	return append(out, s...)
}
