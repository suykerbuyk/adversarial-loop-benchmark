package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
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

// Batch is a nested sequence of records. Kind is 3.
//
// A nil Records slice and an empty Records slice both encode as n = 0
// followed by no inner frames. Decode of that payload returns Records set
// to a non-nil empty slice. Nil and empty are equal for this codec: compare
// with len, or with a non-nil empty slice. reflect.DeepEqual treats nil and
// empty as different, so a Batch encoded with a nil Records does not
// DeepEqual the value Decode returns. This is the same equality rule as
// Put.Value (nil and empty both encode as length 0 and both decode as a
// non-nil length-0 slice) and the same shape as top-level Decode of a
// version-only stream, which returns []Record{}, not nil.
type Batch struct {
	Records []Record
}

func (Put) Kind() uint8    { return kindPut }
func (Delete) Kind() uint8 { return kindDelete }
func (Batch) Kind() uint8  { return kindBatch }

// ErrCorrupt is the error Decode returns for every format failure: a missing
// version, a version other than 1 or 2, a truncated frame, an oversize
// length, a CRC mismatch, an unknown type tag, a payload whose interior
// lengths do not match the frame, an inner frame that escapes its enclosing
// payload, an n that does not match the payload, or a sub-stream deeper
// than maxDepth. Decode returns this sentinel directly so errors.Is matches
// it.
//
// Encode does not return ErrCorrupt. Its errors are a nil or unknown record,
// a frame that would exceed the 16 MiB cap, a stream that would exceed the
// addressable size of an int, and nesting whose sub-stream depth would
// exceed maxDepth. They come with a nil buffer.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

const (
	versionV1 = 1
	versionV2 = 2
	kindPut   = 1
	kindDelete = 2
	kindBatch = 3
	// maxFrameLen is the inclusive cap on len (type + payload).
	maxFrameLen = 16 << 20
	headerSize  = 4
	crcSize     = 4
	u32Size     = 4
	// maxDepth is the deepest legal sub-stream. The top-level stream is depth 0.
	// A sub-stream at depth greater than maxDepth is rejected. Eight wrapping
	// Batches are legal; a ninth is not.
	maxDepth = 8
)

// minFrameWire is a safe lower bound on any frame this format can hold:
// 4 (len) + 1 (type) + 4 (crc). A future kind with an empty payload is still
// at least this large. A kind that needs a smaller wire cannot exist under
// the current frame shape, so this bound does not need to be revisited when
// a kind is added. It is not the 13-byte minimum of today's Put, Delete, and
// Batch payloads. Using 9 instead of 13 only lets a few more impossible n
// values reach the append loop; the loop still allocates only for records
// that have already been decoded.
const minFrameWire = headerSize + 1 + crcSize // 9

// maxInt is the largest value that fits in an int on this architecture.
const maxInt = int(^uint(0) >> 1)

// frameSize is one measured frame. Encode builds this tree once and the
// write pass replays it. children is nil for Put and Delete.
type frameSize struct {
	codec    *kindCodec
	kind     uint8
	bodyN    int64 // type + payload
	rec      Record
	children []frameSize
}

// codecTable is indexed by kind. A short table or a nil slot is an unknown tag.
type codecTable struct {
	byKind []*kindCodec
}

// kindCodec is one kind's size, write, and decode.
//
// bodyLen measures rec at depth. The int is that record's stream depth.
// Put and Delete ignore depth and return nil children. Batch returns the
// child tree from this single walk. Signature: (Record, depth int) ->
// (bodyN int64, children []frameSize, err error).
//
// write fills a payload region whose length bodyLen already reserved.
// It has no error return: every failure (unknown type, cap, depth, n
// that does not fit in uint32) is returned by bodyLen during preflight.
// It does not call bodyLen, does not check depth, and does not read the
// caller's Records slice. Put and Delete ignore children.
//
// decode parses a payload whose outer frame CRC has already been checked.
// The int is this record's stream depth. Put and Delete ignore it.
// Signature: (payload []byte, depth int) -> (Record, error).
// The table is not a parameter. decodeBatch reads the version-2 table
// variable, which init has already filled.
//
// Tables are filled in init and not mutated after.
type kindCodec struct {
	bodyLen func(Record, int) (int64, []frameSize, error)
	write   func([]byte, Record, []frameSize)
	decode  func([]byte, int) (Record, error)
}

