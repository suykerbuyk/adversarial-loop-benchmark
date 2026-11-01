package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// This is the reusable, depth-aware frame reader/writer. It knows the framing
// (len / type / payload / crc) and the caps, and it dispatches payloads through
// the registry. It is written once and reused at every nesting level: a v1 top
// level and a v2 Batch sub-stream call the exact same encodeFrames/readFrames.

// maxLen caps a frame's len (type+payload) at 16 MiB, at every depth.
const maxLen = 16 << 20

// maxDepth caps nesting. The top-level stream is depth 0; a sub-stream deeper
// than maxDepth is ErrCorrupt. v1 never nests, but the reader is depth-aware so
// nesting drops in without reworking the framing.
const maxDepth = 8

// buildFrame wraps a type tag and its payload into [len][type][payload][crc].
func buildFrame(typ uint8, payload []byte) ([]byte, error) {
	n := 1 + len(payload) // len counts type + payload
	if n > maxLen {
		return nil, ErrCorrupt
	}
	out := make([]byte, 4+n+4)
	binary.BigEndian.PutUint32(out[0:4], uint32(n))
	out[4] = typ
	copy(out[5:], payload)
	crc := crc32.ChecksumIEEE(out[4 : 4+n]) // crc over type + payload
	binary.BigEndian.PutUint32(out[4+n:], crc)
	return out, nil
}

// encodeFrames encodes a sequence of records into a framed sub-stream at the
// given depth. Each record's payload comes from its registered codec.
func encodeFrames(recs []Record, depth int) ([]byte, error) {
	if depth > maxDepth {
		return nil, ErrCorrupt
	}
	var out []byte
	for _, rec := range recs {
		c, ok := registry[rec.Kind()]
		if !ok {
			return nil, ErrCorrupt
		}
		payload, err := c.encode(rec, depth)
		if err != nil {
			return nil, err
		}
		frame, err := buildFrame(rec.Kind(), payload)
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
	}
	return out, nil
}

// readFrames decodes a framed sub-stream (exactly the bytes in data) into
// records at the given depth. A len/crc/type/truncation fault is ErrCorrupt. An
// inner len that would read past data is a truncated frame, never a read beyond.
func readFrames(data []byte, depth int) ([]Record, error) {
	if depth > maxDepth {
		return nil, ErrCorrupt
	}
	var recs []Record
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(data[0:4])
		if n > maxLen || n < 1 { // oversize, or no room for a type byte
			return nil, ErrCorrupt
		}
		// need n bytes (type+payload) and 4 bytes (crc) after the len field
		if uint64(len(data)) < 4+uint64(n)+4 {
			return nil, ErrCorrupt
		}
		typePayload := data[4 : 4+n]
		crc := binary.BigEndian.Uint32(data[4+n : 4+n+4])
		if crc32.ChecksumIEEE(typePayload) != crc {
			return nil, ErrCorrupt
		}
		c, ok := registry[typePayload[0]]
		if !ok {
			return nil, ErrCorrupt
		}
		rec, err := c.decode(typePayload[1:], depth)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		data = data[4+n+4:]
	}
	return recs, nil
}

// readLenBytes reads a [uint32 len][bytes] field, returning a fresh copy of the
// bytes (len 0 → nil) and the remaining input. Used by the record codecs.
func readLenBytes(b []byte) (val []byte, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b[0:4])
	if n > maxLen {
		return nil, nil, ErrCorrupt
	}
	b = b[4:]
	if uint64(len(b)) < uint64(n) {
		return nil, nil, ErrCorrupt
	}
	val = append([]byte(nil), b[:n]...) // copy; n == 0 yields nil
	return val, b[n:], nil
}

// appendLenBytes appends a [uint32 len][bytes] field.
func appendLenBytes(dst, b []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	dst = append(dst, hdr[:]...)
	return append(dst, b...)
}
