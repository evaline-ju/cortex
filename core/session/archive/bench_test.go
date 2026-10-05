package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// segmentBytes writes evs into one closed segment and returns its size and the naive JSON size
// of the same events — what writing each event out whole would have cost.
func segmentBytes(tb testing.TB, evs []pipeline.SessionEvent) (onDisk, naive int64) {
	tb.Helper()
	w, err := createSegment(filepath.Join(tb.TempDir(), "sess"), time.Now())
	if err != nil {
		tb.Fatal(err)
	}
	for _, e := range evs {
		if err := w.append(e, time.Now()); err != nil {
			tb.Fatal(err)
		}
		b, err := json.Marshal(e)
		if err != nil {
			tb.Fatal(err)
		}
		naive += int64(len(b))
	}
	if err := w.close(); err != nil {
		tb.Fatal(err)
	}
	return w.info().Bytes, naive
}

// THE DEDUP MUST KEEP A RE-SENT CONVERSATION ONCE. Every request carries the whole
// conversation, so a regression that stops sharing strings — a seen set reset per event, the
// on-disk form of gotcha #14's 11.2x — still round-trips perfectly and only shows as size. This
// runs in CI where the benchmarks do not.
//
// THE PROMPT IS PAST THE 1 MiB WINDOW ON PURPOSE, as live prompts are. Below it, zstd finds the
// repeats on its own and hides a lost dedup: measured on a 64KB prompt, 47.2x with the dedup and
// still 31.0x without. At 1.5MB it is 61.2x against 3.1x, so a 20x floor fails on a lost dedup
// and on nothing else.
func TestSegment_SizeStaysFarUnderNaive(t *testing.T) {
	onDisk, naive := segmentBytes(t, synthSession(21, 10, 1536<<10))
	if ratio := float64(naive) / float64(onDisk); ratio < 20 {
		t.Fatalf("segment is %d bytes against %d naive: %.1fx, want at least 20x", onDisk, naive, ratio)
	}
}

// BenchmarkSegment_BytesPerEvent is the disk cost per event at the live prompt size: a system
// prompt and tool manifest of about 800KB, re-sent on every request.
func BenchmarkSegment_BytesPerEvent(b *testing.B) {
	evs := synthSession(22, 60, 800<<10)
	var onDisk, naive int64
	for b.Loop() {
		onDisk, naive = segmentBytes(b, evs)
	}
	b.ReportMetric(float64(onDisk)/float64(len(evs)), "B/event")
	b.ReportMetric(float64(naive)/float64(len(evs)), "naive-B/event")
	b.ReportMetric(float64(naive)/float64(onDisk), "x-smaller")
}

// BenchmarkSegment_HeapPerOpenWriter is what each open segment costs the proxy in heap, which
// is what the archive's cap on open writers multiplies.
func BenchmarkSegment_HeapPerOpenWriter(b *testing.B) {
	const writers = 10
	evs := synthSession(23, 6, 256<<10)
	dir := b.TempDir()
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapInuse
	}
	var per float64
	for b.Loop() {
		before := heap()
		ws := make([]*segmentWriter, writers)
		for i := range ws {
			w, err := createSegment(filepath.Join(dir, "sess"), time.Now())
			if err != nil {
				b.Fatal(err)
			}
			for _, e := range evs {
				if err := w.append(e, time.Now()); err != nil {
					b.Fatal(err)
				}
			}
			if err := w.flush(); err != nil {
				b.Fatal(err)
			}
			ws[i] = w
		}
		per = float64(heap()-before) / writers / (1 << 20)
		for _, w := range ws {
			w.close()
			os.Remove(filepath.Join(dir, "sess", w.info().File))
		}
	}
	b.ReportMetric(per, "MB/writer")
}
