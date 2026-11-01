// Package walcodec encodes and decodes a versioned write-ahead-log record stream.
//
// A stream is a single version byte followed by zero or more frames:
//
//	stream = version:u8 frame*
//	frame  = len:u32be type:u8 payload:[len-1] crc:u32be
//
// len counts the type byte and payload only; crc is CRC32-IEEE over
// type+payload and sits outside len. len is capped at 16 MiB at every depth.
//
// Payloads:
//
//	Put    = klen:u32be key[klen] vlen:u32be value[vlen]   (type 1)
//	Delete = klen:u32be key[klen]                           (type 2)
//	Batch  = n:u32be frame{n}                               (type 3)
//
// A Batch payload is a nested sub-stream of exactly n complete frames (each
// with its own len and crc) that must consume the payload exactly. Batches
// may nest. The top-level stream is depth 0; a Batch's sub-stream is one
// deeper than the stream holding the Batch; a sub-stream deeper than 8 is
// corrupt, so at most 8 levels of Batch nesting are legal.
//
// Versions: Encode writes version 2. Decode accepts version 1 (Put and
// Delete only) and version 2 (Put, Delete and Batch); any other version is
// corrupt.
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

// Batch groups Records into one frame whose payload is a nested framed
// sub-stream. Its Kind is 3. Records may include Put, Delete and nested
// Batch values, up to 8 levels of Batch nesting.
//
// A decoded Batch always has a non-nil Records slice (length 0 when empty).
type Batch struct{ Records []Record }

// Kind returns the wire type tag of a Put (1).
func (Put) Kind() uint8 { return kindPut }

// Kind returns the wire type tag of a Delete (2).
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the wire type tag of a Batch (3).
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is wrapped by every error Decode returns. Test with errors.Is.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	version1    byte   = 1        // accepted for decode (Put/Delete only), never written
	version2    byte   = 2        // the version Encode writes; Decode accepts it
	kindPut     uint8  = 1        // wire tag of Put
	kindDelete  uint8  = 2        // wire tag of Delete
	kindBatch   uint8  = 3        // wire tag of Batch (version 2 only)
	maxFrameLen uint32 = 16 << 20 // cap on the frame len field (type+payload), every depth
	maxDepth           = 8        // deepest legal sub-stream (top level is 0)
	lenSize            = 4        // frame len field
	typeSize           = 1        // frame type byte
	crcSize            = 4        // frame crc trailer
	u32Size            = 4        // klen / vlen / batch count prefixes

	// minFrameWire is a lower bound on the wire size of any frame, because
	// every payload kind starts with a u32 (klen or n). Used only to bound
	// record counts and capacity hints. It is not a payload-validity floor;
	// payload validity is checked per kind by the parsers.
	minFrameWire = lenSize + typeSize + u32Size + crcSize
)

// payloadLen is the byte size of a Put/Delete payload (excluding the type
// byte). All arithmetic is in uint64 so it cannot overflow for any real input.
func payloadLen(kind uint8, keyLen, valLen uint64) uint64 {
	p := u32Size + keyLen
	if kind == kindPut {
		p += u32Size + valLen
	}
	return p
}

// frameLenField is the value written in a frame's len field: type + payload.
func frameLenField(payload uint64) uint64 { return typeSize + payload }
