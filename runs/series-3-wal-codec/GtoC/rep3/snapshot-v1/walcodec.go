package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Record is one log entry. Concrete types return a stable type tag from Kind.
type Record interface {
	Kind() uint8
}

// Put is a key/value write. Kind is 1.
type Put struct {
	Key   string
	Value []byte
}

// Delete removes a key. Kind is 2.
type Delete struct {
	Key string
}

func (Put) Kind() uint8    { return kindPut }
func (Delete) Kind() uint8 { return kindDelete }

// ErrCorrupt is the error Decode returns for every format failure: a missing
// or unknown version, a truncated frame, an oversize length, a CRC mismatch,
// an unknown type tag, or a payload whose interior lengths do not match the
// frame. Decode returns this sentinel directly so errors.Is matches it.
//
// Encode does not return ErrCorrupt. Its errors are a nil or unknown record,
// a frame that would exceed the 16 MiB cap, and a stream that would exceed
// the addressable size of an int. They come with a nil buffer.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1  = 1
	kindPut    = 1
	kindDelete = 2
	// maxFrameLen is the inclusive cap on len (type + payload).
	maxFrameLen = 16 << 20
	headerSize  = 4
	crcSize     = 4
	u32Size     = 4
)

// maxInt is the largest value that fits in an int on this architecture.
const maxInt = int(^uint(0) >> 1)

// kindCodec is one kind's size, write, and decode. The codecs table is indexed
// by Kind(), so adding a kind is one entry and the encoder cannot drift from
// its decoder on the tag.
type kindCodec struct {
	bodyLen func(Record) (int64, error)
	write   func([]byte, Record)
	decode  func([]byte) (Record, error)
}

// codecs is indexed by the tags Kind returns. A nil slot is an unknown tag.
// Only v1 kinds are registered.
var codecs = [kindDelete + 1]*kindCodec{
	kindPut: {
		bodyLen: putBodyLen,
		write:   writePut,
		decode:  decodePut,
	},
	kindDelete: {
		bodyLen: deleteBodyLen,
		write:   writeDelete,
		decode:  decodeDelete,
	},
}

// Encode writes a version-1 stream: one version byte, then one frame per record
// in order.
//
// Only Put and Delete values are encodable. Kind has a value receiver, so *Put
// and *Delete also satisfy Record, but Encode rejects those pointer forms (and
// a nil Record, and any other implementation). Pass Put and Delete by value.
// Decode always returns values, never pointers.
//
// Encode returns a non-ErrCorrupt error, with a nil buffer, when a record is
// not an encodable value, when a frame's type+payload would exceed 16 MiB, or
// when the summed stream would exceed the addressable size of an int. The
// error text is not a contract; match encode failures by errors.Is against
// ErrCorrupt being false, not by string comparison.
func Encode(recs []Record) ([]byte, error) {
	// Integer preflight only. Each record is sized once, then written once
	// into a buffer of that exact capacity. An interface holds a copy of the
	// Put or Delete value, so the write sees the lengths the preflight measured.
	total := int64(1)
	bodies := make([]int64, len(recs))
	kinds := make([]uint8, len(recs))
	for i, rec := range recs {
		c, kind, err := codecOf(rec)
		if err != nil {
			return nil, err
		}
		body, err := c.bodyLen(rec)
		if err != nil {
			return nil, err
		}
		bodies[i] = body
		kinds[i] = kind
		total += int64(headerSize) + body + int64(crcSize)
	}
	if total > int64(maxInt) {
		return nil, fmt.Errorf("walcodec: stream exceeds addressable size")
	}
	buf := make([]byte, 1, int(total))
	buf[0] = versionV1
	for i, rec := range recs {
		buf = appendFrame(buf, kinds[i], bodies[i], rec)
	}
	return buf, nil
}

