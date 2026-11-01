package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a version-1 or version-2 stream. Records are returned in
// stream order; Batches are returned as Batch values. Every error wraps
// ErrCorrupt, and on error no records are returned. Decoded records never
// alias b.
//
// Errors are preallocated, and no allocation is sized by an unvalidated
// length or count field, so an error found before any record is built
// allocates nothing. An error found after some records were decoded discards
// them; their allocation is not undone.
//
// Heap use is linear in len(b) but is NOT bounded by len(b) itself. Every
// frame takes at least 13 input bytes, yet each decoded record costs a
// boxed record header (16-48 bytes), a 16-byte slot in its parent's
// []Record (plus append growth slack), and copies of its key and value. A
// stream of minimal records (empty Deletes, Puts, or Batches) therefore
// allocates up to roughly 10x len(b) in total, success or failure. Decode
// imposes no record-count limit of its own; callers decoding untrusted input
// should cap len(b) with that factor in mind.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, errNoVersion
	}
	var allowBatch bool
	switch b[0] {
	case version1:
		allowBatch = false // Put/Delete only
	case version2:
		allowBatch = true
	default:
		return nil, errBadVersion
	}

	var recs []Record // grown lazily: nothing is allocated before a frame validates
	for off := 1; off < len(b); {
		rec, n, err := decodeFrame(b[off:], 0, allowBatch)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off += n
	}
	if recs == nil {
		recs = []Record{}
	}
	return recs, nil
}

// decodeFrame decodes the frame at the start of p and returns the record and
// the number of bytes consumed. depth is the depth of the (sub-)stream that
// contains the frame; the top level is 0. p never extends past the enclosing
// payload, so an inner len that overruns it is errTruncated.
func decodeFrame(p []byte, depth int, allowBatch bool) (Record, int, error) {
	if len(p) < lenFieldSize {
		return nil, 0, errShortHeader
	}
	n := binary.BigEndian.Uint32(p)
	if n > maxFrameLen {
		// Rejected before anything beyond the header is read or allocated.
		return nil, 0, errOversize
	}
	if n == 0 {
		return nil, 0, errEmptyFrame
	}
	end := lenFieldSize + int(n)
	need := end + crcFieldSize
	if len(p) < need {
		return nil, 0, errTruncated
	}
	body := p[lenFieldSize:end]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(p[end:need]) {
		return nil, 0, errCRC
	}

	var (
		rec Record
		err error
	)
	switch body[0] {
	case KindPut:
		rec, err = parsePut(body[1:])
	case KindDelete:
		rec, err = parseDelete(body[1:])
	case kindBatch:
		if !allowBatch {
			return nil, 0, errUnknownType
		}
		rec, err = parseBatch(body[1:], depth)
	default:
		return nil, 0, errUnknownType
	}
	if err != nil {
		return nil, 0, err
	}
	return rec, need, nil
}

// parseBatch decodes a Batch payload p belonging to a frame in a sub-stream
// at depth. Its records form a sub-stream at depth+1. p is exactly the
// Batch's payload, so inner frames can never read into a sibling or outer
// frame.
func parseBatch(p []byte, depth int) (Record, error) {
	inner := depth + 1
	if inner > maxDepth {
		return nil, errTooDeep
	}
	if len(p) < batchCountSize {
		return nil, errBatchCount
	}
	n := binary.BigEndian.Uint32(p)
	rest := p[batchCountSize:]
	var recs []Record // lazy: the untrusted count n is never used as a capacity
	for i := uint32(0); i < n; i++ {
		if len(rest) == 0 {
			return nil, errBatchShort
		}
		rec, used, err := decodeFrame(rest, inner, true)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		rest = rest[used:]
	}
	if len(rest) != 0 {
		return nil, errTrailing
	}
	if recs == nil {
		recs = []Record{}
	}
	return Batch{Records: recs}, nil
}

// readBytes splits a [uint32 len][bytes] field off the front of p.
func readBytes(p []byte) (field, rest []byte, err error) {
	if len(p) < u32Size {
		return nil, nil, errPrefixOverrun
	}
	l := binary.BigEndian.Uint32(p)
	p = p[u32Size:]
	if uint64(l) > uint64(len(p)) {
		return nil, nil, errPrefixOverrun
	}
	return p[:l], p[l:], nil
}

func parsePut(p []byte) (Record, error) {
	k, p, err := readBytes(p)
	if err != nil {
		return nil, err
	}
	v, p, err := readBytes(p)
	if err != nil {
		return nil, err
	}
	if len(p) != 0 {
		return nil, errTrailing
	}
	return Put{Key: string(k), Value: copyBytes(v)}, nil
}

func parseDelete(p []byte) (Record, error) {
	k, p, err := readBytes(p)
	if err != nil {
		return nil, err
	}
	if len(p) != 0 {
		return nil, errTrailing
	}
	return Delete{Key: string(k)}, nil
}

// copyBytes returns a non-nil copy of v, so nil and empty values decode
// identically and never alias the input.
func copyBytes(v []byte) []byte {
	return append([]byte{}, v...)
}
