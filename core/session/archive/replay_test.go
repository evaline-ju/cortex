package archive

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
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
