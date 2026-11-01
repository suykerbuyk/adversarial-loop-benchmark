package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Encode writes a version-2 stream: version byte, then one frame per record,
// in slice order. A nest past depth 8 returns bare ErrCorrupt (err == ErrCorrupt
// and errors.Is both hold). An unsupported concrete type and a frame body that
// would exceed 16 MiB return unexported errors that do not match ErrCorrupt.
//
// Each frame carries its own CRC32-IEEE over type+payload, including frames
// nested inside a Batch. The outer CRC covers those inner frames again. That
// repeated hash is required: a later change must not skip an inner CRC.
func Encode(recs []Record) ([]byte, error) {
	dst := []byte{version2}
	for _, rec := range recs {
		var err error
		dst, err = appendFrame(dst, rec, 0)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

func appendU32(dst []byte, v uint32) []byte {
	var buf [u32Size]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

func putBodyLen(r Put) (int, error) {
	keyLen := len(r.Key)
	valLen := len(r.Value)
	if keyLen > maxFrameLen || valLen > maxFrameLen {
		return 0, errTooLarge
	}
	bodyLen := 1 + u32Size + keyLen + u32Size + valLen
	if bodyLen > maxFrameLen {
		return 0, errTooLarge
	}
	return bodyLen, nil
}

func deleteBodyLen(r Delete) (int, error) {
	keyLen := len(r.Key)
	if keyLen > maxFrameLen {
		return 0, errTooLarge
	}
	bodyLen := 1 + u32Size + keyLen
	if bodyLen > maxFrameLen {
		return 0, errTooLarge
	}
	return bodyLen, nil
}

func appendFrame(dst []byte, rec Record, depth int) ([]byte, error) {
	switch r := rec.(type) {
	case Put:
		bodyLen, err := putBodyLen(r)
		if err != nil {
			return nil, err
		}
		return appendClosedFrame(dst, kindPut, bodyLen, func(dst []byte) []byte {
			return appendPutPayload(dst, r)
		}), nil
	case Delete:
		bodyLen, err := deleteBodyLen(r)
		if err != nil {
			return nil, err
		}
		return appendClosedFrame(dst, kindDelete, bodyLen, func(dst []byte) []byte {
			return appendDeletePayload(dst, r)
		}), nil
	case Batch:
		child, ok := childDepth(depth)
		if !ok {
			// Bare ErrCorrupt so callers may use err == ErrCorrupt or errors.Is.
			return nil, ErrCorrupt
		}
		return appendBatchFrame(dst, r, child)
	default:
		return nil, errUnsupported
	}
}

// appendClosedFrame writes a Put or Delete frame whose bodyLen is already
// known and at most maxFrameLen. It cannot fail: the payload writer only
// appends bytes the caller already sized.
func appendClosedFrame(dst []byte, typ byte, bodyLen int, writePayload func([]byte) []byte) []byte {
	start := len(dst)
	dst = appendU32(dst, uint32(bodyLen))
	dst = append(dst, typ)
	dst = writePayload(dst)
	return appendU32(dst, crc32.ChecksumIEEE(dst[start+u32Size:]))
}

func appendPutPayload(dst []byte, r Put) []byte {
	dst = appendU32(dst, uint32(len(r.Key)))
	dst = append(dst, r.Key...)
	dst = appendU32(dst, uint32(len(r.Value)))
	return append(dst, r.Value...)
}

func appendDeletePayload(dst []byte, r Delete) []byte {
	dst = appendU32(dst, uint32(len(r.Key)))
	return append(dst, r.Key...)
}

func appendBatchFrame(dst []byte, batch Batch, child int) ([]byte, error) {
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0) // reserved len
	dst = append(dst, kindBatch)
	dst = appendU32(dst, uint32(len(batch.Records)))
	for _, rec := range batch.Records {
		var err error
		dst, err = appendFrame(dst, rec, child)
		if err != nil {
			return nil, err
		}
		bodyLen := len(dst) - start - u32Size
		if bodyLen > maxFrameLen {
			return nil, errTooLarge
		}
	}
	bodyLen := len(dst) - start - u32Size
	if bodyLen > maxFrameLen {
		return nil, errTooLarge
	}
	binary.BigEndian.PutUint32(dst[start:start+u32Size], uint32(bodyLen))
	return appendU32(dst, crc32.ChecksumIEEE(dst[start+u32Size:])), nil
}
