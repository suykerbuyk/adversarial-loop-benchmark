// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// A stream is a single version byte followed by zero or more frames:
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// All integers are big-endian. len counts type+payload only; crc is a
// CRC32-IEEE checksum over type+payload and sits outside len. len is capped
// at 16 MiB at every nesting depth.
//
// Record types are Put (1), Delete (2), and Batch (3). A Batch payload is
// itself a framed sub-stream:
//
//	batch payload = n(4) frame_1 ... frame_n
//
// The n inner frames must consume the payload exactly. The top-level stream
// is depth 0 and a Batch's sub-stream sits one level deeper than the Batch
// itself; a sub-stream deeper than 8 is rejected.
//
// Encode writes version 2. Decode accepts version 1 (Put and Delete only)
// and version 2 (all record types); any other version is rejected.
package walcodec

import (
	"errors"
	"fmt"
)

// Record is a single log entry. Concrete types return their wire type tag.
type Record interface{ Kind() uint8 }

// Record type tags.
const (
	KindPut    uint8 = 1
	KindDelete uint8 = 2
)

// kindBatch is unexported: the only new v2 surface is the Batch type.
const kindBatch uint8 = 3

// Put sets Key to Value.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes Key.
type Delete struct{ Key string }

// Batch groups records into a single frame whose payload is a nested framed
// sub-stream. Records may include Put, Delete, and nested Batch values.
type Batch struct{ Records []Record }

// Kind returns KindPut.
func (Put) Kind() uint8 { return KindPut }

// Kind returns KindDelete.
func (Delete) Kind() uint8 { return KindDelete }

// Kind returns 3, the Batch wire type tag.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is wrapped by every error Decode returns.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1       uint8  = 1        // Put/Delete only; accepted by Decode
	version2       uint8  = 2        // all record types; written by Encode
	maxFrameLen    uint32 = 16 << 20 // cap on the frame len field (16 MiB)
	maxDepth              = 8        // deepest legal sub-stream depth
	lenFieldSize          = 4
	crcFieldSize          = 4
	u32Size               = 4 // payload length prefixes
	batchCountSize        = 4 // Batch record count
)

// Preallocated decode errors; all wrap ErrCorrupt. Returning one never
// allocates. Decode allocates only decoded records and the slices holding
// them, never a buffer sized by an unvalidated length or count; the total is
// linear in the input (a small constant multiple of it, not at most its
// size). See Decode's doc comment.
var (
	errNoVersion     = fmt.Errorf("%w: missing version byte", ErrCorrupt)
	errBadVersion    = fmt.Errorf("%w: unknown version", ErrCorrupt)
	errShortHeader   = fmt.Errorf("%w: truncated frame header", ErrCorrupt)
	errOversize      = fmt.Errorf("%w: frame length exceeds 16 MiB cap", ErrCorrupt)
	errEmptyFrame    = fmt.Errorf("%w: empty frame (missing type byte)", ErrCorrupt)
	errTruncated     = fmt.Errorf("%w: truncated frame", ErrCorrupt)
	errCRC           = fmt.Errorf("%w: crc mismatch", ErrCorrupt)
	errUnknownType   = fmt.Errorf("%w: unknown record type", ErrCorrupt)
	errPrefixOverrun = fmt.Errorf("%w: length prefix exceeds payload", ErrCorrupt)
	errTrailing      = fmt.Errorf("%w: trailing bytes in payload", ErrCorrupt)
	errTooDeep       = fmt.Errorf("%w: batch nesting exceeds depth 8", ErrCorrupt)
	errBatchShort    = fmt.Errorf("%w: batch payload ends before its record count is reached", ErrCorrupt)
	errBatchCount    = fmt.Errorf("%w: batch payload shorter than its 4-byte count", ErrCorrupt)
)
