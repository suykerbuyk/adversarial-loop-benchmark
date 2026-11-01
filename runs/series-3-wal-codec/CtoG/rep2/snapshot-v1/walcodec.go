// Package walcodec encodes and decodes a versioned write-ahead-log record stream.
//
// A stream is a single version byte followed by zero or more frames:
//
//	stream = version:u8 frame*
//	frame  = len:u32be type:u8 payload:[len-1] crc:u32be
//
// len counts the type byte and payload only; crc is CRC32-IEEE over
// type+payload and sits outside len. len is capped at 16 MiB.
//
// Payloads:
//
//	Put    = klen:u32be key[klen] vlen:u32be value[vlen]
//	Delete = klen:u32be key[klen]
package walcodec

import "errors"

// Record is one WAL entry. Kind returns its wire type tag.
type Record interface{ Kind() uint8 }

// Put sets Key to Value. Its Kind is 1.
//
// A nil Value and an empty Value encode identically; both decode to a
// non-nil, length-0 slice.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes Key. Its Kind is 2.
type Delete struct{ Key string }

// Kind returns the wire type tag of a Put (1).
func (Put) Kind() uint8 { return kindPut }

// Kind returns the wire type tag of a Delete (2).
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is wrapped by every error Decode returns. Test with errors.Is.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1    byte   = 1        // the only version v1 writes and accepts
	kindPut     uint8  = 1        // wire tag of Put
	kindDelete  uint8  = 2        // wire tag of Delete
	maxFrameLen uint32 = 16 << 20 // cap on the frame len field (type+payload)
	lenSize            = 4        // frame len field
	typeSize           = 1        // frame type byte
	crcSize            = 4        // frame crc trailer
	u32Size            = 4        // klen / vlen prefixes
)

// payloadLen is the byte size of a record's payload (excluding the type byte).
// All arithmetic is in uint64 so it cannot overflow for any real input.
func payloadLen(kind uint8, keyLen, valLen uint64) uint64 {
	p := u32Size + keyLen
	if kind == kindPut {
		p += u32Size + valLen
	}
	return p
}

// frameLenField is the value written in a frame's len field: type + payload.
func frameLenField(payload uint64) uint64 { return typeSize + payload }

// frameWireSize is the full on-wire frame size: len + type + payload + crc.
func frameWireSize(payload uint64) uint64 { return lenSize + frameLenField(payload) + crcSize }
