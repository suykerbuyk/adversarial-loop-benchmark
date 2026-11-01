// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(u32) type(1) payload(len-1) crc(u32)
//
// len counts type+payload only; crc is CRC32-IEEE over type+payload and sits
// outside len. len is capped at 16 MiB.
//
//	Put    payload = klen(u32) key vlen(u32) value
//	Delete payload = klen(u32) key
//
// This package writes and reads version 1 only.
package walcodec

import "errors"

const (
	version1    uint8  = 1        // the only version this codec writes and reads
	kindPut     uint8  = 1        // frame type tag for Put
	kindDelete  uint8  = 2        // frame type tag for Delete
	maxFrameLen uint32 = 16 << 20 // cap on a frame's len field (type+payload)

	lenFieldSize = 4 // size of a uint32 length field (frame len, klen, vlen)
	crcFieldSize = 4 // size of the trailing crc field
	typeSize     = 1 // size of the frame type tag
)

// ErrCorrupt is returned by Decode for any malformed, truncated, oversize, or
// checksum-failing stream, and for an unknown version or record type.
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

// Kind returns 1, the wire tag for Put.
func (Put) Kind() uint8 { return kindPut }

// Kind returns 2, the wire tag for Delete.
func (Delete) Kind() uint8 { return kindDelete }

var (
	_ Record = Put{}
	_ Record = Delete{}
)
