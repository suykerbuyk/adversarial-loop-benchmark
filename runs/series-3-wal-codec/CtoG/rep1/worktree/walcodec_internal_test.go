package walcodec

// v2 internal tests and fuzz targets. Reuses the helpers from both public test
// files (beU32, mkFrame, be2, mkFrame2, putPL2, batchPL2, recEq2, recsEq2,
// v1Fixture, v1FixtureRecords) and declares none of them.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- local helpers (names distinct from the public files) ---

func v2Stream(frames ...[]byte) []byte {
	out := []byte{version2}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func wantCorrupt(t *testing.T, name string, b []byte) error {
	t.Helper()
	got, err := Decode(b)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%s: err = %v, want ErrCorrupt", name, err)
	}
	if got != nil {
		t.Fatalf("%s: got non-nil slice %#v on error", name, got)
	}
	return err
}

// nest wraps r in k Batches, each holding exactly one record.
func nest(r Record, k int) Record {
	for i := 0; i < k; i++ {
		r = Batch{Records: []Record{r}}
	}
	return r
}

// nestBytes wraps frame in k hand-built Batch frames with valid crcs.
func nestBytes(frame []byte, k int) []byte {
	for i := 0; i < k; i++ {
		frame = mkFrame2(3, batchPL2(1, frame))
	}
	return frame
}

// --- golden bytes ---

