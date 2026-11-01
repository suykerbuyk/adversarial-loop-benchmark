package walcodec

// v1 PUBLIC acceptance suite (Phase 1). Shipped with the task; the implementor
// sees these. Package walcodec. References only the v1 surface (Put, Delete,
// Record, Encode, Decode, ErrCorrupt) so it compiles and runs against a v1
// implementation, and — unchanged — serves as the v1 regression suite against a
// v2 implementation (every case is version-agnostic or uses a version-1 fixture
// that a v2 decoder must still accept; no case asserts the written version byte
// is 1).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// --- helpers (v1 file owns these names) ---

func beU32(x uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, x)
	return out
}

// mkFrame builds a [len][type][payload][crc] frame with a correct crc.
func mkFrame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(beU32(uint32(len(tp))), tp...)
	return append(out, beU32(crc32.ChecksumIEEE(tp))...)
}

func recEq(a, b Record) bool {
	switch x := a.(type) {
	case Put:
		y, ok := b.(Put)
		return ok && x.Key == y.Key && bytes.Equal(x.Value, y.Value)
	case Delete:
		y, ok := b.(Delete)
		return ok && x.Key == y.Key
	default:
		return false
	}
}

func recsEq(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !recEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

// --- cases ---

// 1. Round-trip a Put then a Delete, decoded back in order.
func TestRoundTripPutDelete(t *testing.T) {
	recs := []Record{
		Put{Key: "alpha", Value: []byte("one")},
		Delete{Key: "beta"},
	}
	b, err := Encode(recs)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !recsEq(got, recs) {
		t.Fatalf("round-trip mismatch: got %#v want %#v", got, recs)
	}
}

// 2. A flipped crc byte → ErrCorrupt.
func TestCRCFlip(t *testing.T) {
	b, err := Encode([]Record{Put{Key: "k", Value: []byte("v")}})
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff // last byte is part of the frame crc
	if _, err := Decode(b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// 3. An unknown type tag (otherwise valid frame) → ErrCorrupt.
func TestUnknownType(t *testing.T) {
	stream := append([]byte{1}, mkFrame(99, nil)...) // valid crc, unknown type 99
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// 4. A truncated frame → ErrCorrupt.
func TestTruncatedFrame(t *testing.T) {
	b, err := Encode([]Record{Put{Key: "k", Value: []byte("value")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b[:len(b)-2]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// 5. An oversize len (> 16 MiB) → ErrCorrupt, before any allocation.
func TestOversizeLen(t *testing.T) {
	stream := []byte{1}                         // version 1
	stream = append(stream, beU32(16<<20+1)...) // len = 16 MiB + 1
	stream = append(stream, 1)                  // a type byte, but far fewer bytes than len claims
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// 6. Empty recs → a version-only (one-byte) stream that decodes to empty, no error.
func TestEmptyRecsVersionOnly(t *testing.T) {
	b, err := Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 {
		t.Fatalf("empty Encode len = %d, want 1 (version byte only)", len(b))
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode version-only: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("decoded %d records, want 0", len(got))
	}
}

// 7. Decode(nil) and Decode(empty) → ErrCorrupt (missing version byte).
func TestDecodeNilEmpty(t *testing.T) {
	if _, err := Decode(nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Decode(nil) err = %v, want ErrCorrupt", err)
	}
	if _, err := Decode([]byte{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Decode(empty) err = %v, want ErrCorrupt", err)
	}
}

// 8. Duplicate keys kept, in order.
func TestDuplicateKeysKeptInOrder(t *testing.T) {
	recs := []Record{
		Put{Key: "dup", Value: []byte("1")},
		Put{Key: "dup", Value: []byte("2")},
		Delete{Key: "dup"},
	}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !recsEq(got, recs) {
		t.Fatalf("duplicates not kept in order: got %#v want %#v", got, recs)
	}
}

// 9. An empty Value and a nil Value decode equal.
func TestEmptyVsNilValue(t *testing.T) {
	bNil, err := Encode([]Record{Put{Key: "k", Value: nil}})
	if err != nil {
		t.Fatal(err)
	}
	bEmpty, err := Encode([]Record{Put{Key: "k", Value: []byte{}}})
	if err != nil {
		t.Fatal(err)
	}
	gotNil, err := Decode(bNil)
	if err != nil {
		t.Fatal(err)
	}
	gotEmpty, err := Decode(bEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotNil) != 1 || len(gotEmpty) != 1 {
		t.Fatalf("expected 1 record each, got %d and %d", len(gotNil), len(gotEmpty))
	}
	if !recEq(gotNil[0], gotEmpty[0]) {
		t.Fatalf("nil-Value and empty-Value Puts decode unequal: %#v vs %#v", gotNil[0], gotEmpty[0])
	}
	if v := gotNil[0].(Put).Value; len(v) != 0 {
		t.Fatalf("decoded nil Value has len %d, want 0", len(v))
	}
}
