// Package walcodec encodes WAL records as versioned frames. A frame is a
// big-endian uint32 body length, a type byte and payload, then a big-endian
// CRC32-IEEE of the type and payload. Each body is limited to 16 MiB.
// Version 1 contains Put and Delete; version 2 also contains Batch. A Batch
// contains a uint32 count followed by exactly that many complete frames, with
// no trailing bytes. Its nested stream is limited to eight levels (top level
// is depth zero). Decode accepts versions 1 and 2 and returns bare ErrCorrupt
// with nil records for malformed input.
package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	version1       = 1
	encodeVersion  = 2
	putTag         = 1
	deleteTag      = 2
	batchTag       = 3
	uint32Len      = 4
	crcLen         = 4
	putFixedLen    = 1 + uint32Len + uint32Len
	deleteFixedLen = 1 + uint32Len
	// minFrame includes the smallest payload: an empty-key Delete (4 bytes).
	minFrame = uint32Len + 1 + uint32Len + crcLen
	maxBody  = 16 << 20
	maxDepth = 8
)

// ErrCorrupt indicates an invalid or unsupported WAL stream or record.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnsupportedRecord = errors.New("walcodec: unsupported record")
	errFrameTooLarge     = errors.New("walcodec: frame exceeds 16 MiB limit")
	errBatchTooDeep      = fmt.Errorf("walcodec: batch nesting exceeds %d: %w", maxDepth, ErrCorrupt)
)

// Record is a WAL operation. Supported values are Put, Delete, and Batch.
type Record interface{ Kind() uint8 }

// Put stores Value at Key.
type Put struct {
	Key   string
	Value []byte
}

// Kind returns the Put record kind (1).
func (Put) Kind() uint8 { return putTag }

// Delete removes Key.
type Delete struct{ Key string }

// Kind returns the Delete record kind (2).
func (Delete) Kind() uint8 { return deleteTag }

// Batch groups records in a nested framed stream. At most eight Batch levels
// are permitted; recursive or cyclic values are rejected by Encode's depth
// limit. Decode converts nil or empty Records to a non-nil empty slice.
type Batch struct{ Records []Record }

// Kind returns the Batch record kind (3).
func (Batch) Kind() uint8 { return batchTag }

