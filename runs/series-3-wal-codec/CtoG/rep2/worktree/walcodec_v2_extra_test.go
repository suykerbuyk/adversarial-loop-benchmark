package walcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math/rand"
	"runtime"
	"testing"
	"time"
)

// --- helpers ("x" suffix avoids clashes with the public suites) ---

func beX(x uint32) []byte { return binary.BigEndian.AppendUint32(nil, x) }

func frameX(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(beX(uint32(len(tp))), tp...)
	return append(out, beX(crc32.ChecksumIEEE(tp))...)
}

// rawFrameX builds a frame with an explicit len field and a crc over tp.
func rawFrameX(lenField uint32, tp []byte) []byte {
	out := append(beX(lenField), tp...)
	return append(out, beX(crc32.ChecksumIEEE(tp))...)
}

func putX(key, val string) []byte {
	out := append(beX(uint32(len(key))), key...)
	out = append(out, beX(uint32(len(val)))...)
	return frameX(1, append(out, val...))
}

func delX(key string) []byte { return frameX(2, append(beX(uint32(len(key))), key...)) }

func batchX(n uint32, frames ...[]byte) []byte {
	pl := beX(n)
	for _, f := range frames {
		pl = append(pl, f...)
	}
	return frameX(3, pl)
}

func streamX(ver byte, frames ...[]byte) []byte {
	out := []byte{ver}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func wantCorruptX(t *testing.T, name string, b []byte) {
	t.Helper()
	got, err := Decode(b)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%s: err = %v, want ErrCorrupt", name, err)
	}
	if got != nil {
		t.Fatalf("%s: got %#v, want nil result on error", name, got)
	}
}

func eqX(a, b Record) bool {
	switch x := a.(type) {
	case Put:
		y, ok := b.(Put)
		return ok && x.Key == y.Key && bytes.Equal(x.Value, y.Value)
	case Delete:
		y, ok := b.(Delete)
		return ok && x.Key == y.Key
	case Batch:
		y, ok := b.(Batch)
		return ok && eqsX(x.Records, y.Records)
	}
	return false
}

func eqsX(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !eqX(a[i], b[i]) {
			return false
		}
	}
	return true
}

// nestX wraps inner in levels Batches (as records).
func nestX(levels int, inner []Record) []Record {
	cur := inner
	for i := 0; i < levels; i++ {
		cur = []Record{Batch{Records: cur}}
	}
	return cur
}

// nestFrameX wraps the frame f in levels Batch frames of count 1.
func nestFrameX(levels int, f []byte) []byte {
	for i := 0; i < levels; i++ {
		f = batchX(1, f)
	}
	return f
}

// noNilX reports whether recs contains no nil Record at any depth.
func noNilX(recs []Record) bool {
	for _, r := range recs {
		switch x := r.(type) {
		case nil:
			return false
		case Batch:
			if x.Records == nil || !noNilX(x.Records) {
				return false
			}
		}
	}
	return true
}

// --- tests ---

func TestBatchKindX(t *testing.T) {
	if (Batch{}).Kind() != 3 {
		t.Fatal("Batch.Kind != 3")
	}
	var _ Record = &Batch{}
}

func TestDepthBoundaryX(t *testing.T) {
	leaf := []Record{Put{Key: "deep", Value: []byte("x")}}
	hand8 := streamX(2, nestFrameX(8, putX("deep", "x")))
	got, err := Decode(hand8)
	if err != nil {
		t.Fatalf("8 hand-built levels: %v", err)
	}
	if !eqsX(got, nestX(8, leaf)) {
		t.Fatalf("8 levels decoded wrong: %#v", got)
	}
	enc8, err := Encode(nestX(8, leaf))
	if err != nil {
		t.Fatalf("Encode 8 levels: %v", err)
	}
	if !bytes.Equal(enc8, hand8) {
		t.Fatal("Encode of 8 levels differs from hand-built stream")
	}

	enc9, err := Encode(nestX(9, leaf))
	if err == nil || enc9 != nil || errors.Is(err, ErrCorrupt) {
		t.Fatalf("Encode 9 levels: (%v, %v), want (nil, non-ErrCorrupt err)", enc9, err)
	}
	wantCorruptX(t, "9 hand-built levels", streamX(2, nestFrameX(9, putX("deep", "x"))))

	// Empty Batch as 8th level: legal. As 9th level: rejected.
	empty := batchX(0)
	hand8e := streamX(2, nestFrameX(7, empty))
	if _, err := Decode(hand8e); err != nil {
		t.Fatalf("empty 8th level: %v", err)
	}
	enc8e, err := Encode(nestX(8, nil))
	if err != nil || !bytes.Equal(enc8e, hand8e) {
		t.Fatalf("Encode empty 8th level: err=%v equal=%v", err, bytes.Equal(enc8e, hand8e))
	}
	wantCorruptX(t, "empty 9th level", streamX(2, nestFrameX(8, empty)))
	if b, err := Encode(nestX(9, nil)); err == nil || b != nil {
		t.Fatal("Encode empty 9th level accepted")
	}
}