func TestGoldenPutDeleteMatchesV1FixtureWithVersion2(t *testing.T) {
	got, err := Encode(v1FixtureRecords())
	if err != nil {
		t.Fatal(err)
	}
	want := v1Fixture()
	want[0] = 2
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestGoldenBatchOfDelete(t *testing.T) {
	got, err := Encode([]Record{Batch{Records: []Record{Delete{}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := v2Stream(mkFrame2(3, batchPL2(1, mkFrame2(2, be2(0)))))
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	if l := binary.BigEndian.Uint32(got[1:]); l != 18 {
		t.Fatalf("outer len = %d, want 18", l)
	}
	if len(got) != 1+26 {
		t.Fatalf("stream len = %d, want 27", len(got))
	}
}

func TestGoldenTwoLevelNested(t *testing.T) {
	recs := []Record{Batch{Records: []Record{
		Put{Key: "k", Value: []byte("v")},
		Batch{Records: []Record{Delete{Key: "d"}}},
	}}}
	got, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	inner := mkFrame2(3, batchPL2(1, mkFrame2(2, append(be2(1), 'd'))))
	want := v2Stream(mkFrame2(3, batchPL2(2, mkFrame2(1, putPL2("k", []byte("v"))), inner)))
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

// --- versions ---

func TestAllVersionBytes(t *testing.T) {
	base := v2Stream(mkFrame2(1, putPL2("k", []byte("v"))))
	for v := 0; v < 256; v++ {
		b := append([]byte(nil), base...)
		b[0] = byte(v)
		_, err := Decode(b)
		if v == 1 || v == 2 {
			if err != nil {
				t.Fatalf("version %d rejected: %v", v, err)
			}
		} else if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("version %d: err = %v, want ErrCorrupt", v, err)
		}
	}
}

func TestVersionOnlyStreams(t *testing.T) {
	for _, v := range []byte{1, 2} {
		got, err := Decode([]byte{v})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("version %d only: got %#v, %v", v, got, err)
		}
	}
}

func TestBatchInVersion1Rejected(t *testing.T) {
	b := append([]byte{1}, mkFrame2(3, batchPL2(0))...)
	wantCorrupt(t, "batch in v1", b)
	b[0] = 2
	if _, err := Decode(b); err != nil {
		t.Fatalf("same frame in v2: %v", err)
	}
}

// --- depth ---

func TestDepth8Accepted(t *testing.T) {
	recs := []Record{nest(Put{Key: "deep", Value: []byte("x")}, 8)}
	b, err := Encode(recs)
	if err != nil {
		t.Fatalf("Encode 8 nested: %v", err)
	}
	got, err := Decode(b)
	if err != nil || !recsEq2(got, recs) {
		t.Fatalf("round-trip 8 nested: %#v, %v", got, err)
	}
	hand := v2Stream(nestBytes(mkFrame2(1, putPL2("deep", []byte("x"))), 8))
	if !bytes.Equal(hand, b) {
		t.Fatalf("hand-built 8-deep differs from Encode")
	}
	// Eight nested Batches whose innermost is empty: the empty Batch's
	// sub-stream sits at depth 8, which is legal.
	if _, err := Decode(v2Stream(nestBytes(mkFrame2(3, batchPL2(0)), 7))); err != nil {
		t.Fatalf("8 nested with empty innermost: %v", err)
	}
}

func TestDepth9Rejected(t *testing.T) {
	recs := []Record{nest(Put{Key: "deep", Value: []byte("x")}, 9)}
	b, err := Encode(recs)
	if err == nil || b != nil {
		t.Fatalf("Encode 9 nested: b=%v err=%v", b != nil, err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("Encode error wraps ErrCorrupt: %v", err)
	}
	if _, err := Encode([]Record{nest(Batch{}, 8)}); err == nil {
		t.Fatal("Encode of 9 nested with empty innermost succeeded")
	}

	stream := v2Stream(nestBytes(mkFrame2(1, putPL2("deep", []byte("x"))), 9))
	err = wantCorrupt(t, "9 nested", stream)
	// The 9th Batch frame from the outside sits after 1 version byte and
	// 8 × (len+type+n) = 9-byte prefixes.
	off := 1 + 8*9
	for _, s := range []string{fmt.Sprintf("at offset %d", off), "(depth 8)"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("message %q lacks %q", err, s)
		}
	}
}

// --- batch framing ---

func TestBatchFramingErrors(t *testing.T) {
	del := mkFrame2(2, be2(0))
	put := mkFrame2(1, putPL2("k", []byte("v")))
	big := beU32(16<<20 + 1)
	cases := map[string][]byte{
		"n larger than frames":   mkFrame2(3, batchPL2(2, del)),
		"n smaller than frames":  mkFrame2(3, batchPL2(1, del, del)),
		"trailing garbage":       mkFrame2(3, append(batchPL2(1, del), 0)),
		"payload shorter than 4": mkFrame2(3, []byte{0, 0, 1}),
		"empty payload":          mkFrame2(3, nil),
		"inner len over cap":     mkFrame2(3, batchPL2(1, append(append(big, 1), make([]byte, 12)...))),
		"inner crc flip": func() []byte {
			f := append([]byte(nil), del...)
			f[len(f)-1] ^= 1
			return mkFrame2(3, batchPL2(1, f))
		}(),
		"inner unknown type":  mkFrame2(3, batchPL2(1, mkFrame2(9, be2(0)))),
		"inner len zero":      mkFrame2(3, batchPL2(1, append(be2(0), make([]byte, 9)...))),
		"malformed inner put": mkFrame2(3, batchPL2(1, mkFrame2(1, be2(0)))),
		"put trailing":        mkFrame2(3, batchPL2(1, mkFrame2(1, append(putPL2("", nil), 0)))),
		"count n=0 w/ frame":  mkFrame2(3, batchPL2(0, put)),
	}
	for name, f := range cases {
		wantCorrupt(t, name, v2Stream(f))
	}
}

// An inner len exactly one byte past its enclosing payload, while a sibling
// frame follows in the outer stream, is truncation, not a read into the sibling.
func TestInnerLenPastPayloadNoReadIntoSibling(t *testing.T) {
	inner := mkFrame2(2, be2(0))
	binary.BigEndian.PutUint32(inner, 6) // claims one more byte than present
	stream := v2Stream(mkFrame2(3, batchPL2(1, inner)), mkFrame2(2, be2(0)))
	err := wantCorrupt(t, "inner len past payload", stream)
	off := 1 + 4 + 1 + 4 // inner frame's absolute offset
	for _, s := range []string{"truncated frame", fmt.Sprintf("at offset %d", off), "(depth 1)"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("message %q lacks %q", err, s)
		}
	}
}

// --- offsets and depth in messages ---

func TestErrorOffsetsAndDepthAtDepth2(t *testing.T) {
	del := mkFrame2(2, be2(0))
	// wrap places sub as the depth-2 sub-stream (count cnt) inside two Batches.
	wrap := func(cnt uint32, sub []byte) []byte {
		l1 := mkFrame2(3, batchPL2(cnt, sub))
		return v2Stream(mkFrame2(3, batchPL2(1, l1)))
	}
	const base = 1 + 9 + 9 // offset of the first depth-2 frame
	type tc struct {
		name, want string
		b          []byte
		off, depth int
	}
	// A Put with len 15 occupies 23 bytes; padding the region to 26 bytes
	// leaves 3, a truncated header for the second of 2 frames.
	putLen15 := mkFrame2(1, putPL2("", make([]byte, 6)))
	trunc := append([]byte(nil), del...)
	binary.BigEndian.PutUint32(trunc, 6)
	crcBad := append([]byte(nil), del...)
	crcBad[len(crcBad)-1] ^= 1
	cases := []tc{
		{"truncated header", "truncated frame header", wrap(2, append(putLen15, 0, 0, 0)), base + len(putLen15), 2},
		{"cap", "exceeds 16 MiB cap", wrap(1, append(beU32(16<<20+1), make([]byte, 9)...)), base, 2},
		{"empty", "empty frame", wrap(1, append(be2(0), make([]byte, 9)...)), base, 2},
		{"truncation", "truncated frame:", wrap(1, trunc), base, 2},
		{"crc", "crc mismatch", wrap(1, crcBad), base, 2},
		{"malformed", "malformed delete payload", wrap(1, mkFrame2(2, be2(5))), base, 2},
		{"unknown type", "unknown record type 9", wrap(1, mkFrame2(9, be2(0))), base, 2},
		{"batch count", "batch count 5", wrap(1, mkFrame2(3, be2(5))), base, 2},
		{"trailing", "trailing bytes", wrap(1, append(append([]byte(nil), del...), 7)), base + len(del), 2},
		{"trailing in depth-3", "trailing bytes", wrap(1, mkFrame2(3, append(be2(0), 7))), base + 9, 3},
	}
	for _, c := range cases {
		err := wantCorrupt(t, c.name, c.b)
		for _, s := range []string{c.want, fmt.Sprintf("at offset %d", c.off), fmt.Sprintf("(depth %d)", c.depth)} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: message %q lacks %q", c.name, err, s)
			}
		}
	}
}

// --- count bound ---

func TestHostileCountRejectedCheaply(t *testing.T) {
	b := v2Stream(mkFrame2(3, append(be2(0xFFFFFFFF), make([]byte, 20)...)))
	wantCorrupt(t, "n=max", b)
	allocs := testing.AllocsPerRun(20, func() { _, _ = Decode(b) })
	if allocs > 10 {
		t.Fatalf("hostile count: %v allocs per run", allocs)
	}
}

func TestCountBoundExactFill(t *testing.T) {
	const k = 7
	var frames [][]byte
	for i := 0; i < k; i++ {
		frames = append(frames, mkFrame2(2, be2(0)))
	}
	ok := v2Stream(mkFrame2(3, batchPL2(k, frames...)))
	got, err := Decode(ok)
	if err != nil || len(got) != 1 || len(got[0].(Batch).Records) != k {
		t.Fatalf("exact fill: %#v, %v", got, err)
	}
	wantCorrupt(t, "n=k+1", v2Stream(mkFrame2(3, batchPL2(k+1, frames...))))
}

// --- truncation ---

func TestEveryPrefixOfNestedCorrupt(t *testing.T) {
	b, err := Encode([]Record{
		Put{Key: "a", Value: []byte("1")},
		Batch{Records: []Record{Delete{Key: "x"}, Batch{Records: []Record{Put{Key: "y", Value: []byte("z")}, Batch{}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := 1 + 4 + 1 + 4 + 1 + 4 + 1 + 4 // end of first top-level frame
	for i := 0; i < len(b); i++ {
		if i == 1 || i == first {
			continue // frame boundaries: valid shorter streams
		}
		wantCorrupt(t, fmt.Sprintf("prefix %d", i), b[:i])
	}
}

// --- encode error contract ---

type foreign struct{}

func (foreign) Kind() uint8 { return 1 }

func checkEncodeErr(t *testing.T, name string, recs []Record) {
	t.Helper()
	b, err := Encode(recs)
	if err == nil {
		t.Fatalf("%s: Encode succeeded", name)
	}
	if b != nil {
		t.Fatalf("%s: non-nil slice on error", name)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "walcodec: ") || strings.Count(msg, "walcodec:") != 1 {
		t.Fatalf("%s: bad prefix in %q", name, msg)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("%s: Encode error wraps ErrCorrupt", name)
	}
}

func TestEncodeErrorContract(t *testing.T) {
	var nilBatch *Batch
	checkEncodeErr(t, "foreign top", []Record{foreign{}})
	checkEncodeErr(t, "nil top", []Record{nil})
	checkEncodeErr(t, "foreign depth 3", []Record{nest(foreign{}, 3)})
	checkEncodeErr(t, "nil *Batch", []Record{nilBatch})
	checkEncodeErr(t, "nil in batch", []Record{Batch{Records: []Record{Delete{}, nil}}})
	checkEncodeErr(t, "depth 9", []Record{nest(Delete{}, 9)})
	if testing.Short() {
		return
	}
	huge := make([]byte, 16<<20)
	checkEncodeErr(t, "leaf over budget", []Record{Put{Value: huge}})
	// Fits as a top-level leaf (len 16 MiB - 12) but not inside a Batch.
	checkEncodeErr(t, "batch over budget", []Record{Batch{Records: []Record{Put{Value: huge[:16<<20-21]}}}})
	many := make([]Record, (16<<20-minRecordBody)/minRecordFrame+1)
	for i := range many {
		many[i] = Delete{}
	}
	checkEncodeErr(t, "count bound", []Record{Batch{Records: many}})
	// First child leaves 12 bytes of room, so the second child's budget is
	// 4 < minRecordBody.
	first := Put{Value: huge[:16<<20-25-9]}
	_, err := Encode([]Record{Batch{Records: []Record{first, Delete{}}}})
	if err == nil || !strings.Contains(err.Error(), "batch record 1: batch exceeds frame cap") {
		t.Fatalf("child guard: %v", err)
	}
	checkEncodeErr(t, "child guard", []Record{Batch{Records: []Record{first, Delete{}}}})
	// Sanity: the first child alone fits.
	if _, err := Encode([]Record{Batch{Records: []Record{first}}}); err != nil {
		t.Fatalf("first child alone: %v", err)
	}
}

// --- cap boundary ---

// capVlen returns the Put value length that makes a Batch holding exactly one
// Put with a keyLen-byte key reach the 16 MiB cap exactly:
//
//	inner Put len    = 1 (type) + 4 (klen) + keyLen + 4 (vlen) + vlen
//	inner full frame = 4 (len) + inner len + 4 (crc; inner crcs ARE inside the outer len)
//	outer len        = 1 (type) + 4 (n) + inner full frame = 22 + keyLen + vlen
func capVlen(keyLen int) int {
	return (16 << 20) - (1 + 4 + 4 + 1 + 4 + 4 + 4) - keyLen
}

func TestCapBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 16 MiB buffers")
	}
	for _, key := range []string{"", "key"} {
		v := make([]byte, capVlen(len(key))+1)
		recs := []Record{Batch{Records: []Record{Put{Key: key, Value: v[:len(v)-1]}}}}
		b, err := Encode(recs)
		if err != nil {
			t.Fatalf("key %q: at cap: %v", key, err)
		}
		if l := binary.BigEndian.Uint32(b[1:]); l != 16<<20 {
			t.Fatalf("key %q: outer len = %d, want %d", key, l, 16<<20)
		}
		got, err := Decode(b)
		if err != nil || !recsEq2(got, recs) {
			t.Fatalf("key %q: round-trip at cap: %v", key, err)
		}
		over, err := Encode([]Record{Batch{Records: []Record{Put{Key: key, Value: v}}}})
		if err == nil || over != nil {
			t.Fatalf("key %q: cap+1 encoded", key)
		}
		// Same bytes with outer len bumped to cap+1 (crc does not matter: the
		// cap check comes first).
		binary.BigEndian.PutUint32(b[1:], 16<<20+1)
		wantCorrupt(t, "outer len cap+1", b)
	}
}

// --- bounded work ---

func TestCyclesError(t *testing.T) {
	p := &Batch{}
	p.Records = []Record{p}
	checkEncodeErr(t, "pointer cycle", []Record{p})
	s := make([]Record, 1)
	s[0] = Batch{Records: s}
	checkEncodeErr(t, "slice cycle", []Record{s[0]})
}

// fanTree builds 8 Batch levels; each level shares one Records slice of
// fanout aliased children, and the level-8 nodes are leaf (Batch{} by default).
func fanTree(fanout int, leaf Record) Record {
	cur := leaf
	for lvl := 0; lvl < 7; lvl++ {
		s := make([]Record, fanout)
		for i := range s {
			s[i] = cur
		}
		cur = Batch{Records: s}
	}
	return cur
}

func boundedEncodeFail(t *testing.T, name string, r Record) {
	t.Helper()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	start := time.Now()
	b, err := Encode([]Record{r})
	el := time.Since(start)
	runtime.ReadMemStats(&m1)
	if err == nil || b != nil {
		t.Fatalf("%s: encode succeeded", name)
	}
	if !strings.Contains(err.Error(), "frame cap") {
		t.Fatalf("%s: want cap error, got %v", name, err)
	}
	if el > 2*time.Second {
		t.Fatalf("%s: took %v", name, el)
	}
	if d := m1.TotalAlloc - m0.TotalAlloc; d > 64<<20 {
		t.Fatalf("%s: allocated %d bytes", name, d)
	}
}

func TestEmptyNodeFanOutBounded(t *testing.T) {
	boundedEncodeFail(t, "empty fan-out 16", fanTree(16, Batch{}))
}

func TestEmptyNodeFanOutSuccessControl(t *testing.T) {
	r := fanTree(5, Batch{})
	b, err := Encode([]Record{r})
	if err != nil {
		t.Fatal(err)
	}
	nodes := (390625 - 1) / 4 // (5^8 - 1) / 4
	if len(b) != 1+13*nodes {
		t.Fatalf("len = %d, want %d", len(b), 1+13*nodes)
	}
	got, err := Decode(b)
	if err != nil || !recsEq2(got, []Record{r}) {
		t.Fatalf("round-trip: %v", err)
	}
}

func TestLargeLeafFanOutBounded(t *testing.T) {
	leaf := Batch{Records: []Record{Put{Key: "k", Value: make([]byte, 64<<10)}}}
	boundedEncodeFail(t, "large-leaf fan-out 16", fanTree(16, leaf))
}

// --- aliasing and empties ---

func TestDecodeDoesNotAlias(t *testing.T) {
	b, err := Encode([]Record{Batch{Records: []Record{
		Put{Key: "key", Value: []byte("val")},
		Batch{Records: []Record{Delete{Key: "del"}}},
	}}})
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
	want := []Record{Batch{Records: []Record{
		Put{Key: "key", Value: []byte("val")},
		Batch{Records: []Record{Delete{Key: "del"}}},
	}}}
	if !recsEq2(got, want) {
		t.Fatalf("decoded values changed: %#v", got)
	}
}

func TestEmptyBatchesAndPointers(t *testing.T) {
	for _, r := range []Record{Batch{}, Batch{Records: nil}, Batch{Records: []Record{}}, &Batch{}} {
		b, err := Encode([]Record{r})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil || len(got) != 1 {
			t.Fatalf("%#v: %#v, %v", r, got, err)
		}
		if bt := got[0].(Batch); bt.Records == nil || len(bt.Records) != 0 {
			t.Fatalf("decoded Records = %#v, want non-nil empty", bt.Records)
		}
	}
	b, err := Encode([]Record{&Batch{Records: []Record{&Put{Key: "a"}, &Delete{Key: "b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	want := []Record{Batch{Records: []Record{Put{Key: "a"}, Delete{Key: "b"}}}}
	if err != nil || !recsEq2(got, want) {
		t.Fatalf("pointers: %#v, %v", got, err)
	}
}

// --- fuzz ---

func FuzzDecode(f *testing.F) {
	f.Add(v1Fixture())
	f.Add(v2Stream(mkFrame2(3, batchPL2(1, mkFrame2(2, be2(0))))))
	f.Add(v2Stream(nestBytes(mkFrame2(1, putPL2("deep", []byte("x"))), 9)))
	f.Add(v2Stream(nestBytes(mkFrame2(1, putPL2("deep", []byte("x"))), 8)))
	f.Add([]byte{2})
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) || recs != nil {
				t.Fatalf("bad error contract: %v %v", recs, err)
			}
			return
		}
		enc, err := Encode(recs)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(enc[1:], b[1:]) {
			t.Fatalf("not canonical:\n in %x\nout %x", b, enc)
		}
		if b[0] == 2 && !bytes.Equal(enc, b) {
			t.Fatalf("v2 not canonical")
		}
	})
}

// genRecs builds a record tree from fuzz bytes, up to depth 8.
func genRecs(data []byte, depth int) ([]Record, []byte) {
	var out []Record
	for len(data) > 0 {
		op := data[0]
		data = data[1:]
		switch op % 4 {
		case 0:
			return out, data // end of this list
		case 1:
			n := int(op>>2) % 8
			if n > len(data) {
				n = len(data)
			}
			out = append(out, Put{Key: string(data[:n/2]), Value: append([]byte(nil), data[n/2:n]...)})
			data = data[n:]
		case 2:
			n := int(op>>2) % 5
			if n > len(data) {
				n = len(data)
			}
			out = append(out, Delete{Key: string(data[:n])})
			data = data[n:]
		case 3:
			if depth >= maxDepth {
				continue
			}
			var sub []Record
			sub, data = genRecs(data, depth+1)
			out = append(out, Batch{Records: sub})
		}
	}
	return out, data
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte{1, 'a', 'b', 2, 'c', 3, 1, 0, 3, 3, 3, 3, 3, 3, 3, 3, 9})
	f.Fuzz(func(t *testing.T, data []byte) {
		recs, _ := genRecs(data, 0)
		b, err := Encode(recs)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := Decode(b)
		if err != nil || !recsEq2(got, recs) {
			t.Fatalf("round-trip: %v", err)
		}
	})
}
