package walcodec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func TestV2ExtraDepthBoundaries(t *testing.T) {
	var r Record = Put{Key: "k"}
	for i := 0; i < 8; i++ {
		r = Batch{Records: []Record{r}}
	}
	b, err := Encode([]Record{r})
	if err != nil {
		t.Fatalf("8 levels Encode: %v", err)
	}
	if _, err = Decode(b); err != nil {
		t.Fatalf("8 levels Decode: %v", err)
	}
	r = Batch{Records: []Record{r}}
	if _, err = Encode([]Record{r}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("9 levels Encode: %v", err)
	}
	var empty Record = Batch{}
	for i := 0; i < 7; i++ {
		empty = Batch{Records: []Record{empty}}
	}
	if _, err = Encode([]Record{empty}); err != nil {
		t.Fatalf("empty at depth 8: %v", err)
	}
	empty = Batch{Records: []Record{empty}}
	if _, err = Encode([]Record{empty}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("empty at depth 9: %v", err)
	}
}

func TestV2ExtraVersionAndBatchKinds(t *testing.T) {
	for _, in := range [][]byte{{0}, {3}, {255}} {
		if _, err := Decode(in); !errors.Is(err, ErrCorrupt) {
			t.Errorf("version %d: %v", in[0], err)
		}
	}
	if b, err := Encode(nil); err != nil || len(b) != 1 || b[0] != version2 {
		t.Fatalf("empty Encode = %v, %v", b, err)
	}
	if b, err := Encode([]Record{Put{Key: "k"}, Delete{Key: "d"}}); err != nil || b[0] != version2 {
		t.Fatalf("Put/Delete version = %v, %v", b, err)
	}
	stream := append([]byte{version1}, mkFrame2(kindBatch, batchPL2(0))...)
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Batch in v1: %v", err)
	}
}

func TestV2ExtraBatchExactCountAndBoundaries(t *testing.T) {
	frame := mkFrame2(kindPut, putPL2("k", []byte("v")))
	cases := []struct {
		name    string
		payload []byte
	}{
		{"short count", []byte{0, 0, 0}},
		{"count too small", batchPL2(0, frame)},
		{"count too large", batchPL2(2, frame)},
		{"trailing bytes", append(batchPL2(1, frame), 0xaa)},
		{"empty trailing", append(batchPL2(0), 0xaa)},
		{"huge count", []byte{0xff, 0xff, 0xff, 0xff}},
	}
	for _, tc := range cases {
		stream := append([]byte{version2}, mkFrame2(kindBatch, tc.payload)...)
		if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	// Give an inner frame a valid CRC and length that would fit only by reading
	// beyond this Batch payload into the following top-level frame.
	badInner := append([]byte{0, 0, 0, 20, kindPut}, make([]byte, 15)...)
	innerCRC := make([]byte, 4)
	binary.BigEndian.PutUint32(innerCRC, crc32.ChecksumIEEE(badInner[4:]))
	badInner = append(badInner, innerCRC...)
	outer := mkFrame2(kindBatch, batchPL2(1, badInner[:9]))
	stream := append([]byte{version2}, outer...)
	stream = append(stream, mkFrame2(kindDelete, []byte{0, 0, 0, 1, 'x'})...)
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("inner crossed batch boundary: %v", err)
	}
}

func TestV2ExtraInnerCRC(t *testing.T) {
	inner := mkFrame2(kindDelete, []byte{0, 0, 0, 1, 'x'})
	inner[len(inner)-1] ^= 1
	stream := append([]byte{version2}, mkFrame2(kindBatch, batchPL2(1, inner))...)
	if _, err := Decode(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("inner CRC: %v", err)
	}
}