func TestErrorsStopDecodingX(t *testing.T) {
	put := putX("k", "v")
	badInner := map[string][]byte{
		"truncated": put[:len(put)-1],
		"crcflip": func() []byte {
			f := append([]byte(nil), put...)
			f[len(f)-1] ^= 1
			return f
		}(),
		"unknown type": frameX(99, beX(0)),
		"len zero":     append(beX(0), beX(crc32.ChecksumIEEE(nil))...),
		"len over cap": rawFrameX(maxFrameLen+1, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0}),
		"put trailing": frameX(1, append(append(beX(1), 'k'), append(beX(1), 'v', 'z')...)),
		"9th level":    nil, // built per depth below
	}
	type result struct {
		name string
		got  []Record
		err  error
	}
	results := make(chan result)
	go func() {
		defer close(results)
		for name, bad := range badInner {
			for _, depth := range []int{1, 3} {
				inner := bad
				if inner == nil {
					// An empty Batch whose sub-stream would sit at depth 9.
					inner = nestFrameX(8-depth, batchX(0))
				}
				// Bad frame followed by a valid sibling inside a Batch, wrapped
				// so the bad frame sits at the given depth, then a valid
				// top-level sibling after the Batch.
				cur := frameX(3, append(append(beX(2), inner...), delX("sib")...))
				cur = nestFrameX(depth-1, cur)
				got, err := Decode(streamX(2, cur, putX("top", "v")))
				results <- result{name, got, err}
			}
		}
	}()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case r, ok := <-results:
			if !ok {
				return
			}
			if !errors.Is(r.err, ErrCorrupt) || r.got != nil {
				t.Errorf("%s: (%#v, %v), want (nil, ErrCorrupt)", r.name, r.got, r.err)
			}
		case <-timeout:
			t.Fatal("decoder did not terminate")
		}
	}
}

func TestCyclicEncodeX(t *testing.T) {
	s := make([]Record, 1)
	s[0] = Batch{Records: s}
	if b, err := Encode(s); err == nil || b != nil {
		t.Fatal("self-referential Batch accepted")
	}
	p := &Batch{}
	p.Records = []Record{p}
	if b, err := Encode([]Record{p}); err == nil || b != nil {
		t.Fatal("*Batch cycle accepted")
	}
}

func TestEncoderChildErrorX(t *testing.T) {
	for _, depth := range []int{1, 5} {
		recs := nestX(depth-1, []Record{Batch{Records: []Record{Delete{Key: "a"}, nil}}})
		b, err := Encode(recs)
		if err == nil || b != nil {
			t.Fatalf("depth %d: (%v, %v)", depth, b, err)
		}
	}
	for _, r := range []Record{(*Batch)(nil), (*Put)(nil), (*Delete)(nil)} {
		if b, err := Encode([]Record{Batch{Records: []Record{r}}}); err == nil || b != nil {
			t.Fatalf("nil pointer %T accepted", r)
		}
	}
}

func TestInnerLenOverrunX(t *testing.T) {
	// Inner Put frame claims a len 9 bytes longer than present; the bytes that
	// follow the outer Batch (a top-level Delete frame) would complete it if
	// the decoder read past the Batch payload.
	pl := append(append(beX(1), 'k'), beX(1)...)
	pl = append(pl, 'v')
	tp := append([]byte{1}, pl...)
	inner := append(beX(uint32(len(tp)+9)), tp...) // no crc, len too big
	outer := frameX(3, append(beX(1), inner...))
	wantCorruptX(t, "inner overrun", streamX(2, outer, delX("abcde")))
}

func TestCountMismatchX(t *testing.T) {
	d := delX("a")
	cases := map[string][]byte{
		"n=2 with 1":    batchX(2, d),
		"n=1 with 2":    batchX(1, d, d),
		"n=1 + junk":    batchX(1, d, []byte{0}),
		"n=0 + junk":    batchX(0, []byte{1, 2, 3}),
		"n=max tiny":    batchX(0xFFFFFFFF, d),
		"no count":      frameX(3, []byte{0, 0}),
		"v1 batch tag":  nil,
		"n=1 with none": batchX(1),
	}
	for name, f := range cases {
		if f == nil {
			wantCorruptX(t, name, streamX(1, batchX(1, d)))
			continue
		}
		wantCorruptX(t, name, streamX(2, f))
	}
}

func allocPerDecodeX(b []byte) uint64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for i := 0; i < 100; i++ {
		_, _ = Decode(b)
	}
	runtime.ReadMemStats(&m1)
	return (m1.TotalAlloc - m0.TotalAlloc) / 100
}

