package walcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func beU32x(x uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, x)
	return out
}

func mkFrameX(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(beU32x(uint32(len(tp))), tp...)
	return append(out, beU32x(crc32.ChecksumIEEE(tp))...)
}

func putPLX(key string, val []byte) []byte {
	out := append(beU32x(uint32(len(key))), key...)
	out = append(out, beU32x(uint32(len(val)))...)
	return append(out, val...)
}

func delPLX(key string) []byte {
	return append(beU32x(uint32(len(key))), key...)
}

func batchPLX(count uint32, frames ...[]byte) []byte {
	out := beU32x(count)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func recEqX(a, b Record) bool {
	switch x := a.(type) {
	case Put:
		y, ok := b.(Put)
		return ok && x.Key == y.Key && bytes.Equal(x.Value, y.Value)
	case Delete:
		y, ok := b.(Delete)
		return ok && x.Key == y.Key
	case Batch:
		y, ok := b.(Batch)
		return ok && recsEqX(x.Records, y.Records)
	default:
		return false
	}
}

func recsEqX(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !recEqX(a[i], b[i]) {
			return false
		}
	}
	return true
}

func wrapBatches(inner []byte, n int) []byte {
	cur := inner
	for i := 0; i < n; i++ {
		cur = mkFrameX(kindBatch, batchPLX(1, cur))
	}
	return append([]byte{versionV2}, cur...)
}

func chainEmpty(n int) Record {
	var rec Record = Batch{Records: []Record{}}
	for i := 1; i < n; i++ {
		rec = Batch{Records: []Record{rec}}
	}
	return rec
}

func TestDepth8RoundTrip(t *testing.T) {
	var rec Record = Put{Key: "deep", Value: []byte("x")}
	for i := 0; i < 8; i++ {
		rec = Batch{Records: []Record{rec}}
	}
	b, err := Encode([]Record{rec})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEqX(got, []Record{rec}) {
		t.Fatalf("depth 8 encode/decode mismatch: %#v", got)
	}

	hand := wrapBatches(mkFrameX(kindPut, putPLX("deep", []byte("x"))), 8)
	got, err = Decode(hand)
	if err != nil {
		t.Fatalf("hand-built depth 8: %v", err)
	}
	if !recsEqX(got, []Record{rec}) {
		t.Fatalf("hand-built depth 8 mismatch: %#v", got)
	}
}

func TestDepth9EncodeTooDeep(t *testing.T) {
	var rec Record = Put{Key: "deep", Value: []byte("x")}
	for i := 0; i < 9; i++ {
		rec = Batch{Records: []Record{rec}}
	}
	b, err := Encode([]Record{rec})
	if b != nil {
		t.Fatalf("Encode returned %d bytes, want nil", len(b))
	}
	if !errors.Is(err, errTooDeep) {
		t.Fatalf("err = %v, want errTooDeep", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("a nest deeper than 8 must satisfy errors.Is(err, ErrCorrupt)")
	}
}

func TestEmptyBatchDepthBoundary(t *testing.T) {
	empty := mkFrameX(kindBatch, batchPLX(0))
	// Empty frame wrapped 7 times: the empty Batch is encountered at depth 7
	// and its sub-stream sits at depth 8.
	cur := empty
	for i := 0; i < 7; i++ {
		cur = mkFrameX(kindBatch, batchPLX(1, cur))
	}
	got, err := Decode(append([]byte{versionV2}, cur...))
	if err != nil {
		t.Fatalf("7 wraps of empty Batch: %v", err)
	}
	want := chainEmpty(8)
	if !recEqX(got[0], want) {
		t.Fatalf("got %#v want %#v", got[0], want)
	}

	cur = empty
	for i := 0; i < 8; i++ {
		cur = mkFrameX(kindBatch, batchPLX(1, cur))
	}
	if _, err := Decode(append([]byte{versionV2}, cur...)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("8 wraps of empty Batch: err = %v, want ErrCorrupt", err)
	}

	eight := chainEmpty(8)
	b, err := Encode([]Record{eight})
	if err != nil {
		t.Fatalf("Encode 8 empty: %v", err)
	}
	got, err = Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recEqX(got[0], eight) {
		t.Fatalf("8 empty round-trip: %#v", got[0])
	}

	nine := chainEmpty(9)
	b, err = Encode([]Record{nine})
	if b != nil || !errors.Is(err, errTooDeep) {
		t.Fatalf("Encode 9 empty: b=%v err=%v", b, err)
	}
}

func TestVersionZeroAndBatchOnV1(t *testing.T) {
	put := mkFrameX(kindPut, putPLX("k", []byte("v")))
	if _, err := Decode(append([]byte{0}, put...)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("version 0: err = %v, want ErrCorrupt", err)
	}
	got, err := Decode(append([]byte{versionV2}, put...))
	if err != nil {
		t.Fatal(err)
	}
	if !recEqX(got[0], Put{Key: "k", Value: []byte("v")}) {
		t.Fatalf("version 2 put: %#v", got)
	}
	batch := mkFrameX(kindBatch, batchPLX(0))
	if _, err := Decode(append([]byte{versionV1}, batch...)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("version 1 batch: err = %v, want ErrCorrupt", err)
	}
}

func TestEmptyBatchRoundTrip(t *testing.T) {
	nilRecs := Batch{}
	emptyRecs := Batch{Records: []Record{}}
	bNil, err := Encode([]Record{nilRecs})
	if err != nil {
		t.Fatal(err)
	}
	bEmpty, err := Encode([]Record{emptyRecs})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bNil, bEmpty) {
		t.Fatalf("nil and empty Batch encode differently:\n%v\n%v", bNil, bEmpty)
	}
	got, err := Decode(bNil)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := got[0].(Batch)
	if !ok || b.Records == nil || len(b.Records) != 0 {
		t.Fatalf("decoded %#v, want non-nil empty Records", got[0])
	}
}