// Encode returns a version-2 stream. Frame lengths exclude the trailing CRC;
// Put and Delete payloads use big-endian uint32 lengths followed by bytes.
// Frames use CRC32-IEEE checksums and each body is limited to 16 MiB. It rejects nil,
// pointer, and foreign record implementations with errors distinct from
// ErrCorrupt. Excessive nesting returns an error matching ErrCorrupt. Any
// error returns nil bytes.
func Encode(records []Record) ([]byte, error) {
	out := []byte{encodeVersion}
	for _, r := range records {
		var err error
		out, err = appendFrame(out, r, 0)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func appendFrame(dst []byte, r Record, depth int) ([]byte, error) {
	if depth > maxDepth {
		return nil, errBatchTooDeep
	}
	if r == nil {
		return nil, fmt.Errorf("%w: <nil>", errUnsupportedRecord)
	}
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	bodyStart := len(dst)
	switch v := r.(type) {
	case Put:
		bodyLen := uint64(putFixedLen) + uint64(len(v.Key)) + uint64(len(v.Value))
		if bodyLen > maxBody {
			return nil, errFrameTooLarge
		}
		dst = append(dst, putTag)
		dst = appendU32(dst, uint32(len(v.Key)))
		dst = append(dst, v.Key...)
		dst = appendU32(dst, uint32(len(v.Value)))
		dst = append(dst, v.Value...)
	case Delete:
		bodyLen := uint64(deleteFixedLen) + uint64(len(v.Key))
		if bodyLen > maxBody {
			return nil, errFrameTooLarge
		}
		dst = append(dst, deleteTag)
		dst = appendU32(dst, uint32(len(v.Key)))
		dst = append(dst, v.Key...)
	case Batch:
		if depth >= maxDepth {
			return nil, errBatchTooDeep
		}
		dst = append(dst, batchTag)
		dst = appendU32(dst, 0)
		count := uint64(len(v.Records))
		if count > uint64(^uint32(0)) {
			return nil, errFrameTooLarge
		}
		for _, child := range v.Records {
			var err error
			dst, err = appendFrame(dst, child, depth+1)
			if err != nil {
				return nil, err
			}
			if uint64(len(dst)-bodyStart) > maxBody {
				return nil, errFrameTooLarge
			}
		}
		binary.BigEndian.PutUint32(dst[bodyStart+1:bodyStart+5], uint32(count))
	default:
		return nil, fmt.Errorf("%w: %T", errUnsupportedRecord, r)
	}
	bodyLen := uint64(len(dst) - bodyStart)
	if bodyLen == 0 || bodyLen > maxBody {
		return nil, errFrameTooLarge
	}
	binary.BigEndian.PutUint32(dst[start:start+4], uint32(bodyLen))
	crc := crc32.ChecksumIEEE(dst[bodyStart:])
	dst = appendU32(dst, crc)
	return dst, nil
}

func appendU32(dst []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, v)
}

// Decode accepts version 1 Put/Delete streams and version 2 streams with
// nested Batches; version 1 rejects Batch. Malformed input always returns nil
// records and bare ErrCorrupt. Batch payloads must contain exactly their
// declared frames. Empty Put values decode as non-nil empty slices; record
// ordering and duplicates are preserved.
func Decode(data []byte) ([]Record, error) {
	if len(data) < 1 || (data[0] != version1 && data[0] != encodeVersion) {
		return nil, ErrCorrupt
	}
	recs, ok := parseSequence(data[1:], data[0], 0)
	if !ok {
		return nil, ErrCorrupt
	}
	return recs, nil
}

func parseSequence(data []byte, version byte, depth int) ([]Record, bool) {
	if depth > maxDepth {
		return nil, false
	}
	records := make([]Record, 0)
	for len(data) != 0 {
		r, rest, ok := parseFrame(data, version, depth)
		if !ok {
			return nil, false
		}
		records = append(records, r)
		data = rest
	}
	return records, true
}

func parseRecord(body []byte, version byte, depth int) (Record, bool) {
	if len(body) == 0 {
		return nil, false
	}
	switch body[0] {
	case putTag:
		key, rest, ok := takeString(body[1:])
		if !ok {
			return nil, false
		}
		if len(rest) < 4 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(rest[:4])
		if uint64(n) != uint64(len(rest)-4) {
			return nil, false
		}
		value := make([]byte, int(n))
		copy(value, rest[4:])
		return Put{Key: key, Value: value}, true
	case deleteTag:
		key, rest, ok := takeString(body[1:])
		if !ok || len(rest) != 0 {
			return nil, false
		}
		return Delete{Key: key}, true
	case batchTag:
		if version != encodeVersion || depth >= maxDepth || len(body) < uint32Len+1 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(body[1:5])
		payload := body[5:]
		if uint64(n) > uint64(len(payload))/uint64(minFrame) {
			return nil, false
		}
		children := make([]Record, 0, int(n))
		for i := uint32(0); i < n; i++ {
			child, rest, ok := parseFrame(payload, version, depth+1)
			if !ok {
				return nil, false
			}
			children = append(children, child)
			payload = rest
		}
		if len(payload) != 0 {
			return nil, false
		}
		return Batch{Records: children}, true
	default:
		return nil, false
	}
}

func parseFrame(data []byte, version byte, depth int) (Record, []byte, bool) {
	if len(data) < uint32Len {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(data[:uint32Len])
	if n == 0 || n > maxBody {
		return nil, nil, false
	}
	need := uint64(uint32Len) + uint64(n) + crcLen
	if need > uint64(len(data)) {
		return nil, nil, false
	}
	end := uint32Len + int(n)
	body := data[uint32Len:end]
	want := binary.BigEndian.Uint32(data[end : end+crcLen])
	if crc32.ChecksumIEEE(body) != want {
		return nil, nil, false
	}
	r, ok := parseRecord(body, version, depth)
	return r, data[int(need):], ok
}

func takeString(b []byte) (string, []byte, bool) {
	if len(b) < 4 {
		return "", nil, false
	}
	n := binary.BigEndian.Uint32(b[:4])
	if uint64(n) > uint64(len(b)-4) {
		return "", nil, false
	}
	return string(b[4 : 4+int(n)]), b[4+int(n):], true
}
