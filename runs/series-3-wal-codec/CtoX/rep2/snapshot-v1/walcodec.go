// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len counts type+payload only; crc is CRC32-IEEE over type+payload and sits
// outside len. len is capped at 16 MiB. Payloads are:
//
//	Put    = [klen uint32][key][vlen uint32][value]
//	Delete = [klen uint32][key]
//
// Encode accepts Put, Delete, *Put and *Delete records; Decode always returns
// value records (Put, Delete), so pointer identity is not preserved.
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

// Kind returns the Put type tag, 1.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag, 2.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned (possibly wrapped) by Decode for any malformed stream.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1    uint8  = 1        // the only version v1 writes and accepts
	kindPut     uint8  = 1        // Put type tag
	kindDelete  uint8  = 2        // Delete type tag
	maxFrameLen uint32 = 16 << 20 // cap on a frame's len field
	lenSize            = 4        // size of the uint32 len prefix
	crcSize            = 4        // size of the uint32 crc trailer
)
