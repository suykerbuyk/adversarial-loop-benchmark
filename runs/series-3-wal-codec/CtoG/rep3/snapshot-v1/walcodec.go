// Package walcodec encodes and decodes a versioned, framed write-ahead-log
// record stream.
//
// Wire format (all integers big-endian):
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len counts type+payload only (the crc is outside len) and is capped at
// 16 MiB. crc is CRC32-IEEE over type+payload. Payloads:
//
//	Put    = klen(4) key vlen(4) value
//	Delete = klen(4) key
//
// Encode writes version 1, the only version this package decodes.
package walcodec

import "errors"

// Record is a single log entry. Kind reports the record's wire type tag.
type Record interface{ Kind() uint8 }

// Put records that Key was set to Value. A nil and an empty Value are
// equivalent on the wire and both decode to a non-nil, length-0 slice.
type Put struct {
	Key   string
	Value []byte
}

// Delete records that Key was removed.
type Delete struct{ Key string }

// Kind returns the Put type tag, 1.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag, 2.
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned by Decode for any malformed stream: missing or
// unknown version byte, truncated frame, oversize or zero len, crc mismatch,
// unknown type tag, or malformed payload.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1     = 1 // the only version this decoder knows; written by Encode
	kindPut      = 1
	kindDelete   = 2
	lenSize      = 4        // frame len field
	crcSize      = 4        // frame crc field
	u32Size      = 4        // string/bytes length prefix
	maxFrameLen  = 16 << 20 // cap on frame len (type+payload)
	minFrameLen  = 1        // len counts the type byte, so a frame has len >= 1
	minFrameSize = lenSize + minFrameLen + crcSize
)

// Encode-side errors. They describe caller mistakes and are never ErrCorrupt.
var (
	errUnsupportedRecord = errors.New("walcodec: unsupported record type")
	errRecordTooLarge    = errors.New("walcodec: record exceeds frame size limit")
	errOutputTooLarge    = errors.New("walcodec: encoded stream too large")
)
