package archive

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
)

// writeSegment writes evs to a fresh segment, flushing after every event, and returns its path
// and the file's size after each flush — the points a crash can leave a segment at cleanly.
func writeSegment(t *testing.T, evs []pipeline.SessionEvent, closeIt bool) (string, []int64) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sess")
	w, err := createSegment(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int64
	for _, e := range evs {
		if err := w.append(e, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := w.flush(); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, w.info().Bytes)
	}
	if closeIt {
		if err := w.close(); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Cleanup(func() { w.close() })
	}
	return filepath.Join(dir, w.info().File), sizes
}

func readAll(t *testing.T, path string) ([]pipeline.SessionEvent, bool) {
	t.Helper()
	var got []pipeline.SessionEvent
	truncated, err := readSegment(path, func(e *pipeline.SessionEvent) bool {
		got = append(got, *e)
		return true
	})
	if err != nil {
		t.Fatalf("readSegment: %v", err)
	}
	return got, truncated
}

func TestReadSegment_RoundTripsWhatTheWriterWrote(t *testing.T) {
	evs := append(synthSession(11, 10, 4096), edgeEvents()...)
	path, _ := writeSegment(t, evs, true)
	got, truncated := readAll(t, path)
	if truncated || len(got) != len(evs) {
		t.Fatalf("read %d events (truncated=%v), want %d", len(got), truncated, len(evs))
	}
	for i := range evs {
		if w, g := onceThroughJSON(t, evs[i]), mustJSON(t, got[i]); w != g {
			t.Fatalf("event %d differs:\nwant %s\ngot  %s", i, w, g)
		}
	}
}

// A segment still being written has no frame end. Reading it delivers every flushed event and
// says the stream stopped short, which is true and is what a reader of an open segment expects.
func TestReadSegment_AnOpenSegmentReadsUpToItsLastFlush(t *testing.T) {
	evs := synthSession(12, 4, 2048)
	path, _ := writeSegment(t, evs, false)
	got, truncated := readAll(t, path)
	if len(got) != len(evs) || !truncated {
		t.Fatalf("read %d events (truncated=%v), want %d and truncated", len(got), truncated, len(evs))
	}
}

// The crash case. Cut a copy of the file anywhere: the reader must deliver exactly the events
// flushed before the cut — byte-identical, never one more, never a corrupt one — and report
// that the stream stopped short.
func TestReadSegment_TornAtRandomOffsetsDeliversThePrefix(t *testing.T) {
	evs := synthSession(13, 12, 4096)
	path, sizes := writeSegment(t, evs, true)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(42))
	cuts := []int64{0, 1, sizes[0], sizes[0] + 1, sizes[len(sizes)-1] - 1}
	for range 200 {
		cuts = append(cuts, r.Int63n(int64(len(full))))
	}
	torn := filepath.Join(t.TempDir(), "torn.jsonl.zst")
	for _, cut := range cuts {
		if err := os.WriteFile(torn, full[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		want := 0
		for _, s := range sizes {
			if s <= cut {
				want++
			}
		}
		// An empty file is the one cut that is not torn: a segment created and killed before its
		// first write holds no events and lost none. Every other cut stops short of the frame end.
		wantTruncated := cut > 0
		got, truncated := readAll(t, torn)
		if len(got) != want || truncated != wantTruncated {
			t.Fatalf("cut at %d of %d: read %d events (truncated=%v), want %d (truncated=%v)",
				cut, len(full), len(got), truncated, want, wantTruncated)
		}
		for i := range got {
			if w, g := onceThroughJSON(t, evs[i]), mustJSON(t, got[i]); w != g {
				t.Fatalf("cut at %d: event %d differs", cut, i)
			}
		}
	}
}

func TestReadSegment_StopsWhenFnReturnsFalse(t *testing.T) {
	path, _ := writeSegment(t, synthSession(14, 5, 2048), true)
	n := 0
	truncated, err := readSegment(path, func(*pipeline.SessionEvent) bool {
		n++
		return n < 3
	})
	if err != nil || truncated || n != 3 {
		t.Fatalf("n=%d truncated=%v err=%v, want 3 calls", n, truncated, err)
	}
}

// zstdFile writes raw lines as one zstd frame with the given window, for files the writer would
// never produce.
func zstdFile(t *testing.T, window int, lines ...string) string {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithWindowSize(window), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		enc.Write([]byte(l + "\n"))
	}
	enc.Close()
	path := filepath.Join(t.TempDir(), "crafted.jsonl.zst")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A file declaring a window past the cap is refused with an error rather than allocated for:
// a damaged or crafted segment must not set the reader's memory.
func TestReadSegment_RefusesAWindowPast8MiB(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 9<<20)
	path := zstdFile(t, 16<<20, `{"k":"s","h":"a","v":"`+string(big)+`"}`)
	_, err := readSegment(path, func(*pipeline.SessionEvent) bool { return true })
	if err == nil {
		t.Fatal("readSegment accepted a 16 MiB window")
	}
	if !errors.Is(err, zstd.ErrWindowSizeExceeded) {
		t.Fatalf("err = %v, want zstd.ErrWindowSizeExceeded", err)
	}
}

// A reference to a string the segment never defined is corruption: the reader stops there and
// says so, rather than serving a blanked field as if it were the original.
func TestReadSegment_AMissingReferenceIsATruncation(t *testing.T) {
	path := zstdFile(t, 1<<20,
		`{"k":"e","e":{"seq":1,"at":"2026-10-01T00:00:00Z","direction":"outbound","phase":"request"}}`,
		`{"k":"e","e":{"seq":2,"at":"2026-10-01T00:00:00Z","direction":"outbound","phase":"request","inference":{"messages":[{"role":"user"}]}},"r":{"m":["undefined"]}}`)
	got, truncated := readAll(t, path)
	if len(got) != 1 || !truncated {
		t.Fatalf("read %d events (truncated=%v), want 1 and truncated", len(got), truncated)
	}
}

// An unknown record kind is skipped, so a later format can add one without breaking this reader.
func TestReadSegment_SkipsUnknownRecordKinds(t *testing.T) {
	path := zstdFile(t, 1<<20,
		`{"k":"x","future":true}`,
		`{"k":"e","e":{"seq":1,"at":"2026-10-01T00:00:00Z","direction":"outbound","phase":"request"}}`)
	got, truncated := readAll(t, path)
	if len(got) != 1 || truncated {
		t.Fatalf("read %d events (truncated=%v), want 1, not truncated", len(got), truncated)
	}
}
