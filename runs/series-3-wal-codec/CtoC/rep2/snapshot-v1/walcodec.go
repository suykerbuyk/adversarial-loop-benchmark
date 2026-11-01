// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len counts type+payload only (the crc is outside len) and is capped at
// 16 MiB. crc is CRC32-IEEE over type+payload. This version writes and accepts
// only version 1.
//
// Payloads:
//
//	Put    = [klen uint32][key][vlen uint32][value]   (type 1)
//	Delete = [klen uint32][key]                       (type 2)
//
// Decode reports every malformed input as ErrCorrupt.
package walcodec

import (
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	version1    uint8 = 1        // the only version v1 writes and accepts
	kindPut     uint8 = 1        // type tag of Put
	kindDelete  uint8 = 2        // type tag of Delete
	maxFrameLen       = 16 << 20 // largest legal frame len (type+payload), inclusive
	lenSize           = 4        // frame len field
	crcSize           = 4        // frame crc field
	u32Size           = 4        // payload string/bytes length prefix
)

// ErrCorrupt is returned by Decode for any malformed stream.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a single log entry. Kind reports the record's wire type tag.
type Record interface{ Kind() uint8 }

// Put records that Key was set to Value.
type Put struct {
	Key   string
	Value []byte
}

// Delete records that Key was removed.
type Delete struct{ Key string }

// Kind returns 1, the wire type tag of Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire type tag of Delete.
func (Delete) Kind() uint8 { return kindDelete }

// Encode writes recs as a version-1 stream, frames in input order.
//
// Each element must be a Put or Delete value. Pointers (*Put, *Delete), nil
// elements and other Record implementations are rejected with an error, as is
// any record whose frame would exceed the 16 MiB len cap. Decode always yields
// values, so only values round-trip exactly. On error Encode returns a nil
// slice; its errors are never ErrCorrupt.
func Encode(recs []Record) ([]byte, error) {
	total := 0
	for i, r := range recs {
		flen, err := recordFrameLen(i, r)
		if err != nil {
			return nil, err
		}
		fsize := lenSize + flen + crcSize
		if total > math.MaxInt-1-fsize {
			return nil, errors.New("walcodec: encoded stream too large")
		}
		total += fsize
	}

	out := make([]byte, 0, 1+total)
	out = append(out, version1)
	for _, r := range recs {
		n, _ := payloadLen(r) // validated above
		out = appendU32(out, uint32(1+n))
		start := len(out)
		out = append(out, r.Kind())
		out = appendPayload(out, r)
		out = appendU32(out, crc32.ChecksumIEEE(out[start:]))
	}
	return out, nil
}

// recordFrameLen validates recs[i] and returns its frame len (type+payload).
func recordFrameLen(i int, r Record) (int, error) {
	n, ok := payloadLen(r)
	if !ok {
		return 0, fmt.Errorf("walcodec: record %d: unsupported record type %T", i, r)
	}
	// Check field caps before trusting n: on 32-bit platforms the sum inside
	// payloadLen could otherwise have overflowed.
	if !fieldsWithinCap(r) {
		return 0, fmt.Errorf("walcodec: record %d: field exceeds %d-byte limit", i, maxFrameLen)
	}
	flen := 1 + n
	if flen > maxFrameLen {
		return 0, fmt.Errorf("walcodec: record %d: frame length %d exceeds %d-byte limit", i, flen, maxFrameLen)
	}
	return flen, nil
}

// Decode parses a stream produced by Encode. Records are returned as Put and
// Delete values in stream order; decoded keys and values never alias b.
// A version-only stream yields an empty, non-nil slice. Every failure returns
// ErrCorrupt and a nil slice; no partial result is returned.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, ErrCorrupt // missing version byte
	}
	switch b[0] {
	case version1:
		return decodeV1Frames(b[1:])
	default:
		return nil, ErrCorrupt // unknown version, rejected before any frame
	}
}
