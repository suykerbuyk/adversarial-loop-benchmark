package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

// maxStreamSize is the largest stream Encode will build: the largest length a
// []byte can have on this platform.
const maxStreamSize uint64 = math.MaxInt

// Encode serializes recs, in order, into a version-1 stream. An empty or nil
// recs yields a one-byte, version-only stream.
//
// Encode accepts Put and Delete values and non-nil pointers to them. It returns
// an error (never ErrCorrupt) and no output for a nil record, a nil pointer, any
// other Record type, a record whose frame would exceed the 16 MiB limit, or a
// stream too large to address on this platform.
func Encode(recs []Record) ([]byte, error) {
	// Validation pass: check each record and compute the exact output
	// size with overflow-checked arithmetic, so the write pass below cannot
	// fail and the single allocation is never sized by a wrapped integer.
	total := uint64(1) // version byte
	for i, r := range recs {
		bodyLen, err := frameBodyLen(r)
		if err != nil {
			return nil, fmt.Errorf("walcodec: record %d: %w", i, err)
		}
		var ok bool
		total, ok = addStreamSize(total, lenFieldSize+bodyLen+crcFieldSize, maxStreamSize)
		if !ok {
			return nil, fmt.Errorf("walcodec: record %d: encoded stream exceeds %d bytes", i, maxStreamSize)
		}
	}

	buf := make([]byte, 1, int(total))
	buf[0] = version1
	for _, r := range recs {
		// Every r was validated above, so pointers are non-nil.
		switch rec := r.(type) {
		case Put:
			buf = appendPut(buf, rec)
		case *Put:
			buf = appendPut(buf, *rec)
		case Delete:
			buf = appendDelete(buf, rec)
		case *Delete:
			buf = appendDelete(buf, *rec)
		}
	}
	return buf, nil
}

func appendPut(buf []byte, p Put) []byte {
	return appendFrame(buf, kindPut, func(b []byte) []byte {
		b = appendLenPrefixed(b, []byte(p.Key))
		return appendLenPrefixed(b, p.Value)
	})
}

func appendDelete(buf []byte, d Delete) []byte {
	return appendFrame(buf, kindDelete, func(b []byte) []byte {
		return appendLenPrefixed(b, []byte(d.Key))
	})
}

// frameBodyLen returns the frame body length (type+payload) of r. It rejects
// unsupported records and frames over the 16 MiB cap, so Encode never emits a
// stream its own Decode would reject.
func frameBodyLen(r Record) (uint64, error) {
	var n uint64
	switch v := r.(type) {
	case Put:
		n = putFrameLen(v)
	case *Put:
		if v == nil {
			return 0, errors.New("nil *Put")
		}
		n = putFrameLen(*v)
	case Delete:
		n = deleteFrameLen(v)
	case *Delete:
		if v == nil {
			return 0, errors.New("nil *Delete")
		}
		n = deleteFrameLen(*v)
	default:
		return 0, fmt.Errorf("unsupported record type %T", r)
	}
	if n > uint64(maxFrameLen) {
		return 0, fmt.Errorf("frame length %d exceeds %d-byte limit", n, maxFrameLen)
	}
	return n, nil
}

// addStreamSize returns total+n and true if the sum does not exceed limit
// (and does not wrap); otherwise it returns total and false.
func addStreamSize(total, n, limit uint64) (uint64, bool) {
	if n > limit || total > limit-n {
		return total, false
	}
	return total + n, true
}

func putFrameLen(p Put) uint64 {
	return typeSize + lenFieldSize + uint64(len(p.Key)) + lenFieldSize + uint64(len(p.Value))
}

func deleteFrameLen(d Delete) uint64 {
	return typeSize + lenFieldSize + uint64(len(d.Key))
}

// appendFrame appends len, type, the payload written by writePayload, and the
// crc over type+payload. The caller has already checked the frame length cap.
func appendFrame(buf []byte, kind uint8, writePayload func([]byte) []byte) []byte {
	start := len(buf)
	buf = append(buf, 0, 0, 0, 0) // len placeholder
	buf = append(buf, kind)
	buf = writePayload(buf)
	body := buf[start+lenFieldSize:]
	binary.BigEndian.PutUint32(buf[start:], uint32(len(body)))
	return binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(body))
}

// appendLenPrefixed appends [uint32 len(p)][p]. Callers guarantee len(p) fits
// within the frame cap.
func appendLenPrefixed(buf, p []byte) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(p)))
	return append(buf, p...)
}
