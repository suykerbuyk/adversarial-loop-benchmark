package walcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Record is one write-ahead-log entry. Put, Delete, and Batch are the concrete
// records. Encode accepts those values only: Kind uses a value receiver, so
// *Put, *Delete, and *Batch also satisfy Record, but Encode rejects pointers.
// Decode returns Put, Delete, and Batch values, never pointers.
type Record interface {
	Kind() uint8
}

// Put records a key and value. A nil Value and an empty Value are equal:
// both encode as vlen 0 and both decode to a non-nil length-0 slice.
type Put struct {
	Key   string
	Value []byte
}

// Delete records removal of a key.
type Delete struct {
	Key string
}

// Batch groups records. Its payload is a framed sub-stream: a uint32 count
// followed by that many full frames. A Batch may contain Put, Delete, or
// nested Batch values. An empty Records slice and a nil Records slice both
// encode as n = 0. Decode returns a non-nil empty Records slice for that case.
type Batch struct {
	Records []Record
}

const (
	versionV1 uint8 = 1
	versionV2 uint8 = 2

	kindPut    uint8 = 1
	kindDelete uint8 = 2
	kindBatch  uint8 = 3

	// maxFrameLen is the largest legal frame length word (type + payload).
	// A length equal to the cap is legal; a length above it is ErrCorrupt.
	// The same cap applies at every nesting depth.
	maxFrameLen = 16 << 20

	// maxDepth is the deepest legal sub-stream. The top-level stream is depth
	// 0. A Batch's inner sub-stream is decoded and encoded one level deeper
	// than the Batch frame itself, including an empty Batch. A sub-stream at
	// depth greater than maxDepth is rejected. Eight levels of Batch nesting
	// are legal; a ninth is not.
	maxDepth = 8

	// minInnerFrame is the smallest legal inner frame: 4-byte length, 1 type
	// byte, 4-byte body (empty Delete or empty Batch), and 4-byte CRC.
	minInnerFrame = 13
)

// Kind returns the Put type tag.
func (Put) Kind() uint8 { return kindPut }

// Kind returns the Delete type tag.
func (Delete) Kind() uint8 { return kindDelete }

// Kind returns the Batch type tag.
func (Batch) Kind() uint8 { return kindBatch }

// ErrCorrupt is returned for every wire failure: a missing or unknown version,
// a truncated frame, a CRC mismatch, an unknown type tag, an oversize length,
// a payload whose inner lengths do not match the frame, a Batch payload
// shorter than 4 bytes, an n the payload cannot hold, a count that does not
// match the frames that follow, trailing bytes after n frames, an inner length
// that extends past the enclosing payload, or a nest deeper than 8. Decode
// returns it as the bare sentinel. Encode of a nest deeper than 8 returns an
// error that wraps it.
var ErrCorrupt = errors.New("walcodec: corrupt stream")

var (
	errUnknownRecord = errors.New("walcodec: unknown record")
	errFrameTooLarge = errors.New("walcodec: frame exceeds 16MiB")
	// errStreamTooLarge is a top-level size overflow. No single frame is over
	// the 16 MiB cap; the running total does not fit in int. On 386 that is
	// reachable with legal frames.
	errStreamTooLarge = errors.New("walcodec: stream too large")
	// errTooDeep is an encode-time nesting failure. The amendment says a
	// deeper nest is ErrCorrupt without limiting that sentence to Decode, so
	// the error wraps ErrCorrupt. Callers inside the package can still match
	// errTooDeep.
	errTooDeep = fmt.Errorf("walcodec: batch nesting exceeds 8: %w", ErrCorrupt)
)

// frameParser parses a CRC-verified payload for one kind. nil means the kind
// is not a record type this package knows.
type frameParser func(d decoder, payload []byte) (Record, error)

// kindRow is one entry in the kind dispatch table.
type kindRow struct {
	v1, v2 bool
	parse  frameParser
}

// kindTable is the single kind dispatch. Version gates and decode both read
// it, so a new kind is one row plus the encode switches, not a second list
// that can drift from the decoder. It is filled in init: a composite literal
// here would cycle, because the parsers call back into decodeFrame.
var kindTable [256]kindRow

func init() {
	kindTable[kindPut] = kindRow{v1: true, v2: true, parse: parsePutRecord}
	kindTable[kindDelete] = kindRow{v1: true, v2: true, parse: parseDeleteRecord}
	kindTable[kindBatch] = kindRow{v2: true, parse: parseBatchRecord}
}

// allowedKinds reports whether typ is a legal type tag for version.
// Version 1 accepts Put and Delete. Version 2 also accepts Batch.
func allowedKinds(version, typ uint8) bool {
	row := kindTable[typ]
	switch version {
	case versionV1:
		return row.v1
	case versionV2:
		return row.v2
	default:
		return false
	}
}

