package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Decode parses a version-1 stream. On any corrupt input it returns (nil, ErrCorrupt).
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	if b[0] != version1 {
		return nil, ErrCorrupt
	}
	rest := b[1:]
	var out []Record
	for len(rest) > 0 {
		rec, next, err := decodeFrame(rest)
		if err != nil {
			return nil, ErrCorrupt
		}
		out = append(out, rec)
		rest = next
	}
	if out == nil {
		out = []Record{}
	}
	return out, nil
}

func decodeFrame(b []byte) (Record, []byte, error) {
	if len(b) < u32Size {
		return nil, nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b[:u32Size])
	if n > maxFrameLen {
		return nil, nil, ErrCorrupt
	}
	if n < 1 {
		return nil, nil, ErrCorrupt
	}
	frameLen := u32Size + int(n) + crcSize
	if len(b) < frameLen {
		return nil, nil, ErrCorrupt
	}
	body := b[u32Size : u32Size+int(n)]
	got := binary.BigEndian.Uint32(b[u32Size+int(n) : frameLen])
	if crc32.ChecksumIEEE(body) != got {
		return nil, nil, ErrCorrupt
	}
	var (
		rec Record
		err error
	)
	switch body[0] {
	case kindPut:
		var p Put
		p, err = decodePutPayload(body[1:])
		rec = p
	case kindDelete:
		var d Delete
		d, err = decodeDeletePayload(body[1:])
		rec = d
	default:
		return nil, nil, ErrCorrupt
	}
	if err != nil {
		return nil, nil, ErrCorrupt
	}
	return rec, b[frameLen:], nil
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
