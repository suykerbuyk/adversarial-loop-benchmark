package walcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// --- private helpers (names distinct from the public test files) ---

func tU32(x uint32) []byte {
	return binary.BigEndian.AppendUint32(nil, x)
}

// tFrame builds a well-formed frame for typ and payload.
func tFrame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(tU32(uint32(len(tp))), tp...)
	return append(out, tU32(crc32.ChecksumIEEE(tp))...)
}

// tFrameLen builds a frame with a forced len field but a correct crc over
// type+payload.
func tFrameLen(n uint32, typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(tU32(n), tp...)
	return append(out, tU32(crc32.ChecksumIEEE(tp))...)
}

func tPutPL(key, val string) []byte {
	out := append(tU32(uint32(len(key))), key...)
	out = append(out, tU32(uint32(len(val)))...)
	return append(out, val...)
}

func tDelPL(key string) []byte {
	return append(tU32(uint32(len(key))), key...)
}

func tBatchPL(count uint32, frames ...[]byte) []byte {
	out := tU32(count)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func tStream(ver byte, frames ...[]byte) []byte {
	out := []byte{ver}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func tWantCorrupt(t *testing.T, name string, b []byte) {
	t.Helper()
	recs, err := Decode(b)
	if err != ErrCorrupt || recs != nil {
		t.Errorf("%s: got (%#v, %v), want (nil, ErrCorrupt)", name, recs, err)
	}
}

func tEq(a, b Record) bool {
	switch x := a.(type) {
	case Put:
		y, ok := b.(Put)
		return ok && x.Key == y.Key && bytes.Equal(x.Value, y.Value)
	case Delete:
		y, ok := b.(Delete)
		return ok && x.Key == y.Key
	case Batch:
		y, ok := b.(Batch)
		return ok && tEqs(x.Records, y.Records)
	}
	return false
}

func tEqs(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !tEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

func tNest(levels int, leaf Record) Record {
	r := leaf
	for i := 0; i < levels; i++ {
		r = Batch{Records: []Record{r}}
	}
	return r
}

func tNestBytes(levels int) []byte {
	cur := tFrame(kindPut, tPutPL("k", "v"))
	for i := 0; i < levels; i++ {
		cur = tFrame(kindBatch, tBatchPL(1, cur))
	}
	return tStream(version2, cur)
}

// --- mandatory boundary tests ---

func TestSiblingOverrun(t *testing.T) {
	inner := tPutPL("a", "1")                    // inner Put payload
	sibling := tFrame(kindPut, tPutPL("s", "x")) // top-level sibling
	// Inner len enlarged so it runs past the Batch payload into the Batch crc
	// and the sibling, but still fits within the outer remainder.
	innerLen := uint32(1+len(inner)) + 4 + 4
	innerFrame := tFrameLen(innerLen, kindPut, inner)
	batch := tFrame(kindBatch, tBatchPL(1, innerFrame)) // outer crc valid
	stream := tStream(version2, batch, sibling)
	// Sanity: the overrun would fit within the outer remainder.
	if uint32(len(stream)) < 1+4+1+4+4+innerLen+4 {
		t.Fatal("test construction: overrun does not fit outer remainder")
	}
	tWantCorrupt(t, "sibling overrun", stream)
}

func TestOverrunIntoEnclosingCRC(t *testing.T) {
	inner := tPutPL("a", "1")
	for extra := uint32(1); extra <= 4; extra++ {
		innerFrame := tFrameLen(uint32(1+len(inner))+extra, kindPut, inner)
		batch := tFrame(kindBatch, tBatchPL(1, innerFrame))
		tWantCorrupt(t, "overrun into crc", tStream(version2, batch))
	}
}

func TestNestedOverrunDepth2(t *testing.T) {
	inner := tPutPL("a", "1")
	sib := tFrame(kindDelete, tDelPL("sibling-at-depth-1"))
	innerFrame := tFrameLen(uint32(1+len(inner))+4+uint32(len(sib)), kindPut, inner)
	d2 := tFrame(kindBatch, tBatchPL(1, innerFrame))
	d1 := tFrame(kindBatch, tBatchPL(2, d2, sib))
	tWantCorrupt(t, "nested overrun", tStream(version2, d1))
}

func TestCRCBeforeDispatch(t *testing.T) {
	b, err := Encode([]Record{Batch{Records: []Record{Put{Key: "a", Value: []byte("1")}}}})
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0x01
	tWantCorrupt(t, "outer crc flip", b)

	garbage := tFrame(kindBatch, []byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3})
	garbage[len(garbage)-1] ^= 0x01
	tWantCorrupt(t, "garbage batch with bad crc", tStream(version2, garbage))
}

// --- other cases ---

func TestDepth8OKDepth9Rejected(t *testing.T) {
	recs := []Record{tNest(8, Put{Key: "k", Value: []byte("v")})}
	b, err := Encode(recs)
	if err != nil {
		t.Fatalf("encode 8 levels: %v", err)
	}
	got, err := Decode(b)
	if err != nil || !tEqs(got, recs) {
		t.Fatalf("decode 8 levels: %#v %v", got, err)
	}
	if got, err := Decode(tNestBytes(8)); err != nil || len(got) != 1 {
		t.Fatalf("hand-built 8 levels: %v", err)
	}

	out, err := Encode([]Record{tNest(9, Put{Key: "k"})})
	if err == nil || errors.Is(err, ErrCorrupt) || out != nil {
		t.Fatalf("encode 9 levels: out=%v err=%v", out, err)
	}
	tWantCorrupt(t, "9 levels", tNestBytes(9))
	tWantCorrupt(t, "20 levels", tNestBytes(20))
}

func TestEmptyBatch(t *testing.T) {
	for _, r := range []Record{Batch{}, Batch{Records: []Record{}}} {
		b, err := Encode([]Record{r})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil || len(got) != 1 {
			t.Fatalf("decode: %v", err)
		}
		bt, ok := got[0].(Batch)
		if !ok || bt.Records == nil || len(bt.Records) != 0 {
			t.Fatalf("got %#v", got[0])
		}
	}
}

func TestVersions(t *testing.T) {
	put := tFrame(kindPut, tPutPL("k", "v"))
	for _, v := range []byte{0, 3, 255} {
		tWantCorrupt(t, "bad version", tStream(v, put))
	}
	tWantCorrupt(t, "version only 0", []byte{0})
	if got, err := Decode([]byte{1}); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("v1 empty: %#v %v", got, err)
	}
	if got, err := Decode([]byte{2}); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("v2 empty: %#v %v", got, err)
	}
	b, _ := Encode(nil)
	if !bytes.Equal(b, []byte{2}) {
		t.Fatalf("Encode(nil) = %v", b)
	}
	batch := tFrame(kindBatch, tBatchPL(1, put))
	if _, err := Decode(tStream(version2, batch)); err != nil {
		t.Fatalf("v2 batch: %v", err)
	}
	tWantCorrupt(t, "batch in v1", tStream(version1, batch))
}