// Encode writes a version-2 stream: a version byte, then one frame per record
// in order. Only Put, Delete, and Batch values are accepted. A nil element or
// any other concrete type, including *Put, *Delete, and *Batch, returns an
// error that does not wrap ErrCorrupt. A record whose type+payload would
// exceed 16 MiB, including a Batch, returns an error that does not wrap
// ErrCorrupt, before the output buffer is allocated. A stream whose total
// size does not fit in int returns a distinct error that also does not wrap
// ErrCorrupt. A nest deeper than 8 returns an error that wraps ErrCorrupt,
// also before that allocation. Encode(nil) and Encode of an empty slice both
// return a one-byte stream whose value is 2.
func Encode(recs []Record) ([]byte, error) {
	n, err := streamSize(recs)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1, n)
	buf[0] = versionV2
	for _, r := range recs {
		buf, err = appendRecord(buf, r, 0)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// streamSize is the size pass Encode calls. It returns the total byte count
// of the stream, including the version byte. Unknown types, oversize frames,
// and depth greater than 8 are rejected here, before the output buffer is
// allocated.
func streamSize(recs []Record) (int, error) {
	n := 1
	for _, r := range recs {
		frameLen, err := frameSize(r, 0)
		if err != nil {
			return 0, err
		}
		next, err := addSize(n, 4+frameLen+4)
		if err != nil {
			return 0, err
		}
		n = next
	}
	return n, nil
}

// frameSize reports the type+payload length of r at the given stream depth.
// A nil interface does not panic: the type switch selects the nil case.
// Only a method call on a nil interface would panic, and this function
// never makes one.
//
// For a Batch, total is type+payload. It starts at 5 (type byte plus the
// uint32 count). Each child adds that child's full frame: 4-byte length word,
// child type+payload, and 4-byte CRC. The cap check is total > maxFrameLen.
// A total equal to the cap is legal. The Batch's own length word and CRC are
// outside total; the caller adds them. A partial sum that exceeds the cap, or
// a depth greater than 8, returns immediately and does not visit the remaining
// children.
func frameSize(r Record, depth int) (int, error) {
	switch x := r.(type) {
	case Put:
		return putFrameLen(x.Key, x.Value)
	case Delete:
		return deleteFrameLen(x.Key)
	case Batch:
		if depth+1 > maxDepth {
			return 0, errTooDeep
		}
		total := 5
		for _, child := range x.Records {
			childLen, err := frameSize(child, depth+1)
			if err != nil {
				return 0, err
			}
			next, err := addSize(total, 4+childLen+4)
			if err != nil || next > maxFrameLen {
				return 0, errFrameTooLarge
			}
			total = next
		}
		return total, nil
	case nil:
		return 0, fmt.Errorf("%w: nil", errUnknownRecord)
	default:
		return 0, fmt.Errorf("%w: %T", errUnknownRecord, r)
	}
}

// putFrameLen is 1 (type) + 4 (klen) + len(key) + 4 (vlen) + len(value).
// The 16 MiB frame cap is the only size bound: it is far below MaxUint32, so
// a length that passes it always fits in a uint32 prefix. Comparing against
// the cap in int also compiles on 32-bit ports.
func putFrameLen(key string, value []byte) (int, error) {
	k := len(key)
	v := len(value)
	if k > maxFrameLen-9 || v > maxFrameLen-9 || k+v > maxFrameLen-9 {
		return 0, errFrameTooLarge
	}
	return 9 + k + v, nil
}

// deleteFrameLen is 1 (type) + 4 (klen) + len(key).
func deleteFrameLen(key string) (int, error) {
	k := len(key)
	if k > maxFrameLen-5 {
		return 0, errFrameTooLarge
	}
	return 5 + k, nil
}

// addSize rejects a sum that does not fit in int. A negative input is treated
// as that same overflow: it cannot occur for a legal frame, and reporting it
// as errStreamTooLarge keeps a wrapped total distinct from a single frame
// that exceeds 16 MiB. Callers that are sizing one frame map it back to
// errFrameTooLarge.
func addSize(n, delta int) (int, error) {
	if delta < 0 || n < 0 {
		return 0, errStreamTooLarge
	}
	sum := n + delta
	if sum < n {
		return 0, errStreamTooLarge
	}
	return sum, nil
}

// appendRecord writes one frame. The same switch serves the top-level loop
// and Batch children, so a new kind cannot be accepted at one level and
// skipped at the other.
func appendRecord(buf []byte, r Record, depth int) ([]byte, error) {
	switch x := r.(type) {
	case Put:
		return appendPut(buf, x)
	case Delete:
		return appendDelete(buf, x)
	case Batch:
		return appendBatch(buf, x, depth)
	case nil:
		return nil, fmt.Errorf("%w: nil", errUnknownRecord)
	default:
		return nil, fmt.Errorf("%w: %T", errUnknownRecord, r)
	}
}

func appendPut(buf []byte, p Put) ([]byte, error) {
	frameLen, err := putFrameLen(p.Key, p.Value)
	if err != nil {
		return nil, err
	}
	start := len(buf)
	buf = appendU32(buf, uint32(frameLen))
	buf = append(buf, kindPut)
	buf = appendU32(buf, uint32(len(p.Key)))
	buf = append(buf, p.Key...)
	buf = appendU32(buf, uint32(len(p.Value)))
	buf = append(buf, p.Value...)
	return appendU32(buf, crc32.ChecksumIEEE(buf[start+4:])), nil
}

func appendDelete(buf []byte, d Delete) ([]byte, error) {
	frameLen, err := deleteFrameLen(d.Key)
	if err != nil {
		return nil, err
	}
	start := len(buf)
	buf = appendU32(buf, uint32(frameLen))
	buf = append(buf, kindDelete)
	buf = appendU32(buf, uint32(len(d.Key)))
	buf = append(buf, d.Key...)
	return appendU32(buf, crc32.ChecksumIEEE(buf[start+4:])), nil
}

// appendBatch writes a Batch frame. The length word is reserved as zeros and
// backfilled from the bytes actually written, so the size pass and the write
// pass cannot drift by a recomputed formula. frameLen > maxFrameLen is a drift
// guard; the size pass is the validator. A child the switch does not recognize
// returns errUnknownRecord. Skipping it would emit a count that disagrees with
// the frames that follow.
func appendBatch(buf []byte, b Batch, depth int) ([]byte, error) {
	if depth+1 > maxDepth {
		return nil, errTooDeep
	}
	start := len(buf)
	buf = appendU32(buf, 0)
	payloadStart := len(buf)
	buf = append(buf, kindBatch)
	buf = appendU32(buf, uint32(len(b.Records)))
	for _, child := range b.Records {
		var err error
		buf, err = appendRecord(buf, child, depth+1)
		if err != nil {
			return nil, err
		}
	}
	frameLen := len(buf) - payloadStart
	if frameLen > maxFrameLen {
		return nil, errFrameTooLarge
	}
	binary.BigEndian.PutUint32(buf[start:start+4], uint32(frameLen))
	return appendU32(buf, crc32.ChecksumIEEE(buf[payloadStart:])), nil
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// decoder carries the version and the current sub-stream depth. The top-level
// stream is depth 0. A Batch sub-stream is one deeper than the Batch frame.
type decoder struct {
	version uint8
	depth   int
}

// Decode parses a version-1 or version-2 stream. Version 1 accepts Put and
// Delete only. Version 2 also accepts Batch, nested up to depth 8. Any other
// version, including 0 and 3, is ErrCorrupt, checked before any frame. Every
// wire failure is the bare ErrCorrupt sentinel. On error the record slice is
// nil. A version-only stream returns a non-nil empty slice.
//
// A byte inside a Batch at nesting depth d is covered by d+1 CRC checks: its
// own frame, plus each enclosing Batch frame. Skipping an outer CRC would
// accept a corrupt enclosing frame.
func Decode(b []byte) ([]Record, error) {
	if len(b) < 1 {
		return nil, ErrCorrupt
	}
	version := b[0]
	if version != versionV1 && version != versionV2 {
		return nil, ErrCorrupt
	}
	return decoder{version: version, depth: 0}.decodeUntilEnd(b[1:])
}

// nextFrame reads one frame starting at i, inside span only. CRC is verified
// before the type byte is interpreted. An oversize or zero length is rejected
// before the body is sliced. An inner length that extends past span is
// ErrCorrupt; sibling and outer bytes are outside span.
func (d decoder) nextFrame(span []byte, i int) (Record, int, error) {
	if len(span)-i < 4 {
		return nil, 0, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(span[i : i+4])
	if n > maxFrameLen || n == 0 {
		return nil, 0, ErrCorrupt
	}
	i += 4
	need := int(n) + 4
	if len(span)-i < need {
		return nil, 0, ErrCorrupt
	}
	body := span[i : i+int(n)]
	gotCRC := binary.BigEndian.Uint32(span[i+int(n) : i+need])
	if crc32.ChecksumIEEE(body) != gotCRC {
		return nil, 0, ErrCorrupt
	}
	rec, err := d.decodeFrame(body)
	if err != nil {
		return nil, 0, err
	}
	return rec, i + need, nil
}

// decodeUntilEnd reads every frame in span. A sub-stream at depth greater
// than maxDepth is ErrCorrupt. An empty span returns a non-nil empty slice.
func (d decoder) decodeUntilEnd(span []byte) ([]Record, error) {
	if d.depth > maxDepth {
		return nil, ErrCorrupt
	}
	var out []Record
	i := 0
	for i < len(span) {
		rec, next, err := d.nextFrame(span, i)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		i = next
	}
	if out == nil {
		out = []Record{}
	}
	return out, nil
}

// decodeExactly reads n frames and returns the cursor after the last one.
// It does not keep parsing after n frames. n is never converted to int.
// The caller has already rejected an n the span cannot hold, so the capacity
// is bounded by len(span)/minInnerFrame and is not an attacker-chosen size.
// The depth check runs before the n == 0 success path, so an empty Batch
// whose sub-stream sits at depth 9 is ErrCorrupt.
func (d decoder) decodeExactly(span []byte, n uint32) ([]Record, int, error) {
	if d.depth > maxDepth {
		return nil, 0, ErrCorrupt
	}
	// Capacity is taken only after the first frame passes the length cap and
	// the CRC check. An oversize or truncated first frame then allocates
	// nothing. n is already bounded by len(span)/minInnerFrame.
	var out []Record
	i := 0
	for parsed := uint32(0); parsed < n; parsed++ {
		rec, next, err := d.nextFrame(span, i)
		if err != nil {
			return nil, 0, err
		}
		if out == nil {
			out = make([]Record, 0, n)
		}
		out = append(out, rec)
		i = next
	}
	return out, i, nil
}

// decodeFrame parses a CRC-verified type+payload span. On error it returns a
// nil Record interface, not a boxed zero Put or Delete. Kind dispatch comes
// from kindTable, the same table allowedKinds reads.
func (d decoder) decodeFrame(body []byte) (Record, error) {
	if len(body) < 1 {
		return nil, ErrCorrupt
	}
	typ := body[0]
	row := kindTable[typ]
	if row.parse == nil || !allowedKinds(d.version, typ) {
		return nil, ErrCorrupt
	}
	return row.parse(d, body[1:])
}

func parsePutRecord(_ decoder, payload []byte) (Record, error) {
	p, err := parsePut(payload)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func parseDeleteRecord(_ decoder, payload []byte) (Record, error) {
	del, err := parseDelete(payload)
	if err != nil {
		return nil, err
	}
	return del, nil
}

func parseBatchRecord(d decoder, payload []byte) (Record, error) {
	return d.parseBatch(payload)
}

// parseBatch reads n, then exactly n inner frames, from payload. A hostile n
// that the remainder cannot hold returns ErrCorrupt before any allocation
// sized from n. After that check, decodeExactly may use n as a slice
// capacity: it is at most len(rest)/minInnerFrame. The inner decoder sees
// only payload[4:], so an inner length cannot read into a sibling or outer
// frame.
func (d decoder) parseBatch(payload []byte) (Record, error) {
	if len(payload) < 4 {
		return nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(payload[:4])
	rest := payload[4:]
	if uint64(n)*minInnerFrame > uint64(len(rest)) {
		return nil, ErrCorrupt
	}
	inner := decoder{version: d.version, depth: d.depth + 1}
	recs, end, err := inner.decodeExactly(rest, n)
	if err != nil {
		return nil, err
	}
	if end != len(rest) {
		return nil, ErrCorrupt
	}
	// decodeExactly returns exactly n records or an error, so a second
	// len(recs) != n check would be unreachable. n == 0 leaves recs nil
	// because the loop never appends; return a non-nil empty slice.
	if n == 0 {
		recs = []Record{}
	}
	return Batch{Records: recs}, nil
}

func parsePut(payload []byte) (Put, error) {
	if len(payload) < 8 {
		return Put{}, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[0:4])
	// klen must leave the 4-byte vlen prefix. uint64 keeps a hostile klen
	// from wrapping the slice bound.
	if uint64(klen) > uint64(len(payload)-8) {
		return Put{}, ErrCorrupt
	}
	keyEnd := 4 + int(klen)
	vlen := binary.BigEndian.Uint32(payload[keyEnd : keyEnd+4])
	valOff := keyEnd + 4
	if uint64(vlen) != uint64(len(payload)-valOff) {
		return Put{}, ErrCorrupt
	}
	value := []byte{}
	if vlen != 0 {
		value = append([]byte(nil), payload[valOff:]...)
	}
	return Put{Key: string(payload[4:keyEnd]), Value: value}, nil
}

func parseDelete(payload []byte) (Delete, error) {
	if len(payload) < 4 {
		return Delete{}, ErrCorrupt
	}
	klen := binary.BigEndian.Uint32(payload[0:4])
	if uint64(klen) != uint64(len(payload)-4) {
		return Delete{}, ErrCorrupt
	}
	return Delete{Key: string(payload[4:])}, nil
}
