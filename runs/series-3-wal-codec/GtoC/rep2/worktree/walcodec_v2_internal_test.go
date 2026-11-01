package walcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func privBe(x uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, x)
	return out
}

func privMkFrame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(privBe(uint32(len(tp))), tp...)
	return append(out, privBe(crc32.ChecksumIEEE(tp))...)
}

func privPutPL(key string, val []byte) []byte {
	out := append(privBe(uint32(len(key))), key...)
	out = append(out, privBe(uint32(len(val)))...)
	return append(out, val...)
}

func privDelPL(key string) []byte {
	return append(privBe(uint32(len(key))), key...)
}

func privBatchPL(count uint32, frames ...[]byte) []byte {
	out := privBe(count)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func privRecEq(a, b Record) bool {
	switch x := a.(type) {
	case Put:
		y, ok := b.(Put)
		return ok && x.Key == y.Key && bytes.Equal(x.Value, y.Value)
	case Delete:
		y, ok := b.(Delete)
		return ok && x.Key == y.Key
	case Batch:
		y, ok := b.(Batch)
		return ok && privRecsEq(x.Records, y.Records)
	default:
		return false
	}
}

func privRecsEq(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !privRecEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

func privV1Fixture() []byte {
	return []byte{
		0x01, 0x00, 0x00, 0x00, 0x11, 0x01, 0x00, 0x00, 0x00, 0x05, 0x61, 0x6c,
		0x70, 0x68, 0x61, 0x00, 0x00, 0x00, 0x03, 0x6f, 0x6e, 0x65, 0x20, 0x74,
		0x5b, 0x79, 0x00, 0x00, 0x00, 0x09, 0x02, 0x00, 0x00, 0x00, 0x04, 0x62,
		0x65, 0x74, 0x61, 0x93, 0xaa, 0x41, 0x97,
	}
}

func privMustDecode(t *testing.T, b []byte, want []Record) {
	t.Helper()
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !privRecsEq(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestPrivBatchGoldenBytes(t *testing.T) {
	putP := privMkFrame(1, privPutPL("p", []byte("v")))
	innerPut := privMkFrame(1, privPutPL("a", []byte("1")))
	innerDel := privMkFrame(2, privDelPL("b"))
	nestedPut := privMkFrame(1, privPutPL("c", []byte("d")))
	nested := privMkFrame(3, privBatchPL(1, nestedPut))
	batch := privMkFrame(3, privBatchPL(3, innerPut, innerDel, nested))
	empty := privMkFrame(3, privBatchPL(0))
	hand := append([]byte{2}, putP...)
	hand = append(hand, batch...)
	hand = append(hand, empty...)

	recs := []Record{
		Put{Key: "p", Value: []byte("v")},
		Batch{Records: []Record{
			Put{Key: "a", Value: []byte("1")},
			Delete{Key: "b"},
			Batch{Records: []Record{
				Put{Key: "c", Value: []byte("d")},
			}},
		}},
		Batch{Records: []Record{}},
	}
	privMustDecode(t, hand, recs)
	got, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, hand) {
		t.Fatalf("Encode bytes mismatch\ngot  %x\nwant %x", got, hand)
	}
}

func TestPrivDepth8AcceptsHandBuilt(t *testing.T) {
	cur := privMkFrame(1, privPutPL("deep", []byte("x")))
	for i := 0; i < 8; i++ {
		cur = privMkFrame(3, privBatchPL(1, cur))
	}
	stream := append([]byte{2}, cur...)
	want := privWrapBatches(8, Put{Key: "deep", Value: []byte("x")})
	privMustDecode(t, stream, []Record{want})
}

func privWrapBatches(n int, inner Record) Record {
	cur := inner
	for i := 0; i < n; i++ {
		cur = Batch{Records: []Record{cur}}
	}
	return cur
}

func TestPrivDepth9EncodeRefused(t *testing.T) {
	eight, err := Encode([]Record{privWrapBatches(8, Put{Key: "deep", Value: []byte("x")})})
	if err != nil {
		t.Fatalf("8 wraps: %v", err)
	}
	if _, err := Decode(eight); err != nil {
		t.Fatalf("decode 8 wraps: %v", err)
	}
	_, err = Encode([]Record{privWrapBatches(9, Put{Key: "deep", Value: []byte("x")})})
	if err != ErrCorrupt || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want bare ErrCorrupt", err)
	}
}

func TestPrivEmptyBatchAtDepth9(t *testing.T) {
	// One empty Batch frame, wrapped 7 more times: 8 Batch frames.
	// The innermost sub-stream sits at depth 8. The value is 7 wraps
	// around that empty Batch.
	cur := privMkFrame(3, privBatchPL(0))
	for i := 0; i < 7; i++ {
		cur = privMkFrame(3, privBatchPL(1, cur))
	}
	eight := append([]byte{2}, cur...)
	want := privWrapBatches(7, Batch{Records: []Record{}})
	privMustDecode(t, eight, []Record{want})

	nine := append([]byte{2}, privMkFrame(3, privBatchPL(1, cur))...)
	if _, err := Decode(nine); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("depth-9 empty: err = %v, want ErrCorrupt", err)
	}

	_, err := Encode([]Record{privWrapBatches(8, Batch{Records: []Record{}})})
	if err != ErrCorrupt || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("encode empty at depth 9: err = %v, want bare ErrCorrupt", err)
	}

	s := make([]Record, 1)
	s[0] = Batch{Records: s}
	_, err = Encode(s)
	if err != ErrCorrupt || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("self-ref encode: err = %v, want bare ErrCorrupt", err)
	}
}

