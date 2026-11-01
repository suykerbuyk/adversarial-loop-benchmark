// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len counts type+payload only; crc is CRC32-IEEE over type+payload and sits
// outside len. len is capped at 16 MiB at every nesting depth. Payloads are:
//
//	Put    = [klen uint32][key][vlen uint32][value]
//	Delete = [klen uint32][key]
//	Batch  = [n uint32] frame{n}
//
// A Batch payload is a nested framed sub-stream whose n frames must consume
// the payload exactly. The top-level stream is depth 0 and a Batch's inner
// sub-stream sits one level deeper than the Batch; a sub-stream deeper than
// 8 is corrupt, and Encode refuses to produce one.
//
// Encode writes version 2. Decode accepts version 1 (Put and Delete only)
// and version 2 (Put, Delete and Batch); any other version is corrupt.
//
// Encode accepts Put, Delete, Batch and their pointer forms; Decode always
// returns value records (Put, Delete, Batch), so pointer identity is not
// preserved.
package walcodec

import "errors"

// Record is a single log entry. Kind reports the record's wire type tag.
type Record interface{ Kind() uint8 }

// Put sets Key to Value. A nil and an empty Value are equivalent.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes Key.
type Delete struct {
	Key string
}

// Batch groups records into one frame whose payload is a nested framed
// sub-stream: [n uint32] followed by n inner frames. Batches may nest up to
// 8 levels deep.
type Batch struct{ Records []Record }

// Kind returns the Put type tag, 1.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag, 2.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch type tag, 3.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is returned (possibly wrapped) by Decode for any malformed stream.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1     uint8  = 1        // accepted on decode (Put/Delete only)
	version2     uint8  = 2        // written by Encode; accepted on decode
	kindPut      uint8  = 1        // Put type tag
	kindDelete   uint8  = 2        // Delete type tag
	kindBatch    uint8  = 3        // Batch type tag
	maxFrameLen  uint32 = 16 << 20 // cap on a frame's len field
	maxDepth            = 8        // deepest legal sub-stream depth
	lenSize             = 4        // size of the uint32 len prefix
	crcSize             = 4        // size of the uint32 crc trailer
	countSize           = 4        // size of a Batch's uint32 record count
	minFrameSize        = lenSize + 1 + crcSize
)