func TestBatchCountMismatch(t *testing.T) {
	put := tFrame(kindPut, tPutPL("k", "v"))
	del := tFrame(kindDelete, tDelPL("d"))
	cases := map[string][]byte{
		"count too big":             tBatchPL(3, put, del),
		"count too small":           tBatchPL(1, put, del),
		"count zero, frame present": tBatchPL(0, put),
		"trailing garbage":          append(tBatchPL(2, put, del), 0xAA),
		"short payload 0":           {},
		"short payload 3":           {0, 0, 0},
		"huge count":                tBatchPL(0xFFFFFFFF, put),
		"huge count empty":          tBatchPL(0xFFFFFFFF),
	}
	for name, pl := range cases {
		tWantCorrupt(t, name, tStream(version2, tFrame(kindBatch, pl)))
	}
	if allocs := testing.AllocsPerRun(10, func() {
		_, _ = Decode(tStream(version2, tFrame(kindBatch, tBatchPL(0xFFFFFFFF, put))))
	}); allocs > 20 {
		t.Fatalf("huge count allocs = %v", allocs)
	}
}

func TestInnerFrameErrors(t *testing.T) {
	put := tPutPL("k", "v")
	badCRC := tFrame(kindPut, put)
	badCRC[len(badCRC)-1] ^= 0xff
	zeroLen := append(tU32(0), tU32(0)...)
	bigLen := tFrameLen(maxFrameLen+1, kindPut, put)
	unknown := tFrame(9, put)
	for name, f := range map[string][]byte{
		"inner crc flip": badCRC,
		"inner len 0":    zeroLen,
		"inner len big":  bigLen,
		"inner unknown":  unknown,
		"inner short":    {0, 0},
	} {
		tWantCorrupt(t, name, tStream(version2, tFrame(kindBatch, tBatchPL(1, f))))
	}
}

