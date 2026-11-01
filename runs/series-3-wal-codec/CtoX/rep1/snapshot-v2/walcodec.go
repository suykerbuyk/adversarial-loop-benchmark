// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(u32) type(1) payload(len-1) crc(u32)
//
// len counts type+payload only; crc is CRC32-IEEE over type+payload and sits
// outside len. len is capped at 16 MiB at every nesting depth.
//
//	Put    payload = klen(u32) key vlen(u32) value
//	Delete payload = klen(u32) key
//	Batch  payload = n(u32) frame_1 ... frame_n
//
// A Batch payload is itself a framed sub-stream: exactly n inner frames of
// the same shape, which must fill the payload exactly. Batches may contain
// Put, Delete, or further Batch records. The top-level stream is depth 0 and a
// Batch's sub-stream is one level deeper than the Batch; a sub-stream deeper
// than 8 is rejected, so at most 8 levels of Batch nesting are legal.
//
// Encode writes version 2. Decode accepts version 2 (all record types) and the
// legacy version 1 (Put and Delete only); any other version is rejected.
package walcodec

import "errors"

const (
	version1    uint8  = 1        // legacy version, decode-only (Put/Delete)
	version2    uint8  = 2        // the version Encode writes (Put/Delete/Batch)
	kindPut     uint8  = 1        // frame type tag for Put
	kindDelete  uint8  = 2        // frame type tag for Delete
	kindBatch   uint8  = 3        // frame type tag for Batch
	maxFrameLen uint32 = 16 << 20 // cap on a frame's len field (type+payload)
	maxDepth           = 8        // deepest legal sub-stream depth (top level = 0)

	lenFieldSize   = 4 // size of a uint32 length field (frame len, klen, vlen)
	crcFieldSize   = 4 // size of the trailing crc field
	typeSize       = 1 // size of the frame type tag
	batchCountSize = 4 // size of a Batch's inner record count

	// minFrameSize is the smallest well-formed frame (len >= 1).
	minFrameSize = lenFieldSize + typeSize + crcFieldSize
)

// ErrCorrupt is returned by Decode for any malformed, truncated, oversize, or
// checksum-failing stream; for an unknown version or record type; and for a
// Batch nested more than 8 levels deep, or whose inner frames do not exactly
// fill its payload.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Record is a single log entry. Kind reports the record's wire type tag.
type Record interface {
	Kind() uint8
}

// Put records that Key was set to Value. A nil and an empty Value encode
// identically and decode to a non-nil, zero-length slice.
type Put struct {
	Key   string
	Value []byte
}

// Delete records that Key was removed.
type Delete struct {
	Key string
}

// Batch groups records into one frame. Its payload is a nested framed
// sub-stream: count(u32) followed by count inner frames. Batches may nest up
// to 8 levels deep. A decoded Batch always has a non-nil Records slice.
type Batch struct {
	Records []Record
}

// Kind returns 1, the wire tag for Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire tag for Delete.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns 3, the wire tag for Batch.
func (Batch) Kind() uint8 { return kindBatch }

var (
	_ Record = Put{}
	_ Record = Delete{}
	_ Record = Batch{}
)
