package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a stream produced by Encode. Records are returned in stream
// order. Every error wraps ErrCorrupt, and on error no records are returned.
// Decoded records never alias b.
func Decode(b []byte) ([]Record, error) {
	if len(b) == 0 {
		return nil, errNoVersion
	}
	switch b[0] {
	case version1:
	default:
		return nil, errBadVersion
	}

	var recs []Record // grown lazily: nothing is allocated before a frame validates
	for off := 1; off < len(b); {
		rec, n, err := decodeFrame(b[off:])
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
// the number of bytes consumed.
func decodeFrame(p []byte) (Record, int, error) {
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
	default:
		return nil, 0, errUnknownType
	}
	if err != nil {
		return nil, 0, err
	}
	return rec, need, nil
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