func TestAllocationX(t *testing.T) {
	const overhead = 1024
	// 1. n = 0xFFFFFFFF with a 13-byte frame.
	b1 := streamX(2, rawFrameX(5, append([]byte{3}, beX(0xFFFFFFFF)...)))
	if got := allocPerDecodeX(b1); got > overhead+16 {
		t.Errorf("huge n: %d bytes/op", got)
	}
	// 2. Huge claimed len, 6-byte stream.
	b2 := append([]byte{2}, beX(maxFrameLen+1)...)
	b2 = append(b2, 3)
	if got := allocPerDecodeX(b2); got > overhead {
		t.Errorf("huge len: %d bytes/op", got)
	}
	// 3. Same header plus 1 MiB padding: hint is capped at 1024 entries
	// (16 KiB). The runtime adds a malloc header to pointer-bearing objects,
	// pushing 16 KiB into the next size class (18 KiB), so allow 1/8 slack
	// for size-class rounding. An uncapped hint would be ~1.2 MiB.
	b3 := append(append([]byte(nil), b2...), make([]byte, 1<<20)...)
	if got := allocPerDecodeX(b3); got > overhead+16*1024*9/8 {
		t.Errorf("huge len padded: %d bytes/op", got)
	}
	// 4. Linear bound: 10,000 empty-key Deletes in one Batch.
	recs := make([]Record, 10000)
	for i := range recs {
		recs[i] = Delete{}
	}
	b4, err := Encode([]Record{Batch{Records: recs}})
	if err != nil {
		t.Fatal(err)
	}
	if got := allocPerDecodeX(b4); got > uint64(4*len(b4)) {
		t.Errorf("linear: %d bytes/op for %d-byte input", got, len(b4))
	}
}

func TestEncoderCapBoundaryX(t *testing.T) {
	if testing.Short() {
		t.Skip("large allocations")
	}
	// Batch len = type(1) + n(4) + child frame; child frame = 4 + 1 + 4+0 + 4+v + 4.
	// Want 1+4+17+v = maxFrameLen => v = maxFrameLen - 22.
	v := int(maxFrameLen) - 22
	val := make([]byte, v)
	b, err := Encode([]Record{Batch{Records: []Record{Put{Value: val}}}})
	if err != nil {
		t.Fatalf("exact cap: %v", err)
	}
	if got := binary.BigEndian.Uint32(b[1:]); got != maxFrameLen {
		t.Fatalf("len field %d, want %d", got, maxFrameLen)
	}
	got, err := Decode(b)
	if err != nil || len(got) != 1 {
		t.Fatalf("decode exact cap: %v", err)
	}
	if bb, err := Encode([]Record{Batch{Records: []Record{Put{Value: make([]byte, v+1)}}}}); err == nil || bb != nil {
		t.Fatal("over cap by one accepted")
	}
	// Max leaf Put (len = maxFrameLen) wrapped in a Batch: outer overflows.
	maxLeaf := make([]byte, int(maxFrameLen)-9)
	if _, err := Encode([]Record{Put{Value: maxLeaf}}); err != nil {
		t.Fatalf("max leaf: %v", err)
	}
	if bb, err := Encode([]Record{Batch{Records: []Record{Put{Value: maxLeaf}}}}); err == nil || bb != nil {
		t.Fatal("wrapped max leaf accepted")
	}
	// Hand-built Batch frame with len = maxFrameLen+1 and a valid crc.
	tp := append([]byte{3}, beX(1)...)
	child := putX("", string(make([]byte, v+1)))
	tp = append(tp, child...)
	if uint32(len(tp)) != maxFrameLen+1 {
		t.Fatalf("setup: len %d", len(tp))
	}
	wantCorruptX(t, "batch over cap", streamX(2, rawFrameX(uint32(len(tp)), tp)))
}

func TestPreGuardX(t *testing.T) {
	if testing.Short() {
		t.Skip("large allocations")
	}
	recs := make([]Record, 1290555)
	for i := range recs {
		recs[i] = Delete{}
	}
	if b, err := Encode([]Record{Batch{Records: recs}}); err == nil || b != nil {
		t.Fatal("over pre-guard accepted")
	}
	b, err := Encode([]Record{Batch{Records: recs[:1290554]}})
	if err != nil {
		t.Fatalf("at pre-guard: %v", err)
	}
	if got := binary.BigEndian.Uint32(b[1:]); got != 16777207 {
		t.Fatalf("len %d", got)
	}
}

