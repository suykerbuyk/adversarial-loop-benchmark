package walcodec

// v2 addition — the WHOLE of it. Batch nests a framed sub-stream, so it needs a
// recursive codec. Because v1 is a type-keyed dispatch over a reusable,
// depth-aware frame reader/writer, adding it touches nothing: this file
// registers one more codec, flips the version set, and makes one recursive call
// in each direction (encodeFrames / readFrames at depth+1). The v1 files are
// byte-for-byte unchanged between the v1 and v2 snapshots.

import "encoding/binary"

const kindBatch uint8 = 3

// Batch holds an ordered list of records; its payload is a nested sub-stream.
type Batch struct {
	Records []Record
}

// Kind returns the Batch wire tag (3).
func (Batch) Kind() uint8 { return kindBatch }

func init() {
	register(kindBatch, recordCodec{encode: encodeBatch, decode: decodeBatch})
	acceptedVersions[2] = true // Decode now accepts version 2 as well as 1
	writeVersion = 2           // Encode now writes version 2
}

// Batch payload = [n uint32] then n inner frames. Encoding recurses one level
// deeper through the shared frame writer, which enforces the depth cap.
func encodeBatch(rec Record, depth int) ([]byte, error) {
	b := rec.(Batch)
	body, err := encodeFrames(b.Records, depth+1)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(b.Records)))
	return append(out, body...), nil
}

// decodeBatch reads the count, then decodes exactly that many inner frames one
// level deeper through the shared frame reader. The inner frames must consume
// the Batch payload exactly; a short, long, or over-deep sub-stream is
// ErrCorrupt.
func decodeBatch(payload []byte, depth int) (Record, error) {
	if len(payload) < 4 {
		return nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[0:4])
	inner, err := readFrames(payload[4:], depth+1)
	if err != nil {
		return nil, err
	}
	if uint64(len(inner)) != uint64(n) { // declared count must match exactly
		return nil, ErrCorrupt
	}
	return Batch{Records: inner}, nil
}
