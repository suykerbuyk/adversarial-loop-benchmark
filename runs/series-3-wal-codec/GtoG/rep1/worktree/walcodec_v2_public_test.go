package walcodec

// v2 PUBLIC acceptance suite (Phase 2). Shipped at phase 2 and run ALONGSIDE the
// v1 public suite (walcodec_public_test.go) in the same walcodec package; its
// helpers therefore carry a distinct "2" suffix to avoid colliding with the v1
// file. References the full v2 surface (Put, Delete, Batch, ...).

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// --- helpers (v2 file owns these "2"-suffixed names) ---

func be2(x uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, x)
	return out
}

func mkFrame2(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(be2(uint32(len(tp))), tp...)
	return append(out, be2(crc32.ChecksumIEEE(tp))...)
}

func putPL2(key string, val []byte) []byte {
	out := append(be2(uint32(len(key))), key...)
	out = append(out, be2(uint32(len(val)))...)
	return append(out, val...)
}

func batchPL2(count uint32, frames ...[]byte) []byte {
	out := be2(count)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func recEq2(a, b Record) bool {
	switch x := a.(type) {
	case Put:
		y, ok := b.(Put)
		return ok && x.Key == y.Key && string(x.Value) == string(y.Value)
	case Delete:
		y, ok := b.(Delete)
		return ok && x.Key == y.Key
	case Batch:
		y, ok := b.(Batch)
		return ok && recsEq2(x.Records, y.Records)
	default:
		return false
	}
}

func recsEq2(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !recEq2(a[i], b[i]) {
			return false
		}
	}
	return true
}

// v1Fixture is a FIXED version-1 byte stream (generated once by a v1 encoder),
// encoding Put{"alpha","one"} then Delete{"beta"}. It is a literal, NOT a replay
// of this package's Encode (which writes version 2). A v2 decoder must accept it.
func v1Fixture() []byte {
	return []byte{
		0x01, 0x00, 0x00, 0x00, 0x11, 0x01, 0x00, 0x00, 0x00, 0x05, 0x61, 0x6c,
		0x70, 0x68, 0x61, 0x00, 0x00, 0x00, 0x03, 0x6f, 0x6e, 0x65, 0x20, 0x74,
		0x5b, 0x79, 0x00, 0x00, 0x00, 0x09, 0x02, 0x00, 0x00, 0x00, 0x04, 0x62,
		0x65, 0x74, 0x61, 0x93, 0xaa, 0x41, 0x97,
	}
}

func v1FixtureRecords() []Record {
	return []Record{
		Put{Key: "alpha", Value: []byte("one")},
		Delete{Key: "beta"},
	}
}

// --- cases ---

// 1. A Batch of Put/Delete round-trips.
func TestBatchRoundTrip(t *testing.T) {
	recs := []Record{
		Batch{Records: []Record{
			Put{Key: "a", Value: []byte("1")},
			Delete{Key: "b"},
		}},
	}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEq2(got, recs) {
		t.Fatalf("got %#v want %#v", got, recs)
	}
}

// 2. A nested Batch (Batch-in-Batch) round-trips.
func TestNestedBatchRoundTrip(t *testing.T) {
	recs := []Record{
		Batch{Records: []Record{
			Batch{Records: []Record{
				Put{Key: "inner", Value: []byte("x")},
			}},
			Delete{Key: "d"},
		}},
	}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEq2(got, recs) {
		t.Fatalf("got %#v want %#v", got, recs)
	}
}

// 3. A v1-encoded (version-1) stream still decodes under v2, via a fixed fixture.
func TestV1FixtureDecodesUnderV2(t *testing.T) {
	got, err := Decode(v1Fixture())
	if err != nil {
		t.Fatalf("Decode v1 fixture: %v", err)
	}
	if !recsEq2(got, v1FixtureRecords()) {
		t.Fatalf("v1 fixture decoded as %#v, want %#v", got, v1FixtureRecords())
	}
}

// 4. A mixed top-level stream round-trips.
func TestMixedTopLevelRoundTrip(t *testing.T) {
	recs := []Record{
		Put{Key: "p", Value: []byte("v")},
		Delete{Key: "d"},
		Batch{Records: []Record{
			Put{Key: "bp", Value: []byte("bv")},
			Delete{Key: "bd"},
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
	if !recsEq2(got, recs) {
		t.Fatalf("got %#v want %#v", got, recs)
	}
}

// 5. A truncated Batch payload → ErrCorrupt.
func TestTruncatedBatch(t *testing.T) {
	b, err := Encode([]Record{
		Batch{Records: []Record{
			Put{Key: "a", Value: []byte("1")},
			Delete{Key: "bb"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b[:len(b)-2]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// 6. A stream with version = 3 → ErrCorrupt.
func TestVersion3Rejected(t *testing.T) {
	b, err := Encode([]Record{Put{Key: "k", Value: []byte("v")}})
	if err != nil {
		t.Fatal(err)
	}
	b[0] = 3 // otherwise valid stream, wrong version
	if _, err := Decode(b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// 7. A Batch nested past depth 8 → ErrCorrupt. Built by hand (the encoder
// refuses to emit an over-deep stream), wrapping a Put in 9 Batch frames.
func TestBatchNestedPastDepth8(t *testing.T) {
	cur := mkFrame2(1, putPL2("deep", []byte("x"))) // innermost: a Put frame
	for i := 0; i < 9; i++ {
		cur = mkFrame2(3, batchPL2(1, cur)) // wrap once more
	}
	stream := append([]byte{2}, cur...) // version 2
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt (9 nested Batches)", err)
	}
}

// 8. The v1 decode behaviors still hold via the fixed v1-bytes fixture (not by
// replaying v1 Encode's first byte): the fixture round-trips, a crc flip in it
// is ErrCorrupt, and a truncation of it is ErrCorrupt.
func TestV1BehaviorsViaFixture(t *testing.T) {
	got, err := Decode(v1Fixture())
	if err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	if !recsEq2(got, v1FixtureRecords()) {
		t.Fatalf("fixture records mismatch: %#v", got)
	}

	flipped := v1Fixture()
	flipped[len(flipped)-1] ^= 0xff // corrupt the last frame's crc
	if _, err := Decode(flipped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("crc flip on fixture: err = %v, want ErrCorrupt", err)
	}

	truncated := v1Fixture()
	if _, err := Decode(truncated[:len(truncated)-3]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated fixture: err = %v, want ErrCorrupt", err)
	}
}
