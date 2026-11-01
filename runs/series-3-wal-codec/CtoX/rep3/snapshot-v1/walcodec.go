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
// at 16 MiB.
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

// Put sets Key to Value.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes Key.
type Delete struct{ Key string }

// Kind returns KindPut.
func (Put) Kind() uint8 { return KindPut }

// Kind returns KindDelete.
func (Delete) Kind() uint8 { return KindDelete }

// ErrCorrupt is wrapped by every error Decode returns.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1     uint8  = 1        // the only version v1 writes and accepts
	maxFrameLen  uint32 = 16 << 20 // cap on the frame len field (16 MiB)
	lenFieldSize        = 4
	crcFieldSize        = 4
	u32Size             = 4 // payload length prefixes
)

// Preallocated decode errors; all wrap ErrCorrupt. Returning them never
// allocates, so no Decode error path allocates.
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
)
