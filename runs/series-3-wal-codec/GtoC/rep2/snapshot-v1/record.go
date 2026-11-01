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

func (Put) Kind() uint8    { return kindPut }
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is the only decode error. The text is fixed.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnsupported = errors.New("walcodec: unsupported record")
	errTooLarge    = errors.New("walcodec: record exceeds max frame length")
)

const (
	version1    = 1
	kindPut     = 1
	kindDelete  = 2
	maxFrameLen = 16 << 20 // 16777216; legal len is <= this; larger is corrupt
	u32Size     = 4
	crcSize     = 4
)

var (
	_ Record = Put{}
	_ Record = Delete{}
)
