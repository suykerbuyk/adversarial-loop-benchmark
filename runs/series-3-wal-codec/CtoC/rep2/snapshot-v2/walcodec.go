// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len counts type+payload only (the crc is outside len) and is capped at
// 16 MiB (inclusive) for every frame at every nesting depth. crc is
// CRC32-IEEE over type+payload. Encode writes version 2; Decode accepts
// version 1 (Put and Delete only) and version 2 (Put, Delete and Batch).
// Any other version byte is rejected before any frame is read.
//
// Payloads:
//
//	Put    = [klen uint32][key][vlen uint32][value]   (type 1)
//	Delete = [klen uint32][key]                       (type 2)
//	Batch  = [n uint32] frame_1 ... frame_n           (type 3, version 2 only)
//
// A Batch payload is a nested framed sub-stream: exactly n inner frames of the
// same shape, which must consume the payload exactly. An inner frame can never
// extend past its enclosing Batch payload. The top-level stream is depth 0 and
// a Batch's inner sub-stream is one level deeper than the Batch itself; a
// sub-stream deeper than 8 is rejected (so at most 8 levels of Batch nesting).
//
// Decode reports every malformed input as ErrCorrupt.
package walcodec

import (
	"errors"
	"fmt"
	"math"
)

const (
	version1    uint8 = 1        // legacy, decode-only (Put/Delete)
	version2    uint8 = 2        // the version Encode writes
	kindPut     uint8 = 1        // type tag of Put
	kindDelete  uint8 = 2        // type tag of Delete
	kindBatch   uint8 = 3        // type tag of Batch
	maxFrameLen       = 16 << 20 // largest legal frame len (type+payload), inclusive
	maxDepth          = 8        // deepest legal sub-stream depth (top level = 0)
	lenSize           = 4        // frame len field
	crcSize           = 4        // frame crc field
	u32Size           = 4        // payload length/count prefix
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

// Batch groups records; its payload is a nested framed sub-stream (type 3).
// Records may contain Put, Delete or nested Batch records. Decode always
// yields a non-nil Records slice, so Batch{} (nil Records) decodes as
// Batch{Records: []Record{}}, mirroring how a nil Put.Value decodes as an
// empty, non-nil []byte.
type Batch struct{ Records []Record }

// Kind returns 1, the wire type tag of Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire type tag of Delete.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns 3, the wire type tag of Batch.
func (Batch) Kind() uint8 { return kindBatch }

// Encode writes recs as a version-2 stream, frames in input order.
//
// Each element must be a Put, Delete or Batch, as a value or a non-nil
// pointer. Batches may nest up to 8 levels. Nil elements, nil pointers, other
// Record implementations, nesting deeper than 8 levels and any frame (at any
// depth) exceeding the 16 MiB len cap are rejected with an error. Decode always
// yields values, so pointers decode as the values they point to. On error
// Encode returns a nil slice; its errors are never ErrCorrupt.
func Encode(recs []Record) ([]byte, error) {
	total := 0
	for i, r := range recs {
		flen, err := frameLen(r, 0)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		fsize := lenSize + flen + crcSize
		if total > math.MaxInt-1-fsize {
			return nil, errors.New("walcodec: encoded stream too large")
		}
		total += fsize
	}

	out := make([]byte, 0, 1+total)
	out = append(out, version2)
	for i, r := range recs {
		var err error
		if out, err = appendFrame(out, r, 0); err != nil {
			// Only reachable if the input changed after the sizing pass.
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
	}
	return out, nil
}

// Decode parses a version-1 or version-2 stream. Records are returned as Put,
// Delete and Batch values in stream order; decoded keys and values never alias
// b. A version-only stream yields an empty, non-nil slice. Every failure
// returns ErrCorrupt and a nil slice; no partial result is returned.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, ErrCorrupt // missing version byte
	}
	switch b[0] {
	case version1:
		return decodeStream(version1, b[1:]) // Put/Delete only
	case version2:
		return decodeStream(version2, b[1:]) // Put/Delete/Batch
	default:
		return nil, ErrCorrupt // unknown version, rejected before any frame
	}
}
