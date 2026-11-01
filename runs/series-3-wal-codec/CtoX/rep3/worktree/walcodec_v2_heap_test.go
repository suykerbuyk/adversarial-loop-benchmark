package walcodec

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// maxHeapFactor is the documented ceiling (Decode doc comment: "up to
// roughly 10x len(b)") with headroom for allocator size-class rounding.
const maxHeapFactor = 12

// decodeHeapFactor returns total bytes allocated by one Decode(b) divided by
// len(b). GC is disabled during the measurement so TotalAlloc is exact.
func decodeHeapFactor(t *testing.T, b []byte, wantErr bool) float64 {
	t.Helper()
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	recs, err := Decode(b)
	runtime.ReadMemStats(&m1)
	if (err != nil) != wantErr {
		t.Fatalf("Decode err = %v, wantErr %v", err, wantErr)
	}
	runtime.KeepAlive(recs)
	return float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(b))
}

// TestDecodeHeapAmplification pins the linear heap bound documented on
// Decode, for the worst-case shapes: many minimal records at top level and
// inside a Batch, deep nests of empty Batches, and (A4) a stream whose final
// frame is corrupt so every earlier record was allocated and discarded.
func TestDecodeHeapAmplification(t *testing.T) {
	if raceEnabled {
		t.Skip("race detector changes allocation accounting")
	}
	const n = 100000
	rep := func(r Record, k int) []Record {
		rs := make([]Record, k)
		for i := range rs {
			rs[i] = r
		}
		return rs
	}
	shapes := map[string]Record{
		"emptyDelete": Delete{},
		"emptyPut":    Put{},
		"emptyBatch":  Batch{},
		"tinyPut":     Put{Key: "a", Value: []byte{1}},
		"tinyDelete":  Delete{Key: "a"},
		"nest7Batch":  nestRec(7), // 7 so it still fits inside the outer Batch (depth 8)
	}
	for name, r := range shapes {
		top, err := Encode(rep(r, n))
		if err != nil {
			t.Fatalf("%s: Encode: %v", name, err)
		}
		// A Batch frame is capped at 16 MiB; keep the inner count well under it.
		inner, err := Encode([]Record{Batch{Records: rep(r, n/10)}})
		if err != nil {
			t.Fatalf("%s: Encode batch: %v", name, err)
		}
		bad := append([]byte(nil), top...)
		bad[len(bad)-1] ^= 0xff // corrupt the last frame's crc
		for _, c := range []struct {
			label   string
			b       []byte
			wantErr bool
		}{{"top", top, false}, {"inBatch", inner, false}, {"lateError", bad, true}} {
			f := decodeHeapFactor(t, c.b, c.wantErr)
			t.Logf("%s/%s: %.2f bytes allocated per input byte", name, c.label, f)
			if f > maxHeapFactor {
				t.Errorf("%s/%s: heap factor %.2f exceeds %d", name, c.label, f, maxHeapFactor)
			}
		}
	}
}