func TestBatchCountMismatch(t *testing.T) {
	inner := mkFrameX(kindDelete, delPLX("a"))
	extra := mkFrameX(kindDelete, delPLX("b"))

	// n = 1 with two well-formed frames.
	two := append([]byte{versionV2}, mkFrameX(kindBatch, batchPLX(1, inner, extra))...)
	if recs, err := Decode(two); !errors.Is(err, ErrCorrupt) || recs != nil {
		t.Fatalf("n=1 two frames: recs=%#v err=%v", recs, err)
	}
	// n = 2 with one inner frame.
	one := append([]byte{versionV2}, mkFrameX(kindBatch, batchPLX(2, inner))...)
	if recs, err := Decode(one); !errors.Is(err, ErrCorrupt) || recs != nil {
		t.Fatalf("n=2 one frame: recs=%#v err=%v", recs, err)
	}
	// n = 0 with trailing bytes.
	trail := append(beU32x(0), inner...)
	zero := append([]byte{versionV2}, mkFrameX(kindBatch, trail)...)
	if recs, err := Decode(zero); !errors.Is(err, ErrCorrupt) || recs != nil {
		t.Fatalf("n=0 trailing: recs=%#v err=%v", recs, err)
	}
}

func TestShortBatchPayload(t *testing.T) {
	payloads := [][]byte{
		nil,
		{0x00},
		{0x00, 0x00},
		{0x00, 0x00, 0x00},
	}
	for _, p := range payloads {
		stream := append([]byte{versionV2}, mkFrameX(kindBatch, p)...)
		recs, err := Decode(stream)
		if !errors.Is(err, ErrCorrupt) || recs != nil {
			t.Fatalf("payload len %d: recs=%#v err=%v", len(p), recs, err)
		}
	}
}

