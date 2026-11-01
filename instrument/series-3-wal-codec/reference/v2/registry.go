package walcodec

// This file is the type-keyed dispatch table — the seam that makes the codec
// extensible. A new record type is added by registering one more recordCodec;
// the frame reader/writer in frame.go never needs to change. v2 adds Batch by
// appending a registration and extending the version set, editing nothing here.

const (
	kindPut    uint8 = 1
	kindDelete uint8 = 2
)

// recordCodec is one entry in the dispatch table. encode produces a record's
// payload (the bytes after the type tag); decode parses a payload back into a
// record. Both receive the current nesting depth so a composite type (Batch)
// can recurse through the shared frame reader/writer.
type recordCodec struct {
	encode func(rec Record, depth int) ([]byte, error)
	decode func(payload []byte, depth int) (Record, error)
}

// registry maps a wire tag to its codec.
var registry = map[uint8]recordCodec{}

// register installs a codec for a tag. Called from init() in the files that
// define each record type.
func register(tag uint8, c recordCodec) { registry[tag] = c }

// acceptedVersions is the set of stream versions Decode will accept, and
// writeVersion is the version Encode writes. v2 extends both by appending an
// init(), so bumping the format is itself an additive change.
var acceptedVersions = map[uint8]bool{1: true}
var writeVersion uint8 = 1
