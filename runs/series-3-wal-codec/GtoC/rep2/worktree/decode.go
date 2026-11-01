package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a version-1 stream (Put and Delete only) or a version-2 stream
// (Put, Delete, and nested Batch). On any corrupt input it returns (nil, ErrCorrupt).
//
// Each frame's CRC is checked before its payload is trusted, including frames
// nested inside a Batch. An outer CRC does not replace those inner checks.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	version := b[0]
	if version != version1 && version != version2 {
		return nil, ErrCorrupt
	}
	// kinds is the set of type tags this version admits. A later version that
	// adds a kind changes this mask in one place; the frame parsers already
	// take the set rather than a single allowBatch flag.
	var kinds uint8
	switch version {
	case version1:
		kinds = kindBit(kindPut) | kindBit(kindDelete)
	case version2:
		kinds = kindBit(kindPut) | kindBit(kindDelete) | kindBit(kindBatch)
	default:
		return nil, ErrCorrupt
	}
	return decodeStream(capSlice(b[1:]), kinds, 0)
}

func kindBit(kind byte) uint8 {
	return 1 << kind
}

func kindAllowed(kinds uint8, kind byte) bool {
	if kind > 7 {
		return false
	}
	return kinds&kindBit(kind) != 0
}

// capSlice returns p with cap == len so a later reslice cannot reach bytes
// that sit past p in the backing array (a sibling frame or an outer CRC).
func capSlice(p []byte) []byte {
	return p[:len(p):len(p)]
}

func decodeStream(buf []byte, kinds uint8, depth int) ([]Record, error) {
	out := []Record{}
	for len(buf) > 0 {
		rec, consumed, err := decodeFrame(buf, kinds, depth)
		if err != nil {
			return nil, ErrCorrupt
		}
		out = append(out, rec)
		buf = buf[consumed:]
	}
	return out, nil
}

func decodeFrame(buf []byte, kinds uint8, depth int) (Record, int, error) {
	if len(buf) < u32Size {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(buf[:u32Size])
	if n == 0 || n > maxFrameLen {
		return nil, 0, ErrCorrupt
	}
	frameLen := u32Size + int(n) + crcSize
	if len(buf) < frameLen {
		return nil, 0, ErrCorrupt
	}
	// Three-index slice: cap stops at the body so a child parser cannot
	// read the CRC or any byte past this frame by reslicing up to cap.
	body := buf[u32Size : u32Size+int(n) : u32Size+int(n)]
	got := binary.BigEndian.Uint32(buf[u32Size+int(n) : frameLen])
	if crc32.ChecksumIEEE(body) != got {
		return nil, 0, ErrCorrupt
	}
	typ := body[0]
	if !kindAllowed(kinds, typ) {
		return nil, 0, ErrCorrupt
	}
	payload := capSlice(body[1:])
	var (
		rec Record
		err error
	)
	switch typ {
	case kindPut:
		var p Put
		p, err = decodePutPayload(payload)
		rec = p
	case kindDelete:
		var d Delete
		d, err = decodeDeletePayload(payload)
		rec = d
	case kindBatch:
		var batch Batch
		batch, err = decodeBatchPayload(payload, kinds, depth)
		rec = batch
	default:
		return nil, 0, ErrCorrupt
	}
	if err != nil {
		return nil, 0, ErrCorrupt
	}
	return rec, frameLen, nil
}

func decodeBatchPayload(payload []byte, kinds uint8, depth int) (Batch, error) {
	if len(payload) < u32Size {
		return Batch{}, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[:u32Size])
	rest := capSlice(payload[u32Size:])
	if uint64(n) > uint64(len(rest)/minWireFrameBytes) {
		return Batch{}, ErrCorrupt
	}
	child, ok := childDepth(depth)
	if !ok {
		return Batch{}, ErrCorrupt
	}
	recs := make([]Record, 0, int(n))
	for i := uint32(0); i < n; i++ {
		rec, consumed, err := decodeFrame(rest, kinds, child)
		if err != nil {
			return Batch{}, ErrCorrupt
		}
		recs = append(recs, rec)
		rest = capSlice(rest[consumed:])
	}
	if len(rest) != 0 {
		return Batch{}, ErrCorrupt
	}
	return Batch{Records: recs}, nil
}

func decodePutPayload(p []byte) (Put, error) {
	key, rest, err := takeString(p)
	if err != nil {
		return Put{}, err
	}
	val, rest, err := takeBytes(rest)
	if err != nil {
		return Put{}, err
	}
	if len(rest) != 0 {
		return Put{}, ErrCorrupt
	}
	return Put{Key: key, Value: val}, nil
}

func decodeDeletePayload(p []byte) (Delete, error) {
	key, rest, err := takeString(p)
	if err != nil {
		return Delete{}, err
	}
	if len(rest) != 0 {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: key}, nil
}

func takeBytes(p []byte) ([]byte, []byte, error) {
	if len(p) < u32Size {
		return nil, nil, ErrCorrupt
	}
	ln := binary.BigEndian.Uint32(p[:u32Size])
	p = p[u32Size:]
	if uint64(ln) > uint64(len(p)) {
		return nil, nil, ErrCorrupt
	}
	n := int(ln)
	dst := make([]byte, n)
	copy(dst, p[:n])
	return dst, p[n:], nil
}

func takeString(p []byte) (string, []byte, error) {
	if len(p) < u32Size {
		return "", nil, ErrCorrupt
	}
	ln := binary.BigEndian.Uint32(p[:u32Size])
	p = p[u32Size:]
	if uint64(ln) > uint64(len(p)) {
		return "", nil, ErrCorrupt
	}
	n := int(ln)
	return string(p[:n]), p[n:], nil
}
