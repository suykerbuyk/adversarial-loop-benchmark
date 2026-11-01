package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Encode writes recs, in order, as a version-1 stream.
//
// Encode never panics on bad input. It returns (nil, err) if any record is a
// nil interface, a nil *Put or *Delete, a type other than Put, Delete, *Put or
// *Delete, or would produce a frame whose len exceeds 16 MiB. These errors
// describe caller input, not stream corruption, so they do not wrap ErrCorrupt.
//
// Each record is read exactly once. The caller must not modify recs, or
// anything they point to, while Encode runs.
func Encode(recs []Record) ([]byte, error) {
	out := make([]byte, 1, 1+min(len(recs), 1<<16)*32)
	out[0] = version1
	for i, r := range recs {
		kind, key, val, err := fields(r)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		p := payloadLen(kind, uint64(len(key)), uint64(len(val)))
		if n := frameLenField(p); n > uint64(maxFrameLen) {
			return nil, fmt.Errorf("walcodec: record %d: frame length %d exceeds cap %d", i, n, maxFrameLen)
		}
		out = appendFrame(out, kind, key, val, p)
	}
	return out, nil
}

// fields extracts the wire kind, key and value of r. It accepts Put, Delete
// and non-nil pointers to them; everything else is an error.
func fields(r Record) (kind uint8, key string, val []byte, err error) {
	switch x := r.(type) {
	case Put:
		return kindPut, x.Key, x.Value, nil
	case *Put:
		if x == nil {
			return 0, "", nil, errors.New("nil *Put")
		}
		return kindPut, x.Key, x.Value, nil
	case Delete:
		return kindDelete, x.Key, nil, nil
	case *Delete:
		if x == nil {
			return 0, "", nil, errors.New("nil *Delete")
		}
		return kindDelete, x.Key, nil, nil
	case nil:
		return 0, "", nil, errors.New("nil record")
	default:
		return 0, "", nil, fmt.Errorf("unsupported record type %T", r)
	}
}

// appendFrame appends one complete frame to out. p is the payload length,
// already checked against the cap by the caller.
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
