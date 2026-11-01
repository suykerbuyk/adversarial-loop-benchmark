package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	version1    byte  = 1 // the only version v1 writes and accepts
	kindPut     uint8 = 1
	kindDelete  uint8 = 2
	maxFrameLen       = 16 << 20 // cap on a frame's len (type+payload)
	lenSize           = 4        // frame len field
	crcSize           = 4        // frame crc trailer
	u32Size           = 4        // klen / vlen prefixes
)

// ErrCorrupt is wrapped by every error Decode returns. Use errors.Is to test
// for it; the returned errors carry additional detail and are not == to it.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a single WAL entry. Kind reports the record's wire tag.
//
// Kind is informational only: having a Kind of 1 or 2 does not make a value
// encodable. Encode accepts only the concrete types Put, *Put, Delete and
// *Delete (non-nil), and dispatches on concrete type, never on Kind.
type Record interface{ Kind() uint8 }

// Put records that Key was set to Value.
type Put struct {
	Key   string
	Value []byte
}

// Delete records that Key was removed.
type Delete struct{ Key string }

// Kind returns 1, the wire tag for Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire tag for Delete.
func (Delete) Kind() uint8 { return kindDelete }

// Encode writes recs as a version-1 stream, frames in slice order. An empty
// recs yields the one-byte, version-only stream.
//
// Encode fails, returning a nil slice, if any element is not a Put, *Put,
// Delete or *Delete (including nil elements and nil pointers), or if a record
// would not fit in a 16 MiB frame. These errors do not wrap ErrCorrupt.
func Encode(recs []Record) ([]byte, error) {
	// Pass 1: validate every record and size the output; nothing is written.
	total := uint64(1) // version byte
	for i, r := range recs {
		_, key, val, hasValue, err := recordFields(i, r)
		if err != nil {
			return nil, err
		}
		size := frameBodySize(key, val, hasValue)
		if size > maxFrameLen {
			return nil, fmt.Errorf("walcodec: record %d: encoded size %d exceeds 16 MiB frame cap", i, size)
		}
		total += lenSize + size + crcSize
	}
	if total > uint64(math.MaxInt) {
		return nil, errors.New("walcodec: encoded stream too large")
	}

	// Pass 2: write. Every record was accepted above, so nothing can fail.
	out := make([]byte, 1, int(total))
	out[0] = version1
	for i, r := range recs {
		kind, key, val, hasValue, _ := recordFields(i, r)
		size := frameBodySize(key, val, hasValue) // ≤ maxFrameLen, fits uint32
		start := len(out)
		out = binary.BigEndian.AppendUint32(out, uint32(size))
		out = append(out, kind)
		out = appendString(out, key)
		if hasValue {
			out = appendBytes(out, val)
		}
		out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start+lenSize:]))
	}
	return out, nil
}

// recordFields normalises r to its wire parts by concrete type. hasValue is
// true for Put.
func recordFields(i int, r Record) (kind uint8, key string, val []byte, hasValue bool, err error) {
	switch x := r.(type) {
	case Put:
		return kindPut, x.Key, x.Value, true, nil
	case *Put:
		if x != nil {
			return kindPut, x.Key, x.Value, true, nil
		}
	case Delete:
		return kindDelete, x.Key, nil, false, nil
	case *Delete:
		if x != nil {
			return kindDelete, x.Key, nil, false, nil
		}
	}
	return 0, "", nil, false, fmt.Errorf("walcodec: record %d: unsupported record type %T", i, r)
}

// frameBodySize returns the size of type+payload, computed in uint64 so that
// no length is narrowed before the cap check.
func frameBodySize(key string, val []byte, hasValue bool) uint64 {
	n := uint64(1) + u32Size + uint64(len(key))
	if hasValue {
		n += u32Size + uint64(len(val))
	}
	return n
}

// appendString appends s as [uint32 len][bytes]. The caller guarantees
// len(s) fits in uint32.
func appendString(dst []byte, s string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

// appendBytes appends p as [uint32 len][bytes]. The caller guarantees
// len(p) fits in uint32.
func appendBytes(dst []byte, p []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(p)))
	return append(dst, p...)
}

// Decode parses a stream produced by Encode and returns its records in stream
// order. Decoded records never alias b.
//
// Every failure (missing or unknown version byte, truncated frame, oversize
// or empty len, crc mismatch, unknown type, malformed payload) returns a nil
// slice and an error wrapping ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, corrupt("missing version byte")
	}
	if b[0] != version1 {
		return nil, corrupt("unknown version %d", b[0])
	}
	// Non-nil even when empty: a local choice so an empty stream reads as
	// "empty list" rather than "absent".
	recs := []Record{}
	off := 1
	for off < len(b) {
		if len(b)-off < lenSize {
			return nil, corrupt("truncated frame header at offset %d", off)
		}
		n := binary.BigEndian.Uint32(b[off:])
		if n > maxFrameLen {
			return nil, corrupt("frame len %d exceeds 16 MiB cap at offset %d", n, off)
		}
		if n == 0 {
			return nil, corrupt("empty frame at offset %d", off)
		}
		// rem ≥ 0 by the header check. If this passes, off+lenSize+n+crcSize
		// ≤ len(b), so none of the indices below can overflow int.
		rem := len(b) - off - lenSize
		if uint64(rem) < uint64(n)+crcSize {
			return nil, corrupt("truncated frame at offset %d", off)
		}
		end := off + lenSize + int(n)
		body := b[off+lenSize : end]
		if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(b[end:]) {
			return nil, corrupt("crc mismatch at offset %d", off)
		}
		rec, err := decodePayload(body[0], body[1:])
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off = end + crcSize
	}
	return recs, nil
}

// decodePayload parses a crc-verified payload of the given type. Parsing is
// strict: trailing bytes are an error, so a successful Decode is canonical.
func decodePayload(kind uint8, p []byte) (Record, error) {
	switch kind {
	case kindPut:
		key, p, ok := readBytes(p)
		if !ok {
			return nil, corrupt("malformed put payload")
		}
		val, p, ok := readBytes(p)
		if !ok || len(p) != 0 {
			return nil, corrupt("malformed put payload")
		}
		v := make([]byte, len(val))
		copy(v, val)
		return Put{Key: string(key), Value: v}, nil
	case kindDelete:
		key, p, ok := readBytes(p)
		if !ok || len(p) != 0 {
			return nil, corrupt("malformed delete payload")
		}
		return Delete{Key: string(key)}, nil
	default:
		return nil, corrupt("unknown record type %d", kind)
	}
}

// readBytes reads a bounds-checked [uint32 len][bytes] field from p.
func readBytes(p []byte) (field, rest []byte, ok bool) {
	if len(p) < u32Size {
		return nil, nil, false
	}
	l := binary.BigEndian.Uint32(p)
	if uint64(l) > uint64(len(p)-u32Size) {
		return nil, nil, false
	}
	end := u32Size + int(l)
	return p[u32Size:end], p[end:], true
}

// corrupt returns an error wrapping ErrCorrupt with the given detail.
func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}
