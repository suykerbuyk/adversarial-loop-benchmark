package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// frameFields is the normalised wire shape of a record.
type frameFields struct {
	typ      uint8
	key      string
	value    []byte
	hasValue bool // Put writes vlen+value (even when empty); Delete does not
}

// bodyLen returns the frame's len field (type+payload) as a uint64.
func (f frameFields) bodyLen() uint64 {
	n := 1 + lenSize + uint64(len(f.key))
	if f.hasValue {
		n += lenSize + uint64(len(f.value))
	}
	return n
}

// Encode writes recs as a version-1 stream, one frame per record in order.
// It accepts Put, Delete, *Put and *Delete. It returns an error (not
// ErrCorrupt) for a nil element, a nil pointer, an unsupported Record
// implementation, or a record whose frame would exceed the 16 MiB cap.
func Encode(recs []Record) ([]byte, error) {
	fields := make([]frameFields, len(recs))
	total := uint64(1)
	for i, r := range recs {
		f, err := fieldsOf(r)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		n := f.bodyLen()
		if n > uint64(maxFrameLen) {
			return nil, fmt.Errorf("walcodec: record %d: encoded frame length %d exceeds %d", i, n, maxFrameLen)
		}
		fields[i] = f
		total += lenSize + n + crcSize
	}

	out := make([]byte, 0, total)
	out = append(out, version1)
	for _, f := range fields {
		out = appendFrame(out, f)
	}
	return out, nil
}

// fieldsOf normalises the supported record forms into frameFields.
func fieldsOf(r Record) (frameFields, error) {
	switch v := r.(type) {
	case Put:
		return frameFields{typ: kindPut, key: v.Key, value: v.Value, hasValue: true}, nil
	case *Put:
		if v == nil {
			return frameFields{}, errors.New("nil *Put")
		}
		return fieldsOf(*v)
	case Delete:
		return frameFields{typ: kindDelete, key: v.Key}, nil
	case *Delete:
		if v == nil {
			return frameFields{}, errors.New("nil *Delete")
		}
		return fieldsOf(*v)
	default:
		return frameFields{}, fmt.Errorf("unsupported record type %T", r)
	}
}

// appendFrame appends f as [len][type][payload][crc] to out. The caller has
// already checked that f fits within maxFrameLen.
func appendFrame(out []byte, f frameFields) []byte {
	start := len(out)
	out = append(out, 0, 0, 0, 0) // len placeholder
	out = append(out, f.typ)
	out = binary.BigEndian.AppendUint32(out, uint32(len(f.key)))
	out = append(out, f.key...)
	if f.hasValue {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f.value)))
		out = append(out, f.value...)
	}
	body := out[start+lenSize:]
	binary.BigEndian.PutUint32(out[start:], uint32(len(body)))
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}
