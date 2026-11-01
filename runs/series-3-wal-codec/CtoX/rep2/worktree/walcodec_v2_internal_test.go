package walcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func iU32(x uint32) []byte { return binary.BigEndian.AppendUint32(nil, x) }

func iFrame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(iU32(uint32(len(tp))), tp...)
	return append(out, iU32(crc32.ChecksumIEEE(tp))...)
}

func iPut(key string, val []byte) []byte {
	out := append(iU32(uint32(len(key))), key...)
	out = append(out, iU32(uint32(len(val)))...)
	return append(out, val...)
}

func iBatch(n uint32, frames ...[]byte) []byte {
	out := iU32(n)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func iStream(ver byte, frames ...[]byte) []byte {
	out := []byte{ver}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func wantCorrupt(t *testing.T, name string, b []byte) {
	t.Helper()
	recs, err := Decode(b)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%s: err = %v, want ErrCorrupt", name, err)
	}
	if recs != nil {
		t.Fatalf("%s: recs = %#v, want nil", name, recs)
	}
}

func nest(levels int, inner Record) Record {
	r := inner
	for i := 0; i < levels; i++ {
		r = Batch{Records: []Record{r}}
	}
	return r
}

func TestDepth8Accepted(t *testing.T) {
	recs := []Record{nest(8, Put{Key: "k", Value: []byte("v")})}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEq2(got, recs) {
		t.Fatalf("mismatch")
	}
	// hand-built equivalent also accepted
	cur := iFrame(1, iPut("k", []byte("v")))
	for i := 0; i < 8; i++ {
		cur = iFrame(3, iBatch(1, cur))
	}
	if !bytes.Equal(b, iStream(2, cur)) {
		t.Fatalf("encoder output differs from hand-built stream")
	}
}

func TestDepth9Rejected(t *testing.T) {
	b, err := Encode([]Record{nest(9, Delete{Key: "k"})})
	if err == nil || b != nil {
		t.Fatalf("Encode 9 nested: b=%v err=%v", b, err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("Encode error should not be ErrCorrupt")
	}
	// empty Batch at the 9th level
	if _, err := Encode([]Record{nest(8, Batch{})}); err == nil {
		t.Fatal("Encode: empty 9th-level batch accepted")
	}
	cur := iFrame(3, iBatch(0))
	for i := 0; i < 8; i++ {
		cur = iFrame(3, iBatch(1, cur))
	}
	wantCorrupt(t, "empty 9th-level batch", iStream(2, cur))
}

func TestSelfReferentialBatch(t *testing.T) {
	b := &Batch{}
	b.Records = []Record{b}
	if _, err := Encode([]Record{b}); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestBatchInV1Rejected(t *testing.T) {
	f := iFrame(3, iBatch(1, iFrame(2, iU32(0))))
	wantCorrupt(t, "batch in v1", iStream(1, f))
	if _, err := Decode(iStream(2, f)); err != nil {
		t.Fatalf("same batch in v2: %v", err)
	}
}

func TestOtherVersionsRejected(t *testing.T) {
	for _, v := range []byte{0, 3, 255} {
		wantCorrupt(t, "version", iStream(v))
	}
}

func TestBatchCountMismatch(t *testing.T) {
	d := iFrame(2, append(iU32(1), 'x'))
	wantCorrupt(t, "n too big", iStream(2, iFrame(3, iBatch(2, d))))
	wantCorrupt(t, "n too big (enough bytes)", iStream(2, iFrame(3, append(iBatch(2, d), make([]byte, 20)...))))
	wantCorrupt(t, "n too small", iStream(2, iFrame(3, iBatch(1, d, d))))
	wantCorrupt(t, "trailing junk", iStream(2, iFrame(3, append(iBatch(1, d), 0xAA))))
	wantCorrupt(t, "short count", iStream(2, iFrame(3, []byte{0, 0, 0})))
	wantCorrupt(t, "huge n", iStream(2, iFrame(3, iBatch(0xFFFFFFFF, d))))
}

func TestInnerLenOverrunsBatch(t *testing.T) {
	// inner frame claims more bytes than the batch payload holds; a valid
	// sibling follows at top level, which must not be read.
	inner := iFrame(1, iPut("k", []byte("v")))
	bad := append([]byte{}, inner...)
	binary.BigEndian.PutUint32(bad, uint32(len(inner)))
	bad = bad[:len(inner)-2]
	sib := iFrame(2, iU32(0))
	wantCorrupt(t, "overrun", iStream(2, iFrame(3, iBatch(1, bad)), sib))
}

func TestInnerLenOverCap(t *testing.T) {
	inner := append(iU32(maxFrameLen+1), make([]byte, 16)...)
	wantCorrupt(t, "inner cap", iStream(2, iFrame(3, iBatch(1, inner))))
}

func TestHugeCountNoAlloc(t *testing.T) {
	b := iStream(2, iFrame(3, iBatch(0xFFFFFFFF)))
	allocs := testing.AllocsPerRun(10, func() { _, _ = Decode(b) })
	if allocs > 10 {
		t.Fatalf("allocs = %v", allocs)
	}
}

func TestEmptyBatchRoundTrip(t *testing.T) {
	for _, in := range []Batch{{}, {Records: nil}, {Records: []Record{}}} {
		b, err := Encode([]Record{in})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		gb, ok := got[0].(Batch)
		if len(got) != 1 || !ok || gb.Records == nil || len(gb.Records) != 0 {
			t.Fatalf("got %#v", got)
		}
	}
}

func TestPointerBatch(t *testing.T) {
	v := Batch{Records: []Record{&Put{Key: "a"}, &Delete{Key: "b"}}}
	b1, err := Encode([]Record{v})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Encode([]Record{&v})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("*Batch encodes differently")
	}
	for _, bad := range []Record{(*Batch)(nil), Batch{Records: []Record{nil}}, Batch{Records: []Record{(*Put)(nil)}}} {
		if _, err := Encode([]Record{bad}); err == nil {
			t.Fatalf("Encode(%#v) accepted", bad)
		}
	}
}

func TestEncodeCap(t *testing.T) {
	big := make([]byte, maxFrameLen)
	if _, err := Encode([]Record{Batch{Records: []Record{Put{Key: "k", Value: big}}}}); err == nil {
		t.Fatal("inner over cap accepted")
	}
	half := make([]byte, maxFrameLen/2)
	if _, err := Encode([]Record{Batch{Records: []Record{Put{Value: half}, Put{Value: half}}}}); err == nil {
		t.Fatal("batch sum over cap accepted")
	}
	many := make([]Record, maxFrameLen/13+10)
	for i := range many {
		many[i] = Delete{}
	}
	if _, err := Encode([]Record{Batch{Records: many}}); err == nil {
		t.Fatal("many children over cap accepted")
	}
}

func TestInnerValueNoAlias(t *testing.T) {
	b, err := Encode([]Record{Batch{Records: []Record{Put{Key: "k", Value: []byte("vvvv")}}}})
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
	if v := got[0].(Batch).Records[0].(Put).Value; string(v) != "vvvv" {
		t.Fatalf("aliased: %q", v)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(v1Fixture())
	if b, err := Encode([]Record{Put{Key: "a", Value: []byte("b")}, Batch{Records: []Record{Delete{Key: "x"}, Batch{}}}}); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) || recs != nil {
				t.Fatalf("bad error result: %v %v", recs, err)
			}
			return
		}
		out, err := Encode(recs)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		again, err := Decode(out)
		if err != nil || !recsEq2(again, recs) {
			t.Fatalf("re-decode mismatch: %v", err)
		}
	})
}