func TestPrivInnerLenDoesNotSpill(t *testing.T) {
	inner := privMkFrame(1, privPutPL("k", []byte("v")))
	batchPayload := privBatchPL(1, inner)
	outer := privMkFrame(3, batchPayload)
	base := append([]byte{2}, outer...)
	privMustDecode(t, base, []Record{
		Batch{Records: []Record{Put{Key: "k", Value: []byte("v")}}},
	})

	// Outer CRC stays valid. Extra bytes follow the outer frame. The inner
	// frame is sized to consume exactly the bytes left in the outer stream,
	// which reaches through the outer CRC and the extra bytes, past the
	// Batch payload.
	extra := []byte{0x11, 0x22, 0x33, 0x44}
	content := []byte{kindPut} // type byte only; the rest of the claim is outside
	innerLen := uint32(len(content) + len(extra))
	payload := append(privBe(1), privBe(innerLen)...)
	payload = append(payload, content...)
	stream := append([]byte{2}, privMkFrame(3, payload)...)
	stream = append(stream, extra...)
	innerOff := 1 + u32Size + 1 + u32Size // version, outer len, type, n
	if int(innerLen)+u32Size+crcSize != len(stream)-innerOff {
		t.Fatalf("setup: inner frame %d, bytes left %d", int(innerLen)+u32Size+crcSize, len(stream)-innerOff)
	}
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("spill: err = %v, want ErrCorrupt", err)
	}
}

