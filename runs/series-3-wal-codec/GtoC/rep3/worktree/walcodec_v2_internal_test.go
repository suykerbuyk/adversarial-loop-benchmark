package walcodec_test

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"walcodec"
)

func intv2BE(x uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, x)
	return out
}

func intv2Frame(typ byte, payload []byte) []byte {
	tp := append([]byte{typ}, payload...)
	out := append(intv2BE(uint32(len(tp))), tp...)
	return append(out, intv2BE(crc32.ChecksumIEEE(tp))...)
}

func intv2PutPL(key string, val []byte) []byte {
	out := append(intv2BE(uint32(len(key))), key...)
	out = append(out, intv2BE(uint32(len(val)))...)
	return append(out, val...)
}

func intv2BatchPL(count uint32, frames ...[]byte) []byte {
	out := intv2BE(count)
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func intv2RecEq(a, b walcodec.Record) bool {
	switch x := a.(type) {
	case walcodec.Put:
		y, ok := b.(walcodec.Put)
		return ok && x.Key == y.Key && string(x.Value) == string(y.Value)
	case walcodec.Delete:
		y, ok := b.(walcodec.Delete)
		return ok && x.Key == y.Key
	case walcodec.Batch:
		y, ok := b.(walcodec.Batch)
		return ok && intv2RecsEq(x.Records, y.Records)
	default:
		return false
	}
}

func intv2RecsEq(a, b []walcodec.Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !intv2RecEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

func intv2WrapBatches(inner walcodec.Record, levels int) walcodec.Record {
	cur := inner
	for i := 0; i < levels; i++ {
		cur = walcodec.Batch{Records: []walcodec.Record{cur}}
	}
	return cur
}

func intv2WantEncodeFail(t *testing.T, recs []walcodec.Record) {
	t.Helper()
	b, err := walcodec.Encode(recs)
	if err == nil {
		t.Fatalf("Encode succeeded, want error; buf len %d", len(b))
	}
	if b != nil {
		t.Fatalf("Encode error returned non-nil buffer of len %d", len(b))
	}
	if errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("Encode error is ErrCorrupt: %v", err)
	}
}

type intv2Foreign struct{}

func (intv2Foreign) Kind() uint8 { return 9 }

func TestIntV2_Depth8RoundTrip(t *testing.T) {
	recs := []walcodec.Record{
		intv2WrapBatches(walcodec.Put{Key: "deep", Value: []byte("x")}, 8),
	}
	b, err := walcodec.Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := walcodec.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !intv2RecsEq(got, recs) {
		t.Fatalf("got %#v want %#v", got, recs)
	}
}

func TestIntV2_Depth9EncodeAndDecode(t *testing.T) {
	nine := intv2WrapBatches(walcodec.Put{Key: "deep", Value: []byte("x")}, 9)
	intv2WantEncodeFail(t, []walcodec.Record{nine})

	cur := intv2Frame(1, intv2PutPL("deep", []byte("x")))
	for i := 0; i < 9; i++ {
		cur = intv2Frame(3, intv2BatchPL(1, cur))
	}
	stream := append([]byte{2}, cur...)
	if _, err := walcodec.Decode(stream); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_EmptyBatchAtDepth9(t *testing.T) {
	nine := intv2WrapBatches(walcodec.Batch{Records: nil}, 9)
	intv2WantEncodeFail(t, []walcodec.Record{nine})

	cur := intv2Frame(3, intv2BatchPL(0))
	for i := 0; i < 9; i++ {
		cur = intv2Frame(3, intv2BatchPL(1, cur))
	}
	stream := append([]byte{2}, cur...)
	if _, err := walcodec.Decode(stream); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_NCountMismatch(t *testing.T) {
	del := intv2Frame(2, append(intv2BE(1), 'k'))
	put := intv2Frame(1, intv2PutPL("a", []byte("1")))

	// n larger than the number of well-formed inner frames.
	tooBig := append([]byte{2}, intv2Frame(3, intv2BatchPL(2, put))...)
	if _, err := walcodec.Decode(tooBig); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("n too large: err = %v, want ErrCorrupt", err)
	}

	// n smaller, with a trailing inner frame.
	tooSmall := append([]byte{2}, intv2Frame(3, intv2BatchPL(1, put, del))...)
	if _, err := walcodec.Decode(tooSmall); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("n too small: err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_HostileN(t *testing.T) {
	// A few dozen bytes, n = 0xFFFFFFFF. Must return ErrCorrupt, not allocate n.
	payload := intv2BatchPL(0xFFFFFFFF, []byte("short-tail-not-a-frame"))
	frame := intv2Frame(3, payload)
	stream := append([]byte{2}, frame...)
	if len(stream) > 80 {
		t.Fatalf("hostile frame grew to %d bytes", len(stream))
	}
	if _, err := walcodec.Decode(stream); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_TrailingBytesAfterNFrames(t *testing.T) {
	put := intv2Frame(1, intv2PutPL("a", []byte("1")))
	payload := append(intv2BatchPL(1, put), 0x00)
	stream := append([]byte{2}, intv2Frame(3, payload)...)
	if _, err := walcodec.Decode(stream); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_InnerLenCoversSibling(t *testing.T) {
	// Top-level: Batch (one inner Put, but its len covers the following sibling
	// Delete's bytes) then a sibling Delete. The sibling bytes are present, so
	// a reader that uses the outer buffer would see them. The decoder must not
	// return the sibling as an inner record.
	sibling := intv2Frame(2, append(intv2BE(1), 's'))

	// Claim the Batch payload is n + innerPut + sibling, but set the inner
	// frame's len so it extends past the Batch payload into the sibling.
	// Construct the Batch payload as n=1 followed by a frame whose len covers
	// exactly the sibling bytes that follow the Batch frame.
	//
	// Layout:
	//   version
	//   batch frame: len | type=3 | n=1 | inner-len | type | ... | crc
	//   sibling frame (present in the stream)
	//
	// The inner len is set so type+payload of the inner frame extends past
	// the Batch payload and covers exactly the sibling frame's bytes. CRC of
	// the inner frame is computed over that span so a reader that ignores the
	// enclosing bound would accept it.

	// Build the sibling first so we know its size.
	// Batch payload will be: n(4) + a forged inner frame header that claims
	// a body long enough to consume the sibling, plus a CRC that sits where
	// a careless reader would place it after the sibling.
	//
	// Simpler construction from the plan:
	// "An inner len that extends past the enclosing Batch payload and covers
	// exactly the following top-level sibling frame's bytes".
	//
	// Enclosing Batch payload = n(4) + incomplete inner header that does not
	// contain the sibling. The sibling follows the Batch frame. The inner len
	// claims enough bytes to reach through the Batch CRC and the sibling.

	nField := intv2BE(1)
	// Inner frame starts at the beginning of the payload after n.
	// We will set inner len so the claimed frame ends exactly at the end of
	// the sibling. The Batch payload itself ends before the sibling.
	//
	// Let batch payload be n + 4 (the inner len word) + 1 (a type byte) and
	// stop there — truncated relative to the claimed inner len. The sibling
	// follows the batch frame (after the batch CRC).
	//
	// Claimed inner body length = (bytes from inner type through the end of
	// the sibling). Those bytes are: type(1) that we include, then the batch
	// CRC (4) which sits outside the payload, then the sibling frame.
	//
	// A correct decoder sees the inner len extending past the enclosing
	// payload and returns ErrCorrupt without treating the sibling as the
	// inner record.

	siblingBytes := sibling
	// Bytes after the inner len word that a past-the-payload reader would
	// consume: we include only a type byte inside the payload, then the
	// batch CRC (written as part of the outer frame), then the sibling.
	// Claimed inner len (type+payload) = 1 (type) + 4 (batch crc treated as
	// payload) + len(sibling). The CRC of the inner frame would then sit
	// after the sibling. We place a valid CRC there so an unbounded reader
	// would succeed and return a record built from the sibling bytes.

	innerType := byte(1)
	// payload the unbounded reader would see after the type byte:
	// batchCRC(4) + sibling
	// We don't know batchCRC until the batch frame is assembled, and the
	// inner CRC depends on those bytes. Compute them together.

	// Batch payload = n(4) + innerLen(4) + innerType(1). That is all that
	// belongs to the batch. Then batch CRC(4). Then sibling. Then inner CRC(4)
	// so an unbounded reader finds a complete frame.
	batchPayloadLen := 4 + 4 + 1
	innerBodyLen := uint32(1 + 4 + len(siblingBytes)) // type + batchCRC + sibling
	// The frame the unbounded reader reads:
	//   [innerLen 4][type 1][batchCRC 4][sibling][innerCRC 4]
	// Those innerLen and type bytes are inside the batch payload. batchCRC is
	// the batch frame's CRC. sibling follows. innerCRC follows the sibling.

	// Batch frame body = type(3) + batch payload.
	// CRC is over that body. The batch CRC bytes are part of the span the
	// inner frame claims, so we need the CRC of a body whose later bytes
	// include that same CRC. That's circular if the inner CRC depends on the
	// batch CRC and we also want the inner CRC to be valid.
	//
	// The test does not require the inner CRC to be valid. It requires that
	// the sibling bytes are present and that an inner len covers them. A
	// decoder that reads past the enclosing payload might then CRC-fail or
	// might, if we make the CRC valid, return the sibling. Either way the
	// correct decoder returns ErrCorrupt because the len extends past the
	// enclosing payload.
	//
	// To make the "must not return the sibling" assertion meaningful, build
	// a stream where an unbounded reader WOULD succeed. That means the inner
	// CRC must match the bytes it would read, including the batch CRC.
	//
	// batch body = [3][n=1][innerLen][innerType]
	// batch CRC = CRC32(batch body) — independent of what follows. The inner
	// frame's claimed bytes include that CRC, but the CRC itself does not
	// depend on the inner CRC. So:
	//   1. compute batch CRC from the batch body
	//   2. compute inner CRC over [innerType][batchCRC][sibling]
	//   3. append inner CRC after the sibling

	batchBody := append([]byte{3}, nField...)
	batchBody = append(batchBody, intv2BE(innerBodyLen)...)
	batchBody = append(batchBody, innerType)
	if len(batchBody) != 1+batchPayloadLen {
		t.Fatalf("batch body len %d", len(batchBody))
	}
	batchCRC := crc32.ChecksumIEEE(batchBody)

	innerTP := []byte{innerType}
	innerTP = append(innerTP, intv2BE(batchCRC)...)
	innerTP = append(innerTP, siblingBytes...)
	if uint32(len(innerTP)) != innerBodyLen {
		t.Fatalf("inner body %d want %d", len(innerTP), innerBodyLen)
	}
	innerCRC := crc32.ChecksumIEEE(innerTP)

	stream := []byte{2}
	stream = append(stream, intv2BE(uint32(len(batchBody)))...)
	stream = append(stream, batchBody...)
	stream = append(stream, intv2BE(batchCRC)...)
	stream = append(stream, siblingBytes...)
	stream = append(stream, intv2BE(innerCRC)...)

	got, err := walcodec.Decode(stream)
	if !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt; got %#v", err, got)
	}
}

func TestIntV2_InnerLenCoversNextInner(t *testing.T) {
	// Two inner frames claimed (n = 2), but the first inner len covers the
	// next inner frame's bytes. CRC is computed over that span, so a reader
	// that does not stop at a frame boundary inside the payload would accept
	// one oversized frame and then fail to find the second — or, if it used
	// the outer buffer incorrectly, might parse differently. Either way this
	// stream is corrupt: the first inner len does not end at a frame boundary
	// that leaves a second well-formed frame inside the payload.
	second := intv2Frame(2, append(intv2BE(1), 'z'))
	// First frame claims a body of type + second's bytes, CRC over that span.
	// The payload contains n=2, then that one oversized frame, and nothing else.
	// The oversized frame's len extends through what would have been the second
	// frame, so there is no second frame left. n still says 2.
	firstBody := append([]byte{1}, second...)
	first := append(intv2BE(uint32(len(firstBody))), firstBody...)
	first = append(first, intv2BE(crc32.ChecksumIEEE(firstBody))...)

	payload := intv2BatchPL(2, first)
	stream := append([]byte{2}, intv2Frame(3, payload)...)
	got, err := walcodec.Decode(stream)
	if !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt; got %#v", err, got)
	}
}

func TestIntV2_Version1RejectsBatch(t *testing.T) {
	frame := intv2Frame(3, intv2BatchPL(0))
	stream := append([]byte{1}, frame...)
	if _, err := walcodec.Decode(stream); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_Version0Rejected(t *testing.T) {
	if _, err := walcodec.Decode([]byte{0}); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("bare 0: err = %v, want ErrCorrupt", err)
	}
	b, err := walcodec.Encode([]walcodec.Record{walcodec.Put{Key: "k", Value: []byte("v")}})
	if err != nil {
		t.Fatal(err)
	}
	b[0] = 0
	if _, err := walcodec.Decode(b); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("flipped version: err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_InnerCRCFlip(t *testing.T) {
	recs := []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{
			walcodec.Put{Key: "a", Value: []byte("1")},
		}},
	}
	b, err := walcodec.Encode(recs)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the stream but not the version byte. The last byte
	// is the outer CRC; flipping an earlier payload byte corrupts the inner
	// frame (or the outer, if we hit the outer CRC). Flip the byte just
	// before the final 4-byte outer CRC so the inner frame's CRC is hit
	// when the batch has a single small inner frame.
	if len(b) < 8 {
		t.Fatalf("stream too short: %d", len(b))
	}
	b[len(b)-5] ^= 0xff
	if _, err := walcodec.Decode(b); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_InnerOversizeLen(t *testing.T) {
	// Inner len of 16<<20+1, huge body not present.
	payload := intv2BE(1) // n = 1
	payload = append(payload, intv2BE(uint32(16<<20+1))...)
	payload = append(payload, 1) // a type byte, far fewer bytes than len claims
	stream := append([]byte{2}, intv2Frame(3, payload)...)
	if _, err := walcodec.Decode(stream); !errors.Is(err, walcodec.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestIntV2_EmptyBatchNilAndEmpty(t *testing.T) {
	for _, recs := range [][]walcodec.Record{
		{walcodec.Batch{Records: nil}},
		{walcodec.Batch{Records: []walcodec.Record{}}},
	} {
		b, err := walcodec.Encode(recs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := walcodec.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		batch, ok := got[0].(walcodec.Batch)
		if !ok {
			t.Fatalf("got %T, want Batch", got[0])
		}
		if batch.Records == nil || len(batch.Records) != 0 {
			t.Fatalf("Records = %#v, want non-nil empty", batch.Records)
		}
		// Encoded payload is n = 0. Frame is version + len + type + n + crc.
		// Confirm the batch payload's n field is 0 by decoding the frame.
		if len(b) < 1+4+1+4+4 {
			t.Fatalf("encoded len %d", len(b))
		}
		n := binary.BigEndian.Uint32(b[1+4+1 : 1+4+1+4])
		if n != 0 {
			t.Fatalf("encoded n = %d, want 0", n)
		}
	}
}

func TestIntV2_EncodeNilIsVersion2(t *testing.T) {
	b, err := walcodec.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || b[0] != 2 {
		t.Fatalf("Encode(nil) = %v, want []byte{2}", b)
	}
	got, err := walcodec.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Decode = %#v, want non-nil empty", got)
	}
}

func TestIntV2_RejectPointerNilAndForeign(t *testing.T) {
	var nilRec walcodec.Record
	var nilBatch *walcodec.Batch
	foreign := intv2Foreign{}

	intv2WantEncodeFail(t, []walcodec.Record{nilBatch})
	intv2WantEncodeFail(t, []walcodec.Record{nilRec})
	intv2WantEncodeFail(t, []walcodec.Record{foreign})

	// Inside a Batch.
	intv2WantEncodeFail(t, []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{nilRec}},
	})
	intv2WantEncodeFail(t, []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{&walcodec.Put{Key: "p"}}},
	})
	intv2WantEncodeFail(t, []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{foreign}},
	})

	// Inside a Batch nested in a Batch.
	intv2WantEncodeFail(t, []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{
			walcodec.Batch{Records: []walcodec.Record{nilRec}},
		}},
	})
	intv2WantEncodeFail(t, []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{
			walcodec.Batch{Records: []walcodec.Record{&walcodec.Put{Key: "p"}}},
		}},
	})
	intv2WantEncodeFail(t, []walcodec.Record{
		walcodec.Batch{Records: []walcodec.Record{
			walcodec.Batch{Records: []walcodec.Record{foreign}},
		}},
	})
}
