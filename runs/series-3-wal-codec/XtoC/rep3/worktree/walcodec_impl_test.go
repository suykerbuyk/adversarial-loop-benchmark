package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"reflect"
	"testing"
)

type foreignRecord struct{}

func (foreignRecord) Kind() uint8 { return 99 }

func implFrame(tag byte, payload []byte) []byte {
	body := append([]byte{tag}, payload...)
	out := make([]byte, 4, 4+len(body)+4)
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	out = append(out, body...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}

func implBatch(records ...[]byte) []byte {
	p := binary.BigEndian.AppendUint32(nil, uint32(len(records)))
	for _, r := range records {
		p = append(p, r...)
	}
	return implFrame(batchTag, p)
}

func implNested(depth int, leaf []byte) []byte {
	for i := 0; i < depth; i++ {
		leaf = implBatch(leaf)
	}
	return append([]byte{encodeVersion}, leaf...)
}

func TestBatchDepthBounds(t *testing.T) {
	put, err := Encode([]Record{Put{Key: "k"}})
	if err != nil {
		t.Fatal(err)
	}
	for depth := 0; depth <= maxDepth; depth++ {
		stream := implNested(depth, put[1:])
		recs, err := Decode(stream)
		if err != nil {
			t.Fatalf("Decode depth %d: %v", depth, err)
		}
		encoded, err := Encode(recs)
		if err != nil {
			t.Fatalf("Encode depth %d: %v", depth, err)
		}
		if _, err := Decode(encoded); err != nil {
			t.Fatalf("re-decode depth %d: %v", depth, err)
		}
	}
	deep := implNested(maxDepth+1, put[1:])
	if _, err := Decode(deep); err != ErrCorrupt {
		t.Fatalf("depth 9: %v", err)
	}
	rec := Record(Put{Key: "k"})
	for i := 0; i < maxDepth+1; i++ {
		rec = Batch{Records: []Record{rec}}
	}
	if _, err := Encode([]Record{rec}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("encode depth 9: %v", err)
	}
	var empty []Record
	for i := 0; i < maxDepth+1; i++ {
		empty = []Record{Batch{Records: empty}}
	}
	if _, err := Encode(empty); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("empty depth 9: %v", err)
	}
}

func TestBatchMalformedCases(t *testing.T) {
	validPut, _ := Encode([]Record{Put{Key: "k"}})
	putFrame := validPut[1:]
	cases := [][]byte{
		append([]byte{2}, implFrame(batchTag, []byte{0, 0})...), // payload shorter than count
		append([]byte{2}, implBatch(putFrame)...),               // count one, valid
	}
	// Count too large, count too small with trailing frame, and an inner CRC error.
	cases = append(cases,
		append([]byte{2}, implFrame(batchTag, binary.BigEndian.AppendUint32(nil, ^uint32(0)))...),
		append([]byte{2}, implFrame(batchTag, append(binary.BigEndian.AppendUint32(nil, 0), putFrame...))...),
	)
	badCRC := append([]byte(nil), putFrame...)
	badCRC[len(badCRC)-1] ^= 1
	cases = append(cases, append([]byte{2}, implBatch(badCRC)...))
	for i, stream := range cases {
		_, err := Decode(stream)
		if i == 1 {
			if err != nil {
				t.Fatalf("valid nested fixture: %v", err)
			}
			continue
		}
		if err != ErrCorrupt {
			t.Fatalf("case %d error = %v", i, err)
		}
	}
	if _, err := Decode(append([]byte{1}, implBatch(putFrame)...)); err != ErrCorrupt {
		t.Fatalf("v1 batch: %v", err)
	}
	if _, err := Decode([]byte{3}); err != ErrCorrupt {
		t.Fatalf("version 3: %v", err)
	}
	if _, err := Decode([]byte{0}); err != ErrCorrupt {
		t.Fatalf("version 0: %v", err)
	}
	// A frame length may fit the outer frame while exceeding its Batch payload.
	inner := make([]byte, 4)
	binary.BigEndian.PutUint32(inner, maxBody)
	if _, err := Decode(append([]byte{2}, implBatch(inner)...)); err != ErrCorrupt {
		t.Fatalf("inner overrun: %v", err)
	}
}

func TestEncodeErrorCompatibilityAndCycle(t *testing.T) {
	if ErrCorrupt.Error() != "walcodec: corrupt stream" {
		t.Fatal(ErrCorrupt)
	}
	if _, err := Encode([]Record{nil}); err == nil || errors.Is(err, ErrCorrupt) {
		t.Fatalf("nil record error: %v", err)
	}
	for _, r := range []Record{&Put{}, &Delete{}, &Batch{}, foreignRecord{}} {
		if _, err := Encode([]Record{r}); err == nil || errors.Is(err, ErrCorrupt) || !errors.Is(err, errUnsupportedRecord) {
			t.Fatalf("unsupported %T error: %v", r, err)
		}
	}
	cyclicRecords := make([]Record, 1)
	cyclicRecords[0] = Batch{Records: cyclicRecords}
	if _, err := Encode([]Record{Batch{Records: cyclicRecords}}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("cycle error: %v", err)
	}
}

func TestDecodeContractsAndCopy(t *testing.T) {
	got, err := Decode([]byte{2})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty stream: %#v %v", got, err)
	}
	stream, err := Encode([]Record{Put{Key: "k", Value: []byte{}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err = Decode(stream)
	if err != nil {
		t.Fatal(err)
	}
	p := got[0].(Put)
	if p.Value == nil {
		t.Fatal("empty Put value decoded as nil")
	}
	stream, err = Encode([]Record{Put{Key: "k", Value: []byte("v")}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = Decode(stream)
	stream[len(stream)-8] ^= 1
	if string(got[0].(Put).Value) != "v" {
		t.Fatal("Put value aliases input stream")
	}
}

func FuzzBatchDecode(f *testing.F) {
	leaf, _ := Encode([]Record{Put{Key: "seed"}})
	f.Add(implNested(maxDepth, leaf[1:]))
	f.Add(implNested(maxDepth+1, leaf[1:]))
	f.Add([]byte{1})
	f.Add([]byte{2})
	f.Add(append([]byte{3}, leaf[1:]...))
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			if err != ErrCorrupt || recs != nil {
				t.Fatalf("malformed result: %#v, %v", recs, err)
			}
			return
		}
		encoded, err := Encode(recs)
		if err != nil {
			t.Fatalf("accepted input could not encode: %v", err)
		}
		again, err := Decode(encoded)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		if !reflect.DeepEqual(recs, again) {
			t.Fatalf("round trip differs: %#v != %#v", recs, again)
		}
	})
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte{1})
	f.Add([]byte{2})
	f.Add([]byte{3})
	f.Add(implNested(maxDepth, []byte{}))
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := Decode(b)
		if err != nil {
			if err != ErrCorrupt || recs != nil {
				t.Fatalf("malformed result %#v %v", recs, err)
			}
			return
		}
		enc, err := Encode(recs)
		if err != nil {
			t.Fatalf("accepted stream not encodable: %v", err)
		}
		again, err := Decode(enc)
		if err != nil || !reflect.DeepEqual(recs, again) {
			t.Fatalf("round trip: %#v %v", again, err)
		}
	})
}
