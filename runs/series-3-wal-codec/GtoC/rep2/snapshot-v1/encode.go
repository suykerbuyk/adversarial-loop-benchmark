package walcodec

import (
	"encoding/binary"
	"hash/crc32"
)

// Encode writes a version-1 stream: version byte, then one frame per record, in slice order.
func Encode(recs []Record) ([]byte, error) {
	if len(recs) == 0 {
		return []byte{version1}, nil
	}
	dst := []byte{version1}
	for _, rec := range recs {
		var err error
		dst, err = encodeOne(dst, rec)
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

func encodeOne(dst []byte, rec Record) ([]byte, error) {
	switch r := rec.(type) {
	case Put:
		return appendPut(dst, r)
	case Delete:
		return appendDelete(dst, r)
	default:
		return nil, errUnsupported
	}
}

func appendPut(dst []byte, r Put) ([]byte, error) {
	keyLen := len(r.Key)
	valLen := len(r.Value)
	if keyLen > maxFrameLen || valLen > maxFrameLen {
		return nil, errTooLarge
	}
	bodyLen := 1 + u32Size + keyLen + u32Size + valLen
	if bodyLen > maxFrameLen {
		return nil, errTooLarge
	}
	start := len(dst)
	dst = appendU32(dst, uint32(bodyLen))
	dst = append(dst, kindPut)
	dst = appendU32(dst, uint32(keyLen))
	dst = append(dst, r.Key...)
	dst = appendU32(dst, uint32(valLen))
	dst = append(dst, r.Value...)
	body := dst[start+u32Size:]
	return appendU32(dst, crc32.ChecksumIEEE(body)), nil
}

func appendDelete(dst []byte, r Delete) ([]byte, error) {
	keyLen := len(r.Key)
	if keyLen > maxFrameLen {
		return nil, errTooLarge
	}
	bodyLen := 1 + u32Size + keyLen
	if bodyLen > maxFrameLen {
		return nil, errTooLarge
	}
	start := len(dst)
	dst = appendU32(dst, uint32(bodyLen))
	dst = append(dst, kindDelete)
	dst = appendU32(dst, uint32(keyLen))
	dst = append(dst, r.Key...)
	body := dst[start+u32Size:]
	return appendU32(dst, crc32.ChecksumIEEE(body)), nil
}
