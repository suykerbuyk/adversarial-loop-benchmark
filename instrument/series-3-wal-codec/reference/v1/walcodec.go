// Package walcodec encodes a sequence of write-ahead-log records to a framed
// byte stream and decodes it back. This is the v1 snapshot of the CORRECT,
// EXTENSIBLE reference: a type-keyed dispatch table (see registry.go) and a
// reusable, depth-aware frame reader/writer (see frame.go). v1 handles Put and
// Delete; v2 adds Batch + recursion purely additively (a new file that registers
// one more codec and extends the version set), without editing these files.
package walcodec

import "errors"

// Record is the common interface. Each concrete type returns its wire tag.
type Record interface{ Kind() uint8 }

// Put stores a key and its value.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put wire tag (1).
func (Put) Kind() uint8 { return kindPut }

// Delete marks a key as deleted.
type Delete struct {
	Key string
}

// Kind returns the Delete wire tag (2).
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is returned for any malformed or unreadable stream.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

// Encode writes a version byte then one frame per record, in order.
func Encode(recs []Record) ([]byte, error) {
	body, err := encodeFrames(recs, 0)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(body))
	out = append(out, writeVersion)
	out = append(out, body...)
	return out, nil
}

// Decode validates the version byte, then decodes every frame in order.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt // missing version byte
	}
	if !acceptedVersions[b[0]] {
		return nil, ErrCorrupt
	}
	return readFrames(b[1:], 0)
}