// Declared empty so the composite literals do not mention batchCodec.
// init fills them once. Nothing mutates them after init.
//
// encodeTable is the encoder kinds. A later version re-points this.
// decodeV2Table is the version-2 kinds only. Nested Batch uses this, not encodeTable.
// decodeV1Table is the version-1 kinds only. Kind 3 is unknown.
// init fills them to avoid the batchCodec initialization cycle.
var encodeTable codecTable
var decodeV2Table codecTable
var decodeV1Table codecTable

var putCodec kindCodec
var deleteCodec kindCodec
var batchCodec kindCodec

func init() {
	putCodec = kindCodec{
		bodyLen: putBodyLen,
		write:   writePut,
		decode:  decodePut,
	}
	deleteCodec = kindCodec{
		bodyLen: deleteBodyLen,
		write:   writeDelete,
		decode:  decodeDelete,
	}
	batchCodec = kindCodec{
		bodyLen: batchBodyLen,
		write:   writeBatch,
		decode:  decodeBatch,
	}
	v2 := []*kindCodec{
		kindPut:    &putCodec,
		kindDelete: &deleteCodec,
		kindBatch:  &batchCodec, // index 3
	}
	encodeTable = codecTable{byKind: v2}
	// Same slice today. A future version re-points encodeTable only.
	decodeV2Table = codecTable{byKind: v2}
	decodeV1Table = codecTable{byKind: []*kindCodec{
		kindPut:    &putCodec,
		kindDelete: &deleteCodec,
	}}
}

// Encode writes a version-2 stream: one version byte, then one frame per record
// in order. Put/Delete-only input and the empty stream are version 2 as well.
//
// Encodable values are Put, Delete, and Batch. Kind has a value receiver, so
// *Put, *Delete, and *Batch also satisfy Record, but Encode rejects those
// pointer forms (and a nil Record, and any other implementation). Pass values.
// Decode always returns values, never pointers.
//
// Encode returns a non-ErrCorrupt error, with a nil buffer, when a record is
// not an encodable value, when a frame's type+payload would exceed 16 MiB,
// when the summed stream would exceed the addressable size of an int, or when
// nesting would put a sub-stream deeper than maxDepth. The error text is not
// a contract; match encode failures by errors.Is against ErrCorrupt being
// false, not by string comparison.
func Encode(recs []Record) ([]byte, error) {
	// Integer preflight only. Each record is sized once, then written once
	// into a buffer of that exact capacity. An interface holds a copy of the
	// Put, Delete, or Batch value, so the write sees the lengths the
	// preflight measured. Nested records are captured on the child tree.
	total := int64(1)
	sized := make([]frameSize, len(recs))
	for i, rec := range recs {
		fs, err := measure(rec, 0)
		if err != nil {
			return nil, err
		}
		sized[i] = fs
		total += int64(headerSize) + fs.bodyN + int64(crcSize)
		if total > int64(maxInt) {
			return nil, fmt.Errorf("walcodec: stream exceeds addressable size")
		}
	}
	buf := make([]byte, 1, int(total))
	buf[0] = versionV2
	for i := range sized {
		buf = appendFrame(buf, sized[i])
	}
	return buf, nil
}

// measure sizes rec at depth. It is the only recursive walk. The write pass
// replays the returned tree and does not call bodyLen again.
func measure(rec Record, depth int) (frameSize, error) {
	c, kind, err := codecOf(rec)
	if err != nil {
		return frameSize{}, err
	}
	bodyN, children, err := c.bodyLen(rec, depth)
	if err != nil {
		return frameSize{}, err
	}
	return frameSize{
		codec:    c,
		kind:     kind,
		bodyN:    bodyN,
		rec:      rec,
		children: children,
	}, nil
}

