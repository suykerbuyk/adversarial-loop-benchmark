package walcodec

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Helpers reuse the public v2 file's be2/mkFrame2/putPL2/batchPL2/recsEq2.

func delPL(key string) []byte { return append(be2(uint32(len(key))), key...) }

func nestPut(levels int) []byte {
	cur := mkFrame2(KindPut, putPL2("deep", []byte("x")))
	for i := 0; i < levels; i++ {
		cur = mkFrame2(kindBatch, batchPL2(1, cur))
	}
	return cur
}

func nestRec(levels int) Record {
	var r Record = Put{Key: "deep", Value: []byte("x")}
	for i := 0; i < levels; i++ {
		r = Batch{Records: []Record{r}}
	}
	return r
}

func wantErr(t *testing.T, b []byte, want error) {
	t.Helper()
	recs, err := Decode(b)
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if recs != nil {
		t.Fatalf("records = %#v on error, want nil", recs)
	}
}

func v2(frames ...[]byte) []byte {
	out := []byte{version2}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// 1. Depth boundary (decode).
func TestDecodeDepthBoundary(t *testing.T) {
	got, err := Decode(v2(nestPut(8)))
	if err != nil {
		t.Fatalf("8 levels: %v", err)
	}
	if !recsEq2(got, []Record{nestRec(8)}) {
		t.Fatalf("8 levels decoded as %#v", got)
	}
	wantErr(t, v2(nestPut(9)), errTooDeep)
}

// 2. Depth boundary (encode).
func TestEncodeDepthBoundary(t *testing.T) {
	recs := []Record{nestRec(8)}
	b, err := Encode(recs)
	if err != nil {
		t.Fatalf("8 levels: %v", err)
	}
	if !bytes.Equal(b, v2(nestPut(8))) {
		t.Fatalf("8-level encoding differs from hand-built frames")
	}
	got, err := Decode(b)
	if err != nil || !recsEq2(got, recs) {
		t.Fatalf("round-trip: %v %#v", err, got)
	}
	b, err = Encode([]Record{nestRec(9)})
	if err == nil || b != nil {
		t.Fatalf("9 levels: b=%v err=%v, want nil bytes and error", b, err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("encode error wraps ErrCorrupt: %v", err)
	}
	if !strings.Contains(err.Error(), "depth 8") {
		t.Fatalf("unexpected error text: %v", err)
	}
	self := &Batch{}
	self.Records = []Record{self}
	if b, err := Encode([]Record{self}); err == nil || b != nil {
		t.Fatalf("self-referential batch: b=%v err=%v", b, err)
	}
	if _, err := Encode([]Record{Batch{Records: []Record{(*Batch)(nil)}}}); err == nil {
		t.Fatal("nil *Batch child accepted")
	}
	if _, err := Encode([]Record{Batch{Records: []Record{nil}}}); err == nil {
		t.Fatal("nil child accepted")
	}
	pb := &Batch{Records: []Record{&Put{Key: "k", Value: []byte("v")}, &Delete{Key: "d"}}}
	b, err = Encode([]Record{pb})
	if err != nil {
		t.Fatal(err)
	}
	got, err = Decode(b)
	if err != nil || !recsEq2(got, []Record{Batch{Records: []Record{Put{Key: "k", Value: []byte("v")}, Delete{Key: "d"}}}}) {
		t.Fatalf("pointer batch round-trip: %v %#v", err, got)
	}
}

// 3. Versions.
func TestVersions(t *testing.T) {
	frame := mkFrame2(kindBatch, batchPL2(1, mkFrame2(KindDelete, delPL("k"))))
	for _, v := range []byte{0, 3, 255} {
		wantErr(t, append([]byte{v}, frame...), errBadVersion)
	}
	wantErr(t, append([]byte{version1}, frame...), errUnknownType)
	got, err := Decode(append([]byte{version2}, frame...))
	if err != nil || !recsEq2(got, []Record{Batch{Records: []Record{Delete{Key: "k"}}}}) {
		t.Fatalf("v2 decode: %v %#v", err, got)
	}
	wantErr(t, nil, errNoVersion)
	wantErr(t, []byte{}, errNoVersion)
	got, err = Decode([]byte{version1})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty v1 stream: %v %#v", err, got)
	}
}

// 4. An inner len one byte past the Batch payload is truncated even though
// the bytes exist later in b.
func TestNoReadThrough(t *testing.T) {
	innerOK := mkFrame2(KindDelete, delPL("k"))
	inner := append([]byte(nil), innerOK...)
	inner[3]++ // len one past the enclosing payload
	outer := mkFrame2(kindBatch, batchPL2(1, inner))
	next := mkFrame2(KindDelete, delPL("zzzz"))
	wantErr(t, v2(outer, next), errTruncated)
}

// 5. Count mismatches.
func TestBatchCountMismatch(t *testing.T) {
	d := mkFrame2(KindDelete, delPL("k"))
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(2, d))), errBatchShort)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(1, d, d))), errTrailing)
	wantErr(t, v2(mkFrame2(kindBatch, append(batchPL2(1, d), 1, 2, 3))), errTrailing)
	wantErr(t, v2(mkFrame2(kindBatch, []byte{0, 0, 1})), errBatchCount)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(0xFFFFFFFF, d))), errBatchShort)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(0, d))), errTrailing)
}