// TestPrivInnerLenWhiteBox is the regression for a decoder that bounds an
// inner frame by the backing array (cap) instead of the Batch payload (len).
// The bytes past the payload complete a valid Put frame, so CRC mismatch and
// a short read cannot reject it. Only the payload bound can.
func TestPrivInnerLenWhiteBox(t *testing.T) {
	val := []byte{0x09, 0x09, 0x09, 0x09}
	// Put body that will be split: type, klen=0, vlen=4, then the 4 value
	// bytes live past the payload cut. A correct inner CRC follows them.
	putHead := []byte{kindPut}
	putHead = append(putHead, privBe(0)...)
	putHead = append(putHead, privBe(uint32(len(val)))...)
	innerBody := append(append([]byte{}, putHead...), val...)
	innerCRC := privBe(crc32.ChecksumIEEE(innerBody))
	innerLen := uint32(len(innerBody))

	// Batch payload claims n = 1 and then only the inner len prefix plus the
	// Put header. The value bytes and the inner CRC sit after the cut, still
	// inside the same backing array.
	prefix := append(privBe(1), privBe(innerLen)...)
	prefix = append(prefix, putHead...)
	buf := append(append([]byte{}, prefix...), val...)
	buf = append(buf, innerCRC...)
	k := len(prefix)
	if k >= len(buf) {
		t.Fatalf("setup: cut %d is not inside buf len %d", k, len(buf))
	}

	// Control: the same bytes, consumed in full, are a legal Batch of one Put.
	full, err := decodeBatchPayload(buf, kindBit(kindPut)|kindBit(kindDelete)|kindBit(kindBatch), 0)
	if err != nil {
		t.Fatalf("control decodeBatchPayload: %v", err)
	}
	want := Batch{Records: []Record{Put{Key: "", Value: val}}}
	if !privRecEq(full, want) {
		t.Fatalf("control got %#v want %#v", full, want)
	}

	// Defect: the payload ends at k. The completion is still in cap(buf[:k]).
	cut := buf[:k]
	if cap(cut) < len(buf) {
		t.Fatal("setup: cut does not share the completion in its cap")
	}
	if _, err := decodeBatchPayload(cut, kindBit(kindPut)|kindBit(kindDelete)|kindBit(kindBatch), 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("white-box spill: err = %v, want ErrCorrupt", err)
	}

	// End-to-end form of the same split. The outer frame's len stops at the
	// Put header. The value bytes and a correct inner CRC follow that frame,
	// so they sit in the stream backing array past the payload. The outer CRC
	// is valid and does not cover those bytes. Decode must not panic, and
	// only the payload bound rejects the stream.
	outerBody := append([]byte{kindBatch}, prefix...)
	outer := append(privBe(uint32(len(outerBody))), outerBody...)
	outer = append(outer, privBe(crc32.ChecksumIEEE(outerBody))...)
	stream := append([]byte{2}, outer...)
	stream = append(stream, val...)
	stream = append(stream, innerCRC...)
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("end-to-end spill: err = %v, want ErrCorrupt", err)
	}
}

func TestPrivVersion1RejectsBatch(t *testing.T) {
	frame := privMkFrame(3, privBatchPL(0))
	v2 := append([]byte{2}, frame...)
	privMustDecode(t, v2, []Record{Batch{Records: []Record{}}})
	v1 := append([]byte{1}, frame...)
	if _, err := Decode(v1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("v1 batch: err = %v, want ErrCorrupt", err)
	}
}

func TestPrivVersion0Rejected(t *testing.T) {
	if _, err := Decode([]byte{0}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("version 0: err = %v, want ErrCorrupt", err)
	}
	frame := privMkFrame(1, privPutPL("k", []byte("v")))
	v2 := append([]byte{2}, frame...)
	privMustDecode(t, v2, []Record{Put{Key: "k", Value: []byte("v")}})
	v0 := append([]byte{0}, frame...)
	if _, err := Decode(v0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("version 0 put: err = %v, want ErrCorrupt", err)
	}
}

func TestPrivEncodeVersionIs2(t *testing.T) {
	b, err := Encode([]Record{Put{Key: "k", Value: []byte("v")}})
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 2 {
		t.Fatalf("put encode version = %d, want 2", b[0])
	}
	empty, err := Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 1 || empty[0] != 2 {
		t.Fatalf("empty encode = %v, want [2]", empty)
	}
}

func TestPrivBatchNTooLarge(t *testing.T) {
	inner := privMkFrame(1, privPutPL("a", []byte("1")))
	base := append([]byte{2}, privMkFrame(3, privBatchPL(1, inner))...)
	privMustDecode(t, base, []Record{
		Batch{Records: []Record{Put{Key: "a", Value: []byte("1")}}},
	})
	bad := append([]byte{2}, privMkFrame(3, privBatchPL(2, inner))...)
	if _, err := Decode(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("n too large: err = %v, want ErrCorrupt", err)
	}
}

func TestPrivBatchNTrailingBytes(t *testing.T) {
	inner := privMkFrame(1, privPutPL("a", []byte("1")))
	extra := privMkFrame(2, privDelPL("z"))
	payload := privBatchPL(2, inner, extra)
	base := append([]byte{2}, privMkFrame(3, payload)...)
	privMustDecode(t, base, []Record{
		Batch{Records: []Record{
			Put{Key: "a", Value: []byte("1")},
			Delete{Key: "z"},
		}},
	})
	// Same bytes, n reduced by one, so the second frame is trailing.
	badPL := append(privBe(1), payload[4:]...)
	bad := append([]byte{2}, privMkFrame(3, badPL)...)
	if _, err := Decode(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing: err = %v, want ErrCorrupt", err)
	}
}