func TestBatchPointersAndNils(t *testing.T) {
	recs := []Record{&Batch{Records: []Record{&Put{Key: "p"}, &Delete{Key: "d"}}}}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	want := []Record{Batch{Records: []Record{Put{Key: "p", Value: []byte{}}, Delete{Key: "d"}}}}
	if err != nil || !tEqs(got, want) {
		t.Fatalf("got %#v %v", got, err)
	}
	for name, r := range map[string]Record{
		"nil *Batch":        (*Batch)(nil),
		"nil in batch":      Batch{Records: []Record{nil}},
		"nil *Put in batch": Batch{Records: []Record{(*Put)(nil)}},
	} {
		out, err := Encode([]Record{r})
		if err == nil || errors.Is(err, ErrCorrupt) || out != nil {
			t.Errorf("%s: out=%v err=%v", name, out, err)
		}
	}
}

func TestCyclicBatch(t *testing.T) {
	recs := make([]Record, 1)
	recs[0] = Batch{Records: recs} // self-referential slice
	out, err := Encode(recs)
	if err == nil || out != nil {
		t.Fatalf("cyclic: out=%v err=%v", out, err)
	}
	p := &Batch{}
	p.Records = []Record{p}
	if out, err := Encode([]Record{p}); err == nil || out != nil {
		t.Fatalf("cyclic ptr: out=%v err=%v", out, err)
	}
}

func TestBatchOverCap(t *testing.T) {
	big := make([]byte, 9<<20)
	r := Batch{Records: []Record{Put{Key: "a", Value: big}, Put{Key: "b", Value: big}}}
	if out, err := Encode([]Record{r}); err == nil || out != nil {
		t.Fatalf("over cap: err=%v", err)
	}
	// Shared sub-batch DAG: exponential logical size, must fail fast.
	var x Record = Put{Key: "k", Value: make([]byte, 64)}
	for i := 0; i < 8; i++ {
		x = Batch{Records: []Record{x, x, x, x, x, x, x, x, x, x}}
	}
	if out, err := Encode([]Record{x}); err == nil || out != nil {
		t.Fatalf("dag: err=%v", err)
	}
}

func TestNoAliasing(t *testing.T) {
	b, err := Encode([]Record{
		Put{Key: "top", Value: []byte("tv")},
		Batch{Records: []Record{Put{Key: "in", Value: []byte("iv")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = 0
	}
	want := []Record{
		Put{Key: "top", Value: []byte("tv")},
		Batch{Records: []Record{Put{Key: "in", Value: []byte("iv")}}},
	}
	if !tEqs(got, want) {
		t.Fatalf("aliasing: %#v", got)
	}
}

func FuzzDecode(f *testing.F) {
	seeds := [][]byte{
		{1}, {2}, tNestBytes(1), tNestBytes(8), tNestBytes(9),
		tStream(version1, tFrame(kindPut, tPutPL("alpha", "one")), tFrame(kindDelete, tDelPL("beta"))),
	}
	if b, err := Encode([]Record{
		Put{Key: "p", Value: []byte("v")}, Delete{Key: "d"},
		Batch{Records: []Record{Batch{Records: []Record{Put{Key: "x"}}}, Delete{Key: "y"}}},
	}); err == nil {
		seeds = append(seeds, b)
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			if err != ErrCorrupt || recs != nil {
				t.Fatalf("bad error result: %v %v", recs, err)
			}
			return
		}
		enc, err := Encode(recs)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		again, err := Decode(enc)
		if err != nil || !tEqs(recs, again) {
			t.Fatalf("round-trip mismatch: %v", err)
		}
	})
}