// 6. Inner faults under a valid outer crc.
func TestInnerFaults(t *testing.T) {
	d := mkFrame2(KindDelete, delPL("k"))
	badCRC := append([]byte(nil), d...)
	badCRC[len(badCRC)-1] ^= 1
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(1, badCRC))), errCRC)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(1, be2(maxFrameLen+1)))), errOversize)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(1, append(be2(0), be2(0)...)))), errEmptyFrame)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(1, mkFrame2(99, delPL("k"))))), errUnknownType)
	wantErr(t, v2(mkFrame2(kindBatch, batchPL2(1, []byte{0, 0}))), errShortHeader)
}

// 7. Empty Batch and version byte.
func TestEmptyBatchAndVersionByte(t *testing.T) {
	for _, in := range []Batch{{}, {Records: []Record{}}} {
		b, err := Encode([]Record{in})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		bt, ok := got[0].(Batch)
		if !ok || bt.Records == nil || len(bt.Records) != 0 {
			t.Fatalf("empty batch decoded as %#v", got)
		}
	}
	b, err := Encode([]Record{Put{Key: "k"}, Delete{Key: "d"}})
	if err != nil || b[0] != 2 {
		t.Fatalf("version byte: %v %v", b, err)
	}
	b, err = Encode(nil)
	if err != nil || !bytes.Equal(b, []byte{2}) {
		t.Fatalf("empty encode: %v %v", b, err)
	}
}

// 8. Encode caps and exact capacity.
func TestEncodeCaps(t *testing.T) {
	half := make([]byte, 9<<20)
	if _, err := Encode([]Record{Batch{Records: []Record{Put{Key: "a", Value: half}, Put{Key: "b", Value: half}}}}); err == nil {
		t.Fatal("batch over 16 MiB accepted")
	}
	big := make([]byte, 16<<20)
	if _, err := Encode([]Record{Batch{Records: []Record{Put{Key: "a", Value: big}}}}); err == nil {
		t.Fatal("oversize inner put accepted")
	}
	corpus := [][]Record{
		nil,
		{Put{Key: "a", Value: []byte("1")}},
		{Batch{}, Delete{Key: "x"}, nestRec(8)},
		{Batch{Records: []Record{Batch{Records: []Record{Put{Key: "i"}}}, Delete{Key: "d"}}}, Put{Key: "z", Value: half[:1000]}},
	}
	for i, recs := range corpus {
		b, err := Encode(recs)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != cap(b) {
			t.Fatalf("corpus %d: len %d != cap %d", i, len(b), cap(b))
		}
		got, err := Decode(b)
		if err != nil || !recsEq2(got, recs) {
			t.Fatalf("corpus %d round-trip: %v", i, err)
		}
	}
}