func TestVersionGatingX(t *testing.T) {
	for _, v := range []byte{0, 3, 255} {
		wantCorruptX(t, "version", streamX(v, delX("a")))
	}
	wantCorruptX(t, "tag 3 in v1", streamX(1, batchX(0)))
	for _, v := range []byte{1, 2} {
		got, err := Decode([]byte{v})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("version-only %d: %#v, %v", v, got, err)
		}
	}
	wantCorruptX(t, "nil", nil)
	wantCorruptX(t, "empty", []byte{})
}

func TestGoldenBytesX(t *testing.T) {
	b, err := Encode(v1FixtureRecords())
	if err != nil {
		t.Fatal(err)
	}
	want := v1Fixture()
	want[0] = 2
	if !bytes.Equal(b, want) {
		t.Fatalf("Encode(fixture records) = %x, want %x", b, want)
	}
	hand := streamX(2, batchX(2, putX("a", "1"), delX("b")))
	b, err = Encode([]Record{Batch{Records: []Record{Put{Key: "a", Value: []byte("1")}, Delete{Key: "b"}}}})
	if err != nil || !bytes.Equal(b, hand) {
		t.Fatalf("batch golden: %x vs %x (%v)", b, hand, err)
	}
	if b, _ := Encode(nil); !bytes.Equal(b, []byte{2}) {
		t.Fatalf("Encode(nil) = %x", b)
	}
}

func TestMiscX(t *testing.T) {
	b, err := Encode([]Record{&Batch{Records: []Record{&Put{Key: "a"}, &Delete{Key: "b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{Batch{Records: []Record{Put{Key: "a", Value: []byte{}}, Delete{Key: "b"}}}}
	if !eqsX(got, want) {
		t.Fatalf("*Batch: %#v", got)
	}
	// Empty batches, top level and nested.
	b, err = Encode([]Record{Batch{}, Batch{Records: []Record{Batch{}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err = Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	top := got[0].(Batch)
	nested := got[1].(Batch).Records[0].(Batch)
	if top.Records == nil || len(top.Records) != 0 || nested.Records == nil || len(nested.Records) != 0 {
		t.Fatalf("empty batches: %#v", got)
	}
	// No aliasing of the input.
	b, _ = Encode([]Record{Batch{Records: []Record{Batch{Records: []Record{Put{Key: "kk", Value: []byte("vv")}}}}}})
	got, err = Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = 0xEE
	}
	p := got[0].(Batch).Records[0].(Batch).Records[0].(Put)
	if p.Key != "kk" || string(p.Value) != "vv" {
		t.Fatal("decoded records alias input")
	}
}

// --- fuzz ---

func FuzzDecodeV2(f *testing.F) {
	f.Add(v1Fixture())
	f.Add([]byte{2})
	f.Add(streamX(2, batchX(2, putX("a", "1"), delX("b"))))
	f.Add(streamX(2, nestFrameX(8, delX("x"))))
	f.Add(streamX(2, nestFrameX(9, delX("x"))))
	f.Add(streamX(2, batchX(2, delX("a"))))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := Decode(b)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) || got != nil {
				t.Fatalf("bad error result: %v %#v", err, got)
			}
			return
		}
		if got == nil || !noNilX(got) {
			t.Fatalf("nil in success result: %#v", got)
		}
		enc, err := Encode(got)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		want := append([]byte(nil), b...)
		want[0] = 2
		if !bytes.Equal(enc, want) {
			t.Fatalf("non-canonical accept: %x -> %x", b, enc)
		}
		again, err := Decode(enc)
		if err != nil || !eqsX(again, got) {
			t.Fatalf("unstable: %v", err)
		}
	})
}

func genX(r *rand.Rand, depth int) Record {
	switch k := r.Intn(4); {
	case k == 0:
		v := make([]byte, r.Intn(5))
		r.Read(v)
		return Put{Key: string(rune('a' + r.Intn(26))), Value: v}
	case k == 1 || depth >= maxDepth:
		return Delete{Key: string(make([]byte, r.Intn(3)))}
	default:
		n := r.Intn(4)
		rs := make([]Record, n)
		for i := range rs {
			rs[i] = genX(r, depth+1)
		}
		return Batch{Records: rs}
	}
}

func FuzzRoundTripV2(f *testing.F) {
	f.Add(int64(1))
	f.Add(int64(42))
	f.Fuzz(func(t *testing.T, seed int64) {
		r := rand.New(rand.NewSource(seed))
		recs := make([]Record, r.Intn(6))
		for i := range recs {
			recs[i] = genX(r, 0)
		}
		b, err := Encode(recs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			if got == nil || len(got) != 0 {
				t.Fatal("empty")
			}
			return
		}
		norm, _ := Decode(b)
		if !eqsX(got, norm) {
			t.Fatal("mismatch")
		}
		// Structural compare against input (nil Value == empty Value).
		if !recsEq2(got, recs) {
			t.Fatalf("round trip: %#v vs %#v", got, recs)
		}
	})
}
