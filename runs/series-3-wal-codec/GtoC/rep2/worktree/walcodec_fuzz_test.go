package walcodec

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

func privFuzzBe(x uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, x)
	return out
}

func privFuzzFrame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(privFuzzBe(uint32(len(tp))), tp...)
	return append(out, privFuzzBe(crc32.ChecksumIEEE(tp))...)
}

func privFuzzPutPL(key string, val []byte) []byte {
	out := append(privFuzzBe(uint32(len(key))), key...)
	out = append(out, privFuzzBe(uint32(len(val)))...)
	return append(out, val...)
}

func privFuzzBatchPL(count uint32, frames ...[]byte) []byte {
	out := privFuzzBe(count)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func privFuzzV1Fixture() []byte {
	return []byte{
		0x01, 0x00, 0x00, 0x00, 0x11, 0x01, 0x00, 0x00, 0x00, 0x05, 0x61, 0x6c,
		0x70, 0x68, 0x61, 0x00, 0x00, 0x00, 0x03, 0x6f, 0x6e, 0x65, 0x20, 0x74,
		0x5b, 0x79, 0x00, 0x00, 0x00, 0x09, 0x02, 0x00, 0x00, 0x00, 0x04, 0x62,
		0x65, 0x74, 0x61, 0x93, 0xaa, 0x41, 0x97,
	}
}

func privFuzzWrapped(wraps int) []byte {
	cur := privFuzzFrame(1, privFuzzPutPL("deep", []byte("x")))
	for i := 0; i < wraps; i++ {
		cur = privFuzzFrame(3, privFuzzBatchPL(1, cur))
	}
	return append([]byte{2}, cur...)
}

func privFuzzSpill() []byte {
	// Inner frame is truncated even against the outer stream: its claimed
	// length reaches into extra, and extra is not a valid CRC. A decoder that
	// ignores the payload bound still rejects this seed. TestPrivInnerLenWhiteBox
	// is the case that only the bound check can reject.
	extra := []byte{0x11, 0x22, 0x33, 0x44}
	content := []byte{1}
	innerLen := uint32(len(content) + len(extra))
	payload := append(privFuzzBe(1), privFuzzBe(innerLen)...)
	payload = append(payload, content...)
	return append(append([]byte{2}, privFuzzFrame(3, payload)...), extra...)
}

func privFuzzAdd(f *testing.F, b []byte) {
	f.Add(b)
}

func FuzzPrivDecode(f *testing.F) {
	privFuzzAdd(f, privFuzzV1Fixture())
	privFuzzAdd(f, privFuzzWrapped(9))
	privFuzzAdd(f, privFuzzWrapped(8))

	mixed, err := Encode([]Record{
		Put{Key: "p", Value: []byte("v")},
		Delete{Key: "d"},
		Batch{Records: []Record{
			Put{Key: "a", Value: []byte("1")},
			Batch{Records: []Record{
				Delete{Key: "inner"},
			}},
		}},
		Batch{Records: []Record{}},
	})
	if err != nil {
		f.Fatal(err)
	}
	privFuzzAdd(f, mixed)

	empty, err := Encode(nil)
	if err != nil {
		f.Fatal(err)
	}
	privFuzzAdd(f, empty)
	privFuzzAdd(f, []byte{2})
	privFuzzAdd(f, privFuzzSpill())

	frame := privFuzzFrame(1, privFuzzPutPL("k", []byte("v")))
	privFuzzAdd(f, append([]byte{0}, frame...))
	privFuzzAdd(f, append([]byte{3}, frame...))

	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			return
		}
		out, err := Encode(recs)
		if err != nil {
			t.Fatalf("Encode of decoded records: %v", err)
		}
		if len(out) == 0 || out[0] != 2 {
			t.Fatalf("encode version = %v, want 2", out)
		}
		if len(b) == 0 {
			t.Fatal("Decode succeeded on empty input")
		}
		switch b[0] {
		case 2:
			if !bytes.Equal(out, b) {
				t.Fatalf("canonical mismatch\nout %x\nin  %x", out, b)
			}
		case 1:
			want := append([]byte{2}, b[1:]...)
			if !bytes.Equal(out, want) {
				t.Fatalf("v1 rewrite mismatch\nout  %x\nwant %x", out, want)
			}
		default:
			t.Fatalf("Decode accepted version %d", b[0])
		}
	})
}