// 9. No aliasing inside Batches.
func TestNoAliasInBatch(t *testing.T) {
	b, err := Encode([]Record{Batch{Records: []Record{Put{Key: "k", Value: []byte("val")}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = 0xEE
	}
	p := got[0].(Batch).Records[0].(Put)
	if p.Key != "k" || string(p.Value) != "val" {
		t.Fatalf("decoded value aliases input: %#v", p)
	}
}

// 10. Allocation contract. A3: faults found before any record is built, at
// any depth, allocate nothing. A4: a fault after an earlier record (top-level
// or sibling) may allocate that record; this is deliberately NOT asserted,
// only that such streams return (nil, ErrCorrupt).
func TestDecodeErrorAllocations(t *testing.T) {
	sentinels := []error{errNoVersion, errBadVersion, errShortHeader, errOversize,
		errEmptyFrame, errTruncated, errCRC, errUnknownType, errPrefixOverrun,
		errTrailing, errTooDeep, errBatchShort, errBatchCount}
	for _, e := range sentinels {
		if !errors.Is(e, ErrCorrupt) {
			t.Fatalf("%v does not wrap ErrCorrupt", e)
		}
	}
	d := mkFrame2(KindDelete, delPL("k"))
	truncInner := d[:len(d)-1]
	badCRC := append([]byte(nil), d...)
	badCRC[len(badCRC)-1] ^= 1
	a3 := map[string][]byte{
		"missing version":  {},
		"bad version":      append([]byte{3}, d...),
		"inner oversize":   v2(mkFrame2(kindBatch, batchPL2(1, be2(maxFrameLen+1)))),
		"depth 9":          v2(nestPut(9)),
		"batch count":      v2(mkFrame2(kindBatch, []byte{0})),
		"batch short":      v2(mkFrame2(kindBatch, batchPL2(1))),
		"inner truncated":  v2(mkFrame2(kindBatch, batchPL2(1, truncInner))),
		"top oversize":     v2(be2(maxFrameLen + 1)),
		"top truncated":    v2(truncInner),
		"top crc":          v2(badCRC),
		"top unknown type": v2(mkFrame2(99, delPL("k"))),
		"v1 batch":         append([]byte{1}, mkFrame2(kindBatch, batchPL2(0))...),
		"nested inner crc": v2(mkFrame2(kindBatch, batchPL2(1, mkFrame2(kindBatch, batchPL2(1, badCRC))))),
	}
	for name, b := range a3 {
		if _, err := Decode(b); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if raceEnabled {
			continue
		}
		if n := testing.AllocsPerRun(50, func() { _, _ = Decode(b) }); n != 0 {
			t.Fatalf("%s: %v allocs, want 0", name, n)
		}
	}
	a4 := [][]byte{
		v2(d, badCRC),
		v2(mkFrame2(kindBatch, batchPL2(2, d, badCRC))),
	}
	for _, b := range a4 {
		wantErr(t, b, ErrCorrupt)
	}
}

// 11. Fuzz: no panics; accepted input re-encodes canonically.
func FuzzDecode(f *testing.F) {
	f.Add(v1Fixture())
	f.Add(v2(nestPut(8)))
	f.Add(v2(nestPut(9)))
	for _, recs := range [][]Record{
		{Batch{Records: []Record{Put{Key: "a", Value: []byte("1")}, Delete{Key: "b"}}}},
		{Put{Key: "p", Value: []byte("v")}, Delete{Key: "d"}, Batch{Records: []Record{Batch{}}}, Put{Key: "last"}},
	} {
		b, err := Encode(recs)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			if recs != nil {
				t.Fatal("non-nil records on error")
			}
			return
		}
		out, err := Encode(recs)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		want := append([]byte(nil), b...)
		want[0] = version2
		if !bytes.Equal(out, want) {
			t.Fatalf("non-canonical: in %x out %x", b, out)
		}
		again, err := Decode(out)
		if err != nil || !recsEq2(again, recs) {
			t.Fatalf("second round-trip: %v", err)
		}
	})
}