// codecOf resolves a record without calling Kind. A nil *Put or *Delete
// satisfies Record, and calling Kind on it panics, so only the value forms
// are accepted. The tag then comes from Kind() and indexes the shared table.
func codecOf(rec Record) (*kindCodec, uint8, error) {
	var kind uint8
	switch r := rec.(type) {
	case Put:
		kind = r.Kind()
	case Delete:
		kind = r.Kind()
	default:
		return nil, 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	if int(kind) >= len(codecs) || codecs[kind] == nil {
		return nil, 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	return codecs[kind], kind, nil
}

// appendFrame writes one frame into the capacity Encode reserved. bodyN is the
// preflight measurement; the write extends length within that capacity and
// does not size the record again.
func appendFrame(buf []byte, kind uint8, bodyN int64, rec Record) []byte {
	start := len(buf)
	wire := headerSize + int(bodyN) + crcSize
	buf = buf[:start+wire]
	binary.BigEndian.PutUint32(buf[start:], uint32(bodyN))
	body := buf[start+headerSize : start+headerSize+int(bodyN)]
	body[0] = kind
	codecs[kind].write(body[1:], rec)
	binary.BigEndian.PutUint32(buf[start+headerSize+int(bodyN):], crc32.ChecksumIEEE(body))
	return buf
}

func putBodyLen(rec Record) (int64, error) {
	p, ok := rec.(Put)
	if !ok {
		return 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	return sizedBody(true, len(p.Key), len(p.Value))
}

func deleteBodyLen(rec Record) (int64, error) {
	d, ok := rec.(Delete)
	if !ok {
		return 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	return sizedBody(false, len(d.Key), 0)
}

// sizedBody is the type+payload length. It rejects a body over the cap before
// the caller allocates anything for that frame. Lengths are int64 so a large
// field cannot wrap an int on 32-bit platforms before the cap rejects it.
func sizedBody(hasVal bool, keyLen, valLen int) (int64, error) {
	n := int64(1+u32Size) + int64(keyLen)
	if hasVal {
		n += int64(u32Size) + int64(valLen)
	}
	if n > maxFrameLen {
		return 0, fmt.Errorf("walcodec: frame exceeds %d byte cap", maxFrameLen)
	}
	return n, nil
}

func writePut(payload []byte, rec Record) {
	p := rec.(Put)
	writePrefixed(payload, p.Key)
	writePrefixedBytes(payload[u32Size+len(p.Key):], p.Value)
}

func writeDelete(payload []byte, rec Record) {
	d := rec.(Delete)
	writePrefixed(payload, d.Key)
}

func writePrefixed(dst []byte, src string) {
	binary.BigEndian.PutUint32(dst, uint32(len(src)))
	copy(dst[u32Size:], src)
}

func writePrefixedBytes(dst, src []byte) {
	binary.BigEndian.PutUint32(dst, uint32(len(src)))
	copy(dst[u32Size:], src)
}

// Decode parses a version-1 stream into records in stream order.
// On any format error it returns nil records and ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	parse, err := frameParserFor(b[0])
	if err != nil {
		return nil, err
	}
	if len(b) == 1 {
		return []Record{}, nil
	}
	off := 1
	var recs []Record
	for off < len(b) {
		rec, n, err := parse(b[off:])
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		off += n
	}
	return recs, nil
}

// frameParser is one version's frame reader. It returns the record and the
// number of bytes consumed from the start of the remaining buffer.
type frameParser func([]byte) (Record, int, error)

// frameParserFor selects the parser for a version byte. v1 is the only
// version this decoder knows; any other byte is corrupt, before any frame.
func frameParserFor(version byte) (frameParser, error) {
	if version != versionV1 {
		return nil, ErrCorrupt
	}
	return decodeFrameV1, nil
}

// decodeFrameV1 reads one version-1 frame. An oversize or zero len is
// rejected before any allocation.
func decodeFrameV1(b []byte) (Record, int, error) {
	if len(b) < headerSize {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(b[:headerSize])
	// Cap before any allocation or further read. n == 0 cannot hold a type byte.
	if n > maxFrameLen || n < 1 {
		return nil, 0, ErrCorrupt
	}
	// n is at most maxFrameLen, so this sum fits in int even on 32-bit platforms.
	// Compare against len(b) rather than adding an offset that could wrap.
	need := headerSize + int(n) + crcSize
	if len(b) < need {
		return nil, 0, ErrCorrupt
	}
	body := b[headerSize : headerSize+int(n)]
	gotCRC := binary.BigEndian.Uint32(b[headerSize+int(n) : need])
	if crc32.ChecksumIEEE(body) != gotCRC {
		return nil, 0, ErrCorrupt
	}
	rec, err := decodePayload(body[0], body[1:])
	if err != nil {
		return nil, 0, err
	}
	return rec, need, nil
}

func decodePayload(kind byte, payload []byte) (Record, error) {
	if int(kind) >= len(codecs) || codecs[kind] == nil {
		return nil, ErrCorrupt
	}
	return codecs[kind].decode(payload)
}

func decodePut(payload []byte) (Record, error) {
	if len(payload) < u32Size {
		return nil, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[:u32Size])
	// 4 + klen + 4 must fit before the key is sliced. uint64 compare so a
	// huge klen cannot wrap the offset.
	if uint64(u32Size)+uint64(klen)+uint64(u32Size) > uint64(len(payload)) {
		return nil, ErrCorrupt
	}
	keyEnd := u32Size + int(klen)
	key := string(payload[u32Size:keyEnd])
	vlen := binary.BigEndian.Uint32(payload[keyEnd : keyEnd+u32Size])
	valStart := keyEnd + u32Size
	if uint64(valStart)+uint64(vlen) != uint64(len(payload)) {
		return nil, ErrCorrupt
	}
	// make returns a non-nil slice, including when vlen is 0, and the copy
	// detaches Value from the input buffer.
	value := make([]byte, vlen)
	copy(value, payload[valStart:valStart+int(vlen)])
	return Put{Key: key, Value: value}, nil
}

func decodeDelete(payload []byte) (Record, error) {
	if len(payload) < u32Size {
		return nil, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[:u32Size])
	if uint64(u32Size)+uint64(klen) != uint64(len(payload)) {
		return nil, ErrCorrupt
	}
	key := string(payload[u32Size : u32Size+int(klen)])
	return Delete{Key: key}, nil
}
