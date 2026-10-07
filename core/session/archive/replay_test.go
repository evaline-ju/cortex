package archive

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// ReplaySince hands over what is at or after the cutoff, from every session, under each session's
// current id, without tunnel rows. A segment last written before the cutoff is not read at all:
// the old segment here holds an event whose own time is new, and it must not arrive.
func TestReplaySince_DeliversWhatIsNewEnough(t *testing.T) {
	clk := newClock()
	a := openTest(t, t.TempDir(), clk)
	defer a.Close()
	rec := newRecorder(a)
	ev := func(at time.Time, tunnel bool) pipeline.SessionEvent {
		return pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Tunnel: tunnel}
	}
	rec.record("old", ev(clk.now().Add(7*time.Hour), false))
	settle(a)
	clk.advance(7 * time.Hour)
	tick(a) // closes old's idle writer: a closed segment last written 7h ago
	now := clk.now()
	cutoff := now.Add(-6 * time.Hour)
	rec.record("s1", ev(cutoff.Add(-time.Minute), false), ev(cutoff, false), ev(now, true), ev(now, false))
	rec.record("s2", ev(now.Add(-time.Hour), false))
	settle(a)
	a.Rekeyed("s2", "s3")
	settle(a)
	tick(a) // flush the open segments so a reader sees them

	got := map[string]int{}
	skipped, err := a.ReplaySince(context.Background(), cutoff, func(id string, e *pipeline.SessionEvent) {
		if e.SessionID != id {
			t.Errorf("event stamped %q delivered under %q", e.SessionID, id)
		}
		got[id]++
	})
	if err != nil || skipped != 0 {
		t.Fatalf("ReplaySince: skipped %d, err %v", skipped, err)
	}
	if want := map[string]int{"s1": 2, "s3": 1}; !maps.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

// A done context stops the replay with its error, before anything more is delivered.
func TestReplaySince_StopsWhenTheContextIsDone(t *testing.T) {
	clk := newClock()
	a := openTest(t, t.TempDir(), clk)
	defer a.Close()
	newRecorder(a).record("s1", pipeline.SessionEvent{At: clk.now(), Phase: pipeline.SessionResponse})
	settle(a)
	tick(a)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if _, err := a.ReplaySince(ctx, time.Time{}, func(string, *pipeline.SessionEvent) { calls++ }); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("err %v after %d calls, want context.Canceled after none", err, calls)
	}
}

// A segment the replay could read only in part is counted, and its prefix still replays. Each
// case leaves one session's segment as the process that wrote it could have, and the next
// process opens the archive over it.
func TestReplaySince_CountsASegmentReadOnlyInPart(t *testing.T) {
	clk := newClock()
	var evs []pipeline.SessionEvent
	for i := range 3 {
		evs = append(evs, pipeline.SessionEvent{Seq: uint64(i + 1), At: clk.now(), Phase: pipeline.SessionResponse, StatusCode: 200})
	}
	cases := []struct {
		name string
		// segment returns the segment's bytes as its writer left them.
		segment    func(t *testing.T) []byte
		incomplete int
		delivered  int
	}{
		{"closed cleanly", func(t *testing.T) []byte {
			path, _ := writeSegment(t, evs, true)
			return readFile(t, path)
		}, 0, 3},
		{"killed before its frame ended", func(t *testing.T) []byte {
			path, _ := writeSegment(t, evs, false)
			return readFile(t, path)
		}, 1, 3},
		{"torn inside its last event", func(t *testing.T) []byte {
			path, sizes := writeSegment(t, evs, true)
			return readFile(t, path)[:sizes[1]+(sizes[2]-sizes[1])/2]
		}, 1, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, dataDirName, dirName("s1"))
			if err := writeMeta(dir, &sessionMeta{ID: "s1", CreatedAt: clk.now(), UpdatedAt: clk.now(), Summary: session.NewSummaryFold()}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "0001"+segmentExt), c.segment(t), 0o600); err != nil {
				t.Fatal(err)
			}
			a := openTest(t, root, clk)
			defer a.Close()
			delivered := 0
			incomplete, err := a.ReplaySince(context.Background(), time.Time{}, func(string, *pipeline.SessionEvent) { delivered++ })
			if err != nil || incomplete != c.incomplete || delivered != c.delivered {
				t.Fatalf("ReplaySince counted %d incomplete and delivered %d (err %v); want %d and %d",
					incomplete, delivered, err, c.incomplete, c.delivered)
			}
		})
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