func TestHugeNNoAlloc(t *testing.T) {
	// 4-byte count of 0xFFFFFFFF plus 8 padding bytes: long enough for a
	// valid CRC, far shorter than n*13.
	payload := append(beU32x(0xFFFFFFFF), make([]byte, 8)...)
	stream := append([]byte{versionV2}, mkFrameX(kindBatch, payload)...)
	allocs := testing.AllocsPerRun(100, func() {
		recs, err := Decode(stream)
		if !errors.Is(err, ErrCorrupt) || recs != nil {
			t.Fatalf("recs=%#v err=%v", recs, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("AllocsPerRun = %v, want 0", allocs)
	}
}

func TestInnerLenPastEnclosingPayload(t *testing.T) {
	// A legal empty-Delete frame is 13 bytes: len=5, type, klen=0, crc.
	// The Batch payload keeps only the first 9 of those bytes and pads rest
	// to 13 so the n*13 pre-check passes. The missing CRC is the first 4
	// bytes of the following top-level frame. A decoder that reads past the
	// Batch span would accept the inner Delete; a span-bounded decoder sees
	// a truncated inner frame and returns ErrCorrupt.
	fullInner := mkFrameX(kindDelete, delPLX(""))
	if len(fullInner) != 13 {
		t.Fatalf("empty Delete frame len %d, want 13", len(fullInner))
	}
	rest := append(append([]byte{}, fullInner[:9]...), 0, 0, 0, 0)
	if len(rest) != minInnerFrame {
		t.Fatalf("rest len %d", len(rest))
	}
	// If the inner length were allowed to continue into the next top-level
	// frame, those four CRC bytes would complete a legal empty Delete.
	if crc32.ChecksumIEEE(fullInner[4:9]) != binary.BigEndian.Uint32(fullInner[9:]) {
		t.Fatal("fixture CRC does not match the borrowed tail")
	}
	payload := batchPLX(1, rest)
	batchFrame := mkFrameX(kindBatch, payload)
	// The next frame starts with the CRC the inner frame expects, then a
	// legal Put so a cheating read has somewhere to land.
	next := mkFrameX(kindPut, putPLX("after", []byte("ok")))
	borrowed := append(append([]byte{}, fullInner[9:]...), next...)
	stream := append([]byte{versionV2}, batchFrame...)
	stream = append(stream, borrowed...)
	recs, err := Decode(stream)
	if !errors.Is(err, ErrCorrupt) || recs != nil {
		t.Fatalf("past span: recs=%#v err=%v", recs, err)
	}

	// Sibling: a legal inner Delete plus a following top-level Put. The outer
	// cursor must resume after the Batch frame and return both records.
	legalPayload := batchPLX(1, mkFrameX(kindDelete, delPLX("in")))
	legal := append([]byte{versionV2}, mkFrameX(kindBatch, legalPayload)...)
	legal = append(legal, next...)
	got, err := Decode(legal)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		Batch{Records: []Record{Delete{Key: "in"}}},
		Put{Key: "after", Value: []byte("ok")},
	}
	if !recsEqX(got, want) {
		t.Fatalf("sibling resume: %#v", got)
	}
}

func TestInnerOversizeLenNoAlloc(t *testing.T) {
	// rest must be >= 13 so the pre-check does not reject before the cap.
	inner := beU32x(uint32(maxFrameLen + 1))
	inner = append(inner, make([]byte, 9)...)
	if len(inner) < minInnerFrame {
		t.Fatal("rest shorter than minInnerFrame")
	}
	payload := batchPLX(1, inner)
	stream := append([]byte{versionV2}, mkFrameX(kindBatch, payload)...)
	allocs := testing.AllocsPerRun(50, func() {
		recs, err := Decode(stream)
		if !errors.Is(err, ErrCorrupt) || recs != nil {
			t.Fatalf("recs=%#v err=%v", recs, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("AllocsPerRun = %v, want 0", allocs)
	}
}

func TestNestedCRCFlipRewrapped(t *testing.T) {
	// Corrupt the inner CRC, then re-wrap so the outer CRC is valid over the
	// corrupted payload. An implementation that skips inner CRCs would accept
	// this stream.
	for _, depth := range []int{1, 8} {
		inner := mkFrameX(kindPut, putPLX("k", []byte("v")))
		inner[len(inner)-1] ^= 0xff
		cur := inner
		for i := 0; i < depth; i++ {
			cur = mkFrameX(kindBatch, batchPLX(1, cur))
		}
		stream := append([]byte{versionV2}, cur...)
		recs, err := Decode(stream)
		if !errors.Is(err, ErrCorrupt) || recs != nil {
			t.Fatalf("depth %d crc flip: recs=%#v err=%v", depth, recs, err)
		}
	}
}

func TestPutDeleteRulesInsideBatch(t *testing.T) {
	// Hostile klen that would walk off the payload.
	badPut := putPLX("ab", []byte("x"))
	binary.BigEndian.PutUint32(badPut[0:4], 0xFFFFFFFF)
	stream := append([]byte{versionV2}, mkFrameX(kindBatch, batchPLX(1, mkFrameX(kindPut, badPut)))...)
	if recs, err := Decode(stream); !errors.Is(err, ErrCorrupt) || recs != nil {
		t.Fatalf("hostile klen: recs=%#v err=%v", recs, err)
	}

	// Trailing bytes inside a Delete payload.
	trail := append(delPLX("k"), 0x01)
	stream = append([]byte{versionV2}, mkFrameX(kindBatch, batchPLX(1, mkFrameX(kindDelete, trail)))...)
	if recs, err := Decode(stream); !errors.Is(err, ErrCorrupt) || recs != nil {
		t.Fatalf("trailing delete: recs=%#v err=%v", recs, err)
	}

	// Empty key and a binary key round-trip inside a Batch.
	recs := []Record{Batch{Records: []Record{
		Put{Key: "", Value: []byte{0x00, 0xff}},
		Delete{Key: "a\x00b"},
	}}}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEqX(got, recs) {
		t.Fatalf("got %#v", got)
	}
}

type customRec struct{}

func (customRec) Kind() uint8 { return 9 }

func TestEncodeRejectsPointersAndCustom(t *testing.T) {
	cases := []Record{
		(*Batch)(nil),
		&Batch{},
		(*Put)(nil),
		customRec{},
	}
	for _, r := range cases {
		_, err := Encode([]Record{r})
		if !errors.Is(err, errUnknownRecord) || errors.Is(err, ErrCorrupt) {
			t.Fatalf("%T: err = %v", r, err)
		}
	}
	_, err := Encode([]Record{Batch{Records: []Record{nil}}})
	if !errors.Is(err, errUnknownRecord) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("nil child: %v", err)
	}
	_, err = Encode([]Record{Batch{Records: []Record{customRec{}}}})
	if !errors.Is(err, errUnknownRecord) {
		t.Fatalf("custom child: %v", err)
	}
}

func TestCanonicalReencode(t *testing.T) {
	recs := []Record{
		Put{Key: "p", Value: []byte("v")},
		Delete{Key: "d"},
		Batch{Records: []Record{
			Put{Key: "bp", Value: []byte("bv")},
			Batch{Records: []Record{}},
		}},
		Put{Key: "last", Value: nil},
	}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, again) {
		t.Fatal("re-encode is not byte-identical")
	}

	fixture := []byte{
		0x01, 0x00, 0x00, 0x00, 0x11, 0x01, 0x00, 0x00, 0x00, 0x05, 0x61, 0x6c,
		0x70, 0x68, 0x61, 0x00, 0x00, 0x00, 0x03, 0x6f, 0x6e, 0x65, 0x20, 0x74,
		0x5b, 0x79, 0x00, 0x00, 0x00, 0x09, 0x02, 0x00, 0x00, 0x00, 0x04, 0x62,
		0x65, 0x74, 0x61, 0x93, 0xaa, 0x41, 0x97,
	}
	decoded, err := Decode(fixture)
	if err != nil {
		t.Fatal(err)
	}
	re, err := Encode(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if re[0] != versionV2 {
		t.Fatalf("re-encoded version %d", re[0])
	}
	if !bytes.Equal(re[1:], fixture[1:]) {
		t.Fatal("v1 fixture frames changed on re-encode")
	}
}

func TestStreamSizeMatchesEncode(t *testing.T) {
	cases := [][]Record{
		{Batch{}},
		{Batch{Records: []Record{Batch{Records: []Record{Put{Key: "a", Value: []byte("b")}}}}}},
		{
			Put{Key: "p", Value: []byte("v")},
			Delete{Key: "d"},
			Batch{Records: []Record{Delete{Key: "x"}}},
		},
	}
	for _, recs := range cases {
		n, err := streamSize(recs)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Encode(recs)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != n || cap(b) != n {
			t.Fatalf("len=%d cap=%d streamSize=%d", len(b), cap(b), n)
		}
	}
}

func deleteKey(n int) string {
	return string(bytes.Repeat([]byte{'k'}, n))
}

func TestBatchCapBoundary(t *testing.T) {
	legalKey := deleteKey(maxFrameLen - 18)
	illegalKey := deleteKey(maxFrameLen - 17)

	legal := Batch{Records: []Record{Delete{Key: legalKey}}}
	b, err := Encode([]Record{legal})
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != versionV2 {
		t.Fatalf("version %d", b[0])
	}
	if got := binary.BigEndian.Uint32(b[1:5]); got != maxFrameLen {
		t.Fatalf("length word %d, want %d", got, maxFrameLen)
	}
	if len(b) != maxFrameLen+9 || cap(b) != len(b) {
		t.Fatalf("len=%d cap=%d", len(b), cap(b))
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEqX(got, []Record{legal}) {
		t.Fatal("legal cap round-trip mismatch")
	}

	illegal := Batch{Records: []Record{Delete{Key: illegalKey}}}
	illegalRecs := []Record{illegal}
	allocs := testing.AllocsPerRun(5, func() {
		buf, encErr := Encode(illegalRecs)
		if buf != nil || !errors.Is(encErr, errFrameTooLarge) || errors.Is(encErr, ErrCorrupt) {
			t.Fatalf("buf=%v err=%v", buf, encErr)
		}
	})
	if allocs != 0 {
		t.Fatalf("illegal Encode AllocsPerRun = %v, want 0", allocs)
	}
}

func TestNestedBatchCapBoundary(t *testing.T) {
	legalInner := Batch{Records: []Record{Delete{Key: deleteKey(maxFrameLen - 18)}}}
	illegalInner := Batch{Records: []Record{Delete{Key: deleteKey(maxFrameLen - 17)}}}

	n, err := frameSize(legalInner, 1)
	if err != nil || n != maxFrameLen {
		t.Fatalf("frameSize legal at depth 1: n=%d err=%v", n, err)
	}
	if _, err := frameSize(illegalInner, 1); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("frameSize illegal at depth 1: %v", err)
	}

	for _, inner := range []Batch{legalInner, illegalInner} {
		outerRecs := []Record{Batch{Records: []Record{inner}}}
		allocs := testing.AllocsPerRun(3, func() {
			buf, encErr := Encode(outerRecs)
			if buf != nil || !errors.Is(encErr, errFrameTooLarge) {
				t.Fatalf("buf=%v err=%v", buf, encErr)
			}
		})
		if allocs != 0 {
			t.Fatalf("nested cap AllocsPerRun = %v, want 0", allocs)
		}
	}
}

func TestKindTableAgrees(t *testing.T) {
	for version := uint8(0); version < 4; version++ {
		for typ := 0; typ < 256; typ++ {
			row := kindTable[typ]
			allowed := allowedKinds(version, uint8(typ))
			hasParser := row.parse != nil
			switch version {
			case versionV1:
				if allowed != row.v1 || (allowed && !hasParser) {
					t.Fatalf("v1 kind %d: allowed=%v v1=%v parser=%v", typ, allowed, row.v1, hasParser)
				}
			case versionV2:
				if allowed != row.v2 || (allowed && !hasParser) {
					t.Fatalf("v2 kind %d: allowed=%v v2=%v parser=%v", typ, allowed, row.v2, hasParser)
				}
			default:
				if allowed {
					t.Fatalf("version %d kind %d allowed", version, typ)
				}
			}
			if hasParser && !row.v1 && !row.v2 {
				t.Fatalf("kind %d has a parser and no version", typ)
			}
		}
	}
}

func TestStreamTooLarge(t *testing.T) {
	// A wrapped sum is what streamSize sees when legal in-cap frames do not
	// fit in int. On 64-bit hosts that sum is not reachable without a huge
	// allocation, so the test drives addSize, the function streamSize calls,
	// and checks the error streamSize returns unchanged.
	if _, err := addSize(int(^uint(0)>>1), 1); !errors.Is(err, errStreamTooLarge) {
		t.Fatalf("wrapped sum: %v", err)
	}
	if errors.Is(errStreamTooLarge, errFrameTooLarge) || errors.Is(errStreamTooLarge, ErrCorrupt) {
		t.Fatal("stream overflow must not be a frame or wire error")
	}
}

func TestCyclicBatchTooDeep(t *testing.T) {
	recs := make([]Record, 1)
	b := Batch{Records: recs}
	recs[0] = b
	_, err := Encode([]Record{b})
	if !errors.Is(err, errTooDeep) || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want errTooDeep wrapping ErrCorrupt", err)
	}
}

func TestPutErrorPathNilInterface(t *testing.T) {
	// Hostile klen on a top-level Put. decodeFrame must return a nil
	// interface, so this call allocates nothing.
	payload := putPLX("ab", []byte("x"))
	binary.BigEndian.PutUint32(payload[0:4], 0x00FFFFFF)
	stream := append([]byte{versionV2}, mkFrameX(kindPut, payload)...)
	allocs := testing.AllocsPerRun(50, func() {
		recs, err := Decode(stream)
		if !errors.Is(err, ErrCorrupt) || recs != nil {
			t.Fatalf("recs=%#v err=%v", recs, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("AllocsPerRun = %v, want 0", allocs)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte{versionV2})
	f.Add([]byte(nil))
	seed, err := Encode([]Record{
		Put{Key: "alpha", Value: []byte("one")},
		Delete{Key: "beta"},
		Batch{Records: []Record{
			Put{Key: "inner", Value: []byte("x")},
			Batch{Records: []Record{}},
		}},
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{
		0x01, 0x00, 0x00, 0x00, 0x11, 0x01, 0x00, 0x00, 0x00, 0x05, 0x61, 0x6c,
		0x70, 0x68, 0x61, 0x00, 0x00, 0x00, 0x03, 0x6f, 0x6e, 0x65, 0x20, 0x74,
		0x5b, 0x79, 0x00, 0x00, 0x00, 0x09, 0x02, 0x00, 0x00, 0x00, 0x04, 0x62,
		0x65, 0x74, 0x61, 0x93, 0xaa, 0x41, 0x97,
	})
	f.Add(wrapBatches(mkFrameX(kindPut, putPLX("deep", []byte("x"))), 8))
	f.Add(wrapBatches(mkFrameX(kindPut, putPLX("deep", []byte("x"))), 9))

	f.Fuzz(func(t *testing.T, data []byte) {
		recs, err := Decode(data)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) || recs != nil {
				t.Fatalf("err=%v recs=%#v", err, recs)
			}
			return
		}
		if len(data) == 0 || (data[0] != versionV1 && data[0] != versionV2) {
			t.Fatal("accepted a stream with a bad version")
		}
		if data[0] != versionV2 {
			return
		}
		again, err := Encode(recs)
		if err != nil {
			t.Fatalf("accepted stream did not re-encode: %v", err)
		}
		if !bytes.Equal(data, again) {
			t.Fatalf("version-2 stream is not canonical:\n got %x\nwant %x", again, data)
		}
	})
}

// frameStructured builds a CRC-valid frame from fuzz bytes so Decode reaches
// payload parsing. kind selects Put, Delete, or Batch. Nested Batch payloads
// are themselves framed the same way, up to depth 8.
func frameStructured(kind byte, payload []byte, depth int) []byte {
	switch kind % 3 {
	case 0:
		return mkFrameX(kindPut, payload)
	case 1:
		return mkFrameX(kindDelete, payload)
	default:
		if depth >= maxDepth || len(payload) == 0 {
			return mkFrameX(kindBatch, batchPLX(0))
		}
		n := int(payload[0]%4) + 1
		rest := payload[1:]
		chunk := len(rest) / n
		if chunk == 0 {
			chunk = 1
		}
		frames := make([][]byte, 0, n)
		for i := 0; i < n && len(rest) > 0; i++ {
			take := chunk
			if take > len(rest) || i == n-1 {
				take = len(rest)
			}
			childKind := rest[0]
			frames = append(frames, frameStructured(childKind, rest[1:take], depth+1))
			rest = rest[take:]
		}
		return mkFrameX(kindBatch, batchPLX(uint32(len(frames)), frames...))
	}
}

func FuzzDecodeStructured(f *testing.F) {
	f.Add(byte(kindPut), []byte("k\x00v"))
	f.Add(byte(kindDelete), []byte("beta"))
	f.Add(byte(kindBatch), []byte{1, kindPut, 'a', 'b'})
	f.Add(byte(kindBatch), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(byte(kindBatch), []byte{})
	f.Fuzz(func(t *testing.T, kind byte, payload []byte) {
		if len(payload) > 4096 {
			payload = payload[:4096]
		}
		frame := frameStructured(kind, payload, 0)
		stream := append([]byte{versionV2}, frame...)
		recs, err := Decode(stream)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) || recs != nil {
				t.Fatalf("err=%v recs=%#v", err, recs)
			}
			return
		}
		again, err := Encode(recs)
		if err != nil {
			t.Fatalf("accepted stream did not re-encode: %v", err)
		}
		if !bytes.Equal(stream, again) {
			t.Fatalf("structured stream is not canonical")
		}
	})
}