func TestPrivBatchNMaxUint32(t *testing.T) {
	frame := privMkFrame(3, privBe(0xFFFFFFFF))
	stream := append([]byte{2}, frame...)
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("n max: err = %v, want ErrCorrupt", err)
	}
}

func TestPrivVersionOnlyStreams(t *testing.T) {
	for _, v := range []byte{1, 2} {
		got, err := Decode([]byte{v})
		if err != nil {
			t.Fatalf("version %d: %v", v, err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("version %d: got %#v, want empty non-nil", v, got)
		}
	}
}

func TestPrivEmptyBatchRoundTrip(t *testing.T) {
	for _, recs := range [][]Record{
		{Batch{Records: nil}},
		{Batch{Records: []Record{}}},
	} {
		b, err := Encode(recs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d", len(got))
		}
		batch, ok := got[0].(Batch)
		if !ok || batch.Records == nil || len(batch.Records) != 0 {
			t.Fatalf("got %#v, want empty non-nil Records", got[0])
		}
	}
}

func TestPrivNilValueInBatch(t *testing.T) {
	nilB, err := Encode([]Record{Batch{Records: []Record{Put{Key: "k", Value: nil}}}})
	if err != nil {
		t.Fatal(err)
	}
	emptyB, err := Encode([]Record{Batch{Records: []Record{Put{Key: "k", Value: []byte{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	gotNil, err := Decode(nilB)
	if err != nil {
		t.Fatal(err)
	}
	gotEmpty, err := Decode(emptyB)
	if err != nil {
		t.Fatal(err)
	}
	if !privRecsEq(gotNil, gotEmpty) {
		t.Fatalf("nil and empty differ: %#v vs %#v", gotNil, gotEmpty)
	}
	inner := gotNil[0].(Batch).Records[0].(Put)
	if len(inner.Value) != 0 {
		t.Fatalf("value len = %d", len(inner.Value))
	}
}

func TestPrivDupKeysInBatch(t *testing.T) {
	recs := []Record{
		Batch{Records: []Record{
			Put{Key: "dup", Value: []byte("1")},
			Put{Key: "dup", Value: []byte("2")},
		}},
		Put{Key: "dup", Value: []byte("3")},
	}
	b, err := Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !privRecsEq(got, recs) {
		t.Fatalf("got %#v want %#v", got, recs)
	}
}

func TestPrivTooLargeIsNotCorrupt(t *testing.T) {
	// Oversized body is errTooLarge, not ErrCorrupt, and Encode returns no bytes.
	// The size check lives in putBodyLen, which runs before appendPutPayload.
	// This test does not claim to observe that ordering: a check moved after
	// the copy would still return the same error after copying the value.
	val := make([]byte, maxFrameLen+1)
	got, err := Encode([]Record{Put{Key: "k", Value: val}})
	if err == nil {
		t.Fatal("expected error")
	}
	if got != nil {
		t.Fatalf("Encode returned %d bytes with an error", len(got))
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized put matched ErrCorrupt: %v", err)
	}
}

func TestPrivKindValues(t *testing.T) {
	var put Put
	var del Delete
	var batch Batch
	if put.Kind() != 1 || del.Kind() != 2 || batch.Kind() != 3 {
		t.Fatalf("kinds = %d %d %d, want 1 2 3", put.Kind(), del.Kind(), batch.Kind())
	}
}

func TestPrivV1FixtureStillDecodes(t *testing.T) {
	got, err := Decode(privV1Fixture())
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		Put{Key: "alpha", Value: []byte("one")},
		Delete{Key: "beta"},
	}
	if !privRecsEq(got, want) {
		t.Fatalf("got %#v", got)
	}
	flipped := privV1Fixture()
	flipped[len(flipped)-1] ^= 0xff
	if _, err := Decode(flipped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("crc flip: err = %v, want ErrCorrupt", err)
	}
	truncated := privV1Fixture()
	if _, err := Decode(truncated[:len(truncated)-3]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated: err = %v, want ErrCorrupt", err)
	}
}
