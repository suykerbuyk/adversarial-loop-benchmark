package walcodec_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"walcodec"
)

const (
	implVersion1 = 0x01
	implVersion2 = 0x02
	implMaxFrame = 16 << 20
)

type (
	implRecord = walcodec.Record
	implPut    = walcodec.Put
	implDelete = walcodec.Delete
	implBatch  = walcodec.Batch
)

func implBE(x uint32) []byte { return binary.BigEndian.AppendUint32(nil, x) }

func implFrame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(implBE(uint32(len(tp))), tp...)
	return append(out, implBE(crc32.ChecksumIEEE(tp))...)
}

func implPutPL(k string, v []byte) []byte {
	out := append(implBE(uint32(len(k))), k...)
	out = append(out, implBE(uint32(len(v)))...)
	return append(out, v...)
}

func implDelPL(k string) []byte { return append(implBE(uint32(len(k))), k...) }

func implBatchPL(n uint32, frames ...[]byte) []byte {
	out := implBE(n)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func implStream(ver byte, frames ...[]byte) []byte {
	out := []byte{ver}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func implFixture() []byte {
	return []byte{
		0x01, 0x00, 0x00, 0x00, 0x11, 0x01, 0x00, 0x00, 0x00, 0x05, 0x61, 0x6c,
		0x70, 0x68, 0x61, 0x00, 0x00, 0x00, 0x03, 0x6f, 0x6e, 0x65, 0x20, 0x74,
		0x5b, 0x79, 0x00, 0x00, 0x00, 0x09, 0x02, 0x00, 0x00, 0x00, 0x04, 0x62,
		0x65, 0x74, 0x61, 0x93, 0xaa, 0x41, 0x97,
	}
}

func implFixtureRecords() []implRecord {
	return []implRecord{implPut{Key: "alpha", Value: []byte("one")}, implDelete{Key: "beta"}}
}

// implDeref turns pointer records into values for comparison.
func implDeref(r implRecord) implRecord {
	switch v := r.(type) {
	case *implPut:
		return *v
	case *implDelete:
		return *v
	case *implBatch:
		return *v
	}
	return r
}

func implEq(a, b implRecord) bool {
	a, b = implDeref(a), implDeref(b)
	switch x := a.(type) {
	case implPut:
		y, ok := b.(implPut)
		return ok && x.Key == y.Key && bytes.Equal(x.Value, y.Value)
	case implDelete:
		y, ok := b.(implDelete)
		return ok && x.Key == y.Key
	case implBatch:
		y, ok := b.(implBatch)
		return ok && implEqs(x.Records, y.Records)
	}
	return false
}

func implEqs(a, b []implRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !implEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

func implNest(depth int, inner implRecord) implRecord {
	r := inner
	for i := 0; i < depth; i++ {
		r = implBatch{Records: []implRecord{r}}
	}
	return r
}

func implNestBytes(depth int, innermost []byte) []byte {
	cur := innermost
	for i := 0; i < depth; i++ {
		cur = implFrame(3, implBatchPL(1, cur))
	}
	return implStream(implVersion2, cur)
}

func implRoundTrip(t *testing.T, recs []implRecord) {
	t.Helper()
	b, err := walcodec.Encode(recs)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if b[0] != implVersion2 {
		t.Fatalf("version byte %d", b[0])
	}
	got, err := walcodec.Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !implEqs(got, recs) {
		t.Fatalf("got %#v want %#v", got, recs)
	}
}

func implWantCorrupt(t *testing.T, name string, b []byte) {
	t.Helper()
	got, err := walcodec.Decode(b)
	if err != walcodec.ErrCorrupt || got != nil {
		t.Errorf("%s: got (%v, %v), want (nil, ErrCorrupt)", name, got, err)
	}
}

func implWantEncodeErr(t *testing.T, name string, recs []implRecord) {
	t.Helper()
	b, err := walcodec.Encode(recs)
	if err == nil || b != nil {
		t.Errorf("%s: got (%d bytes, %v), want (nil, error)", name, len(b), err)
		return
	}
	if errors.Is(err, walcodec.ErrCorrupt) {
		t.Errorf("%s: Encode error is ErrCorrupt: %v", name, err)
	}
}

// 1. Golden bytes.
func TestImplGoldenBatch(t *testing.T) {
	got, err := walcodec.Encode([]implRecord{implBatch{Records: []implRecord{
		implPut{Key: "a", Value: []byte("1")}, implDelete{Key: "b"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	want := implStream(implVersion2, implFrame(3, implBatchPL(2,
		implFrame(1, implPutPL("a", []byte("1"))),
		implFrame(2, implDelPL("b")),
	)))
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x\nwant %x", got, want)
	}
}

// 2. Frame format unchanged from v1.
func TestImplFrameFormatUnchanged(t *testing.T) {
	got, err := walcodec.Encode(implFixtureRecords())
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != implVersion2 || !bytes.Equal(got[1:], implFixture()[1:]) {
		t.Fatalf("got %x", got)
	}
}

// 3. Every kind, value and pointer, at depths 0-8.
func TestImplKindsAllDepths(t *testing.T) {
	if (implBatch{}).Kind() != 3 || (implPut{}).Kind() != 1 || (implDelete{}).Kind() != 2 {
		t.Fatal("bad Kind values")
	}
	leaves := []implRecord{
		implPut{Key: "k", Value: []byte("v")},
		&implPut{Key: "pk", Value: nil},
		implDelete{Key: "d"},
		&implDelete{Key: "pd"},
		implBatch{},
		&implBatch{Records: []implRecord{implDelete{Key: "x"}}},
	}
	for depth := 0; depth <= 8; depth++ {
		for _, leaf := range leaves {
			// A Batch leaf adds a level of its own, so it fits only below 8.
			if _, ok := implDeref(leaf).(implBatch); ok && depth == 8 {
				continue
			}
			implRoundTrip(t, []implRecord{implNest(depth, leaf)})
		}
	}
}

// 4. Depth boundary.
func TestImplDepthBoundary(t *testing.T) {
	put := implPut{Key: "deep", Value: []byte("x")}
	implRoundTrip(t, []implRecord{implNest(8, put)})
	implRoundTrip(t, []implRecord{implNest(7, implBatch{})})
	implWantEncodeErr(t, "9 nested", []implRecord{implNest(9, put)})
	implWantEncodeErr(t, "8 nested + empty", []implRecord{implNest(8, implBatch{})})

	putFrame := implFrame(1, implPutPL("deep", []byte("x")))
	emptyBatch := implFrame(3, implBatchPL(0))
	if _, err := walcodec.Decode(implNestBytes(8, putFrame)); err != nil {
		t.Fatalf("8-deep: %v", err)
	}
	if _, err := walcodec.Decode(implNestBytes(7, emptyBatch)); err != nil {
		t.Fatalf("8-deep empty: %v", err)
	}
	implWantCorrupt(t, "9-deep put", implNestBytes(9, putFrame))
	implWantCorrupt(t, "9-deep empty", implNestBytes(8, emptyBatch))
}

// 5. Cyclic input.
func TestImplCycle(t *testing.T) {
	s := make([]implRecord, 1)
	s[0] = implBatch{Records: s}
	implWantEncodeErr(t, "cycle value", s)

	p := &implBatch{}
	p.Records = []implRecord{p}
	implWantEncodeErr(t, "cycle pointer", []implRecord{p})
}

// 6. Versions.
func TestImplVersions(t *testing.T) {
	valid, err := walcodec.Encode([]implRecord{implPut{Key: "k", Value: []byte("v")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []byte{0, 3, 255} {
		b := append([]byte(nil), valid...)
		b[0] = v
		implWantCorrupt(t, "version", b)
	}
	for _, v := range []byte{implVersion1, implVersion2} {
		got, err := walcodec.Decode([]byte{v})
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("version-only %d: (%v, %v)", v, got, err)
		}
	}
	implWantCorrupt(t, "empty", nil)
	implWantCorrupt(t, "batch in v1", implStream(implVersion1, implFrame(3, implBatchPL(0))))
	if b, _ := walcodec.Encode(nil); !bytes.Equal(b, []byte{implVersion2}) {
		t.Errorf("Encode(nil) = %x", b)
	}
}

// 7. Batch structure corruption (valid outer CRCs).
func TestImplBatchCorruption(t *testing.T) {
	pf := implFrame(1, implPutPL("a", []byte("1")))
	df := implFrame(2, implDelPL("b"))
	bad := func(pl []byte) []byte { return implStream(implVersion2, implFrame(3, pl)) }

	flipped := append([]byte(nil), pf...)
	flipped[len(flipped)-1] ^= 1
	overrun := append(implBE(uint32(len(pf)-4+5)), pf[4:]...) // len 5 bytes too big
	sibling := implFrame(2, implDelPL("sibling"))

	cases := map[string][]byte{
		"n too large":     bad(implBatchPL(3, pf, df)),
		"n too small":     bad(implBatchPL(1, pf, df)),
		"n=0 trailing":    bad(append(implBatchPL(0), 0)),
		"trailing 1":      bad(append(implBatchPL(2, pf, df), 0)),
		"trailing 2":      bad(append(implBatchPL(2, pf, df), 0, 0)),
		"trailing 3":      bad(append(implBatchPL(2, pf, df), 0, 0, 0)),
		"short payload":   bad([]byte{0, 0, 0}),
		"empty payload":   bad(nil),
		"inner len 0":     bad(implBatchPL(1, implBE(0))),
		"inner oversize":  bad(implBatchPL(1, implBE(implMaxFrame+1), []byte{1, 2, 3, 4})),
		"inner bad type":  bad(implBatchPL(1, implFrame(9, implDelPL("x")))),
		"inner type 0":    bad(implBatchPL(1, implFrame(0, implDelPL("x")))),
		"inner crc flip":  bad(implBatchPL(1, flipped)),
		"put trailing":    bad(implBatchPL(1, implFrame(1, append(implPutPL("a", nil), 7)))),
		"delete trailing": bad(implBatchPL(1, implFrame(2, append(implDelPL("a"), 7)))),
		"inner overrun":   append(bad(implBatchPL(1, overrun)), sibling...),
	}
	for name, b := range cases {
		implWantCorrupt(t, name, b)
	}
	// Sanity: the uncorrupted form decodes.
	if _, err := walcodec.Decode(bad(implBatchPL(2, pf, df))); err != nil {
		t.Fatal(err)
	}
}

// 8. Every prefix of a mixed nested stream.
func TestImplEveryPrefix(t *testing.T) {
	recs := []implRecord{
		implPut{Key: "p", Value: []byte("v")},
		implBatch{Records: []implRecord{
			implDelete{Key: "d"},
			implBatch{Records: []implRecord{implPut{Key: "q", Value: nil}}},
		}},
		implDelete{Key: "z"},
	}
	full, err := walcodec.Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	bounds := map[int]int{1: 0}
	off := 1
	for i, r := range recs {
		one, err := walcodec.Encode([]implRecord{r})
		if err != nil {
			t.Fatal(err)
		}
		off += len(one) - 1
		bounds[off] = i + 1
	}
	if off != len(full) {
		t.Fatalf("boundary math: %d vs %d", off, len(full))
	}
	implWantCorrupt(t, "[:0]", full[:0])
	for i := 1; i <= len(full); i++ {
		got, err := walcodec.Decode(full[:i])
		if k, ok := bounds[i]; ok {
			if err != nil || got == nil || !implEqs(got, recs[:k]) {
				t.Errorf("prefix %d: (%v, %v)", i, got, err)
			}
		} else if err != walcodec.ErrCorrupt || got != nil {
			t.Errorf("prefix %d: (%v, %v), want ErrCorrupt", i, got, err)
		}
	}
}

// 9. Allocation bounds. Must not use t.Parallel(): AllocsPerRun counts
// process-wide allocations.
func TestImplAllocBounds(t *testing.T) {
	oversize := implStream(implVersion2, implFrame(3, implBatchPL(1, implBE(implMaxFrame+1), []byte{1, 2, 3, 4})))
	if a := testing.AllocsPerRun(50, func() { _, _ = walcodec.Decode(oversize) }); a != 0 {
		t.Errorf("inner oversize allocs = %v, want 0", a)
	}
	// Filler after the bogus inner header must not let any preallocation
	// sneak in before the inner len is rejected.
	filler := make([]byte, 1000)
	for _, n := range []uint32{1, 2, 0xFFFFFFFF} {
		withFiller := implStream(implVersion2, implFrame(3, implBatchPL(n, implBE(implMaxFrame+1), filler)))
		implWantCorrupt(t, "oversize+filler", withFiller)
		if a := testing.AllocsPerRun(50, func() { _, _ = walcodec.Decode(withFiller) }); a != 0 {
			t.Errorf("inner oversize with filler (n=%d) allocs = %v, want 0", n, a)
		}
	}
	hugeN := implStream(implVersion2, implFrame(3, implBatchPL(0xFFFFFFFF)))
	implWantCorrupt(t, "huge n", hugeN)
	if a := testing.AllocsPerRun(50, func() { _, _ = walcodec.Decode(hugeN) }); a != 0 {
		t.Errorf("huge n allocs = %v, want 0", a)
	}
	// 8 nested Batches each claiming n=0xFFFFFFFF around junk: no level may
	// reserve memory before validating a frame.
	cur := append(implBE(implMaxFrame+1), make([]byte, 64<<10)...)
	for i := 0; i < 8; i++ {
		cur = implFrame(3, implBatchPL(0xFFFFFFFF, cur))
	}
	nested := implStream(implVersion2, cur)
	implWantCorrupt(t, "nested huge n", nested)
	if a := testing.AllocsPerRun(10, func() { _, _ = walcodec.Decode(nested) }); a != 0 {
		t.Errorf("nested huge n allocs = %v, want 0", a)
	}
}

// 9b. Batch{} (nil Records) decodes with a non-nil, empty Records slice.
func TestImplEmptyBatchNonNil(t *testing.T) {
	b, err := walcodec.Encode([]implRecord{implBatch{}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := walcodec.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if r := got[0].(implBatch).Records; r == nil || len(r) != 0 {
		t.Fatalf("Records = %#v, want empty non-nil", r)
	}
}

type implForeign struct{}

func (implForeign) Kind() uint8 { return 1 }

// 10. Encode rejection.
func TestImplEncodeRejection(t *testing.T) {
	var nilBatch *implBatch
	var nilPut *implPut
	big := make([]byte, 9<<20)
	implWantEncodeErr(t, "nil top", []implRecord{nil})
	implWantEncodeErr(t, "nil in batch", []implRecord{implBatch{Records: []implRecord{nil}}})
	implWantEncodeErr(t, "nil *Batch", []implRecord{nilBatch})
	implWantEncodeErr(t, "nil *Put in batch", []implRecord{implBatch{Records: []implRecord{nilPut}}})
	implWantEncodeErr(t, "foreign", []implRecord{implForeign{}})
	implWantEncodeErr(t, "foreign in batch", []implRecord{implBatch{Records: []implRecord{implForeign{}}}})
	implWantEncodeErr(t, "aggregate", []implRecord{implBatch{Records: []implRecord{
		implPut{Key: "a", Value: big}, implPut{Key: "b", Value: big},
	}}})
	implWantEncodeErr(t, "key oversize", []implRecord{implDelete{Key: string(make([]byte, implMaxFrame+1))}})
	implWantEncodeErr(t, "put frame oversize", []implRecord{implPut{Key: "", Value: make([]byte, implMaxFrame-8)}})

	// Batch frame len exactly 16 MiB: 1 + 4 + (4+1+8+kv+4) = 22 + kv.
	exact := []implRecord{implBatch{Records: []implRecord{implPut{Key: "k", Value: make([]byte, implMaxFrame-22-1)}}}}
	b, err := walcodec.Encode(exact)
	if err != nil {
		t.Fatalf("exact 16 MiB batch: %v", err)
	}
	if l := binary.BigEndian.Uint32(b[1:5]); l != implMaxFrame {
		t.Fatalf("frame len %d", l)
	}
	got, err := walcodec.Decode(b)
	if err != nil || !implEqs(got, exact) {
		t.Fatalf("exact round-trip: %v", err)
	}
	exact[0].(implBatch).Records[0] = implPut{Key: "k", Value: make([]byte, implMaxFrame-22)}
	implWantEncodeErr(t, "batch one over", exact)
}

// 11. No aliasing.
func TestImplNoAliasing(t *testing.T) {
	b, err := walcodec.Encode([]implRecord{implBatch{Records: []implRecord{implPut{Key: "key", Value: []byte("val")}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := walcodec.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = 0xEE
	}
	p := got[0].(implBatch).Records[0].(implPut)
	if p.Key != "key" || string(p.Value) != "val" {
		t.Fatalf("aliased: %#v", p)
	}
}

// 12. Fuzz.
func FuzzImplDecode(f *testing.F) {
	nested, _ := walcodec.Encode([]implRecord{
		implPut{Key: "p", Value: []byte("v")},
		implBatch{Records: []implRecord{implDelete{Key: "d"}, implBatch{}}},
	})
	putFrame := implFrame(1, implPutPL("deep", []byte("x")))
	f.Add(implFixture())
	f.Add(nested)
	f.Add(implNestBytes(8, putFrame))
	f.Add(implNestBytes(9, putFrame))
	f.Fuzz(func(t *testing.T, in []byte) {
		got, err := walcodec.Decode(in)
		if err != nil {
			if err != walcodec.ErrCorrupt || got != nil {
				t.Fatalf("failure contract: (%v, %v)", got, err)
			}
			return
		}
		if got == nil {
			t.Fatal("nil slice on success")
		}
		out, err := walcodec.Encode(got)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if out[0] != implVersion2 || !bytes.Equal(out[1:], in[1:]) {
			t.Fatalf("re-encode mismatch\nin  %x\nout %x", in, out)
		}
	})
}
