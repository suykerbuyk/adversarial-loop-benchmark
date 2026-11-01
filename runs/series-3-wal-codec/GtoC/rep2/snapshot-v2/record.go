package walcodec

import "errors"

// Record is one log entry. Kind returns the wire type tag.
type Record interface {
	Kind() uint8
}

// Put is a key/value write. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete is a key removal. Kind is 2.
type Delete struct {
	Key string
}

// Batch is a nested group of records. Kind is 3.
// Its payload is itself a framed sub-stream: a big-endian uint32 count n,
// then exactly n frames of the same [len][type][payload][crc] shape.
// Top-level records sit at depth 0. A Batch's inner sub-stream is one level
// deeper. A sub-stream at depth greater than 8 is corrupt.
//
// A decoded Records slice is always non-nil, including for an empty Batch.
// Encode of Batch{Records: nil} writes n = 0 and decodes back as an empty
// non-nil slice, the same convention as a nil Put.Value decoding to length 0.
// reflect.DeepEqual therefore does not round-trip a nil Records field.
type Batch struct {
	Records []Record
}

func (Put) Kind() uint8    { return kindPut }
func (Delete) Kind() uint8 { return kindDelete }
func (Batch) Kind() uint8  { return kindBatch }

// ErrCorrupt is returned bare by Decode for every corrupt stream, and bare
// by Encode when nesting exceeds depth 8. It is not the only package error:
// an unsupported concrete type and a frame that would exceed 16 MiB use
// unexported errors that do not match ErrCorrupt.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnsupported = errors.New("walcodec: unsupported record")
	errTooLarge    = errors.New("walcodec: record exceeds max frame length")
)

const (
	version1 = 1
	version2 = 2

	kindPut    = 1
	kindDelete = 2
	kindBatch  = 3

	maxFrameLen = 16 << 20 // 16777216; legal len is <= this; larger is corrupt
	// maxBatchDepth is the deepest legal sub-stream. Top-level is depth 0.
	// Entering a Batch at depth d requires d+1 <= maxBatchDepth.
	maxBatchDepth = 8
	u32Size       = 4
	crcSize       = 4
	// minWireFrameBytes is the full on-wire frame (len + type + crc), not the
	// len field. The smallest legal len field is 1. Used only to bound Batch n
	// before any allocation sized from the untrusted count.
	minWireFrameBytes = u32Size + 1 + crcSize // 9
)

var (
	_ Record = Put{}
	_ Record = Delete{}
	_ Record = Batch{}
)

// childDepth is the depth of a Batch's inner sub-stream.
// Top-level records are encoded and decoded at depth 0.
// A sub-stream at depth greater than maxBatchDepth is illegal.
func childDepth(parent int) (int, bool) {
	d := parent + 1
	if d > maxBatchDepth {
		return 0, false
	}
	return d, true
}