// codecOf resolves a record without calling Kind. A nil *Put, *Delete, or
// *Batch satisfies Record, and calling Kind on it panics, so only the value
// forms are accepted. Lookup is encodeTable only. Nested records go through
// this same switch.
func codecOf(rec Record) (*kindCodec, uint8, error) {
	var kind uint8
	switch r := rec.(type) {
	case Put:
		kind = r.Kind()
	case Delete:
		kind = r.Kind()
	case Batch:
		kind = r.Kind()
	default:
		return nil, 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	if int(kind) >= len(encodeTable.byKind) || encodeTable.byKind[kind] == nil {
		return nil, 0, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	return encodeTable.byKind[kind], kind, nil
}

// appendFrame writes one frame into the capacity Encode reserved. It replays
// a frameSize and does not index a global array. The body slice is
// three-index, cap equal to length, so a child write that overruns panics
// instead of spilling into the outer CRC or the next frame. Nested frames
// come from the child tree. It does not size the record again.
func appendFrame(buf []byte, sized frameSize) []byte {
	start := len(buf)
	bodyN := sized.bodyN
	wire := headerSize + int(bodyN) + crcSize
	buf = buf[:start+wire]
	binary.BigEndian.PutUint32(buf[start:], uint32(bodyN))
	// body is exactly bodyN bytes. Cap equals length, so a child write that
	// overruns panics instead of spilling into the outer CRC or the next frame.
	body := buf[start+headerSize : start+headerSize+int(bodyN) : start+headerSize+int(bodyN)]
	body[0] = sized.kind
	sized.codec.write(body[1:], sized.rec, sized.children)
	binary.BigEndian.PutUint32(buf[start+headerSize+int(bodyN):], crc32.ChecksumIEEE(body))
	return buf
}

func putBodyLen(rec Record, depth int) (int64, []frameSize, error) {
	p, ok := rec.(Put)
	if !ok {
		return 0, nil, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	n, err := sizedBody(true, len(p.Key), len(p.Value))
	if err != nil {
		return 0, nil, err
	}
	return n, nil, nil
}

func deleteBodyLen(rec Record, depth int) (int64, []frameSize, error) {
	d, ok := rec.(Delete)
	if !ok {
		return 0, nil, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	n, err := sizedBody(false, len(d.Key), 0)
	if err != nil {
		return 0, nil, err
	}
	return n, nil, nil
}

// batchBodyLen measures a Batch at depth. The sub-stream sits at depth+1.
// The child tree is built once; writeBatch replays it.
func batchBodyLen(rec Record, depth int) (int64, []frameSize, error) {
	b, ok := rec.(Batch)
	if !ok {
		return 0, nil, fmt.Errorf("walcodec: unknown record type %T", rec)
	}
	if depth+1 > maxDepth {
		return 0, nil, fmt.Errorf("walcodec: batch nesting exceeds depth %d", maxDepth)
	}
	if uint64(len(b.Records)) > math.MaxUint32 {
		return 0, nil, fmt.Errorf("walcodec: batch record count exceeds uint32")
	}
	children := make([]frameSize, 0, len(b.Records))
	// body is type byte + n field + sum of child wires.
	body := int64(1 + u32Size)
	for _, child := range b.Records {
		fs, err := measure(child, depth+1)
		if err != nil {
			return 0, nil, err
		}
		wire := int64(headerSize) + fs.bodyN + int64(crcSize)
		if body > int64(maxInt)-wire {
			return 0, nil, fmt.Errorf("walcodec: frame exceeds %d byte cap", maxFrameLen)
		}
		body += wire
		if body > maxFrameLen {
			return 0, nil, fmt.Errorf("walcodec: frame exceeds %d byte cap", maxFrameLen)
		}
		children = append(children, fs)
	}
	return body, children, nil
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

func writePut(payload []byte, rec Record, children []frameSize) {
	p := rec.(Put)
	writePrefixed(payload, p.Key)
	writePrefixedBytes(payload[u32Size+len(p.Key):], p.Value)
}

func writeDelete(payload []byte, rec Record, children []frameSize) {
	d := rec.(Delete)
	writePrefixed(payload, d.Key)
}

// writeBatch writes n then one frame per child into the payload region
// appendFrame already reserved. It does not re-measure and does not read
// rec.(Batch).Records.
func writeBatch(payload []byte, rec Record, children []frameSize) {
	binary.BigEndian.PutUint32(payload, uint32(len(children)))
	// Length 0, cap equal to the reserved child bytes.
	written := payload[u32Size:u32Size:len(payload)]
	for i := range children {
		written = appendFrame(written, children[i])
	}
	if len(written) != len(payload)-u32Size {
		panic("walcodec: measure/write length mismatch")
	}
}

func writePrefixed(dst []byte, src string) {
	binary.BigEndian.PutUint32(dst, uint32(len(src)))
	copy(dst[u32Size:], src)
}

func writePrefixedBytes(dst, src []byte) {
	binary.BigEndian.PutUint32(dst, uint32(len(src)))
	copy(dst[u32Size:], src)
}

// Decode parses a version-1 stream (Put and Delete only) or a version-2
// stream (Put, Delete, and Batch) into records in stream order. Any other
// version is ErrCorrupt before any frame. Version 2 uses decodeV2Table.
// On any format error it returns nil records and ErrCorrupt.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	var table *codecTable
	switch b[0] {
	case versionV1:
		table = &decodeV1Table
	case versionV2:
		table = &decodeV2Table
	default:
		return nil, ErrCorrupt
	}
	if len(b) == 1 {
		return []Record{}, nil
	}
	return readRecords(b[1:], table, 0, -1)
}

// readRecords walks remaining with readFrame until the slice is empty.
// When limit >= 0, it stops after that many records and reports leftover
// bytes as ErrCorrupt. limit < 0 means consume the slice exactly, with no
// declared count (the top-level stream).
func readRecords(b []byte, table *codecTable, depth int, limit int) ([]Record, error) {
	var recs []Record
	rest := b
	if limit < 0 {
		for len(rest) > 0 {
			rec, n, err := readFrame(rest, table, depth)
			if err != nil {
				return nil, err
			}
			recs = append(recs, rec)
			rest = rest[n:]
		}
		if recs == nil {
			recs = []Record{}
		}
		return recs, nil
	}
	for i := 0; i < limit; i++ {
		rec, n, err := readFrame(rest, table, depth)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return nil, ErrCorrupt
	}
	if recs == nil {
		recs = []Record{}
	}
	return recs, nil
}

// readFrame reads one frame from b. b is the remaining enclosing slice, never
// an offset into a parent buffer. table selects legal kinds. depth is the
// stream depth of the record this frame contains. An oversize or zero len is
// rejected before any allocation. The slice bound is the enclosing payload:
// an inner len that would read past b is ErrCorrupt (truncated).
func readFrame(b []byte, table *codecTable, depth int) (Record, int, error) {
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
	kind := body[0]
	if int(kind) >= len(table.byKind) || table.byKind[kind] == nil {
		return nil, 0, ErrCorrupt
	}
	rec, err := table.byKind[kind].decode(body[1:], depth)
	if err != nil {
		return nil, 0, err
	}
	return rec, need, nil
}

func decodePut(payload []byte, depth int) (Record, error) {
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

func decodeDelete(payload []byte, depth int) (Record, error) {
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

// decodeBatch parses a Batch payload at depth. The inner sub-stream is one
// level deeper. depth+1 > maxDepth is ErrCorrupt before the sub-stream is
// opened, including an empty Batch. n is untrusted: nothing is allocated
// from it. Inner reads use decodeV2Table.
func decodeBatch(payload []byte, depth int) (Record, error) {
	if depth+1 > maxDepth {
		return nil, ErrCorrupt
	}
	if len(payload) < u32Size {
		return nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[:u32Size])
	rest := len(payload) - u32Size
	if uint64(n) > uint64(rest)/uint64(minFrameWire) {
		return nil, ErrCorrupt
	}
	recs, err := readRecords(payload[u32Size:], &decodeV2Table, depth+1, int(n))
	if err != nil {
		return nil, err
	}
	return Batch{Records: recs}, nil
}
