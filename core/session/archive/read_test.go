package archive

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

func page(t *testing.T, a *Archive, id string, before uint64, limit int) ([]uint64, PageInfo, bool) {
	t.Helper()
	var seqs []uint64
	info, ok, err := a.Page(id, before, limit, func(e *pipeline.SessionEvent) bool {
		seqs = append(seqs, e.Seq)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return seqs, info, ok
}

// twoSegments archives 6 events for s1 in one process and 6 in the next, so the history spans
// a closed segment and a newer one.
func twoSegments(t *testing.T) (*Archive, *fakeClock) {
	t.Helper()
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk)
	r := newRecorder(a)
	r.record("s1", synthSession(71, 2, 512)...)
	a.Close()
	clk.advance(time.Second)
	b := openTest(t, root, clk)
	r.a = b
	r.record("s1", synthSession(72, 2, 512)...)
	settle(b)
	t.Cleanup(func() { b.Close() })
	return b, clk
}

// A page comes back newest first, across segments, stopping at the limit — and the open
// segment, still being written, is read up to its last flush.
func TestPage_NewestFirstAcrossSegmentsIncludingTheOpenOne(t *testing.T) {
	a, _ := twoSegments(t)
	tick(a) // flush the open segment
	seqs, info, ok := page(t, a, "s1", 0, 8)
	want := []uint64{12, 11, 10, 9, 8, 7, 6, 5}
	if !ok || !equalSeqs(seqs, want) {
		t.Fatalf("page = %v (ok=%v), want %v", seqs, ok, want)
	}
	if info.TotalEvents != 12 || info.OldestSeq != 1 {
		t.Fatalf("info = %+v, want 12 events from seq 1", info)
	}
}

func TestPage_BeforeBoundsThePageAndCanStartInAnOlderSegment(t *testing.T) {
	a, _ := twoSegments(t)
	tick(a)
	if seqs, _, _ := page(t, a, "s1", 8, 3); !equalSeqs(seqs, []uint64{7, 6, 5}) {
		t.Fatalf("before=8 limit=3: %v, want [7 6 5]", seqs)
	}
	if seqs, _, _ := page(t, a, "s1", 3, 10); !equalSeqs(seqs, []uint64{2, 1}) {
		t.Fatalf("before=3: %v, want [2 1]", seqs)
	}
}

func TestPage_AnUnknownSessionIsNotOK(t *testing.T) {
	a, _ := twoSegments(t)
	if _, _, ok := page(t, a, "nope", 0, 5); ok {
		t.Fatal("an unknown session reported ok")
	}
}

// Events on disk keep the id they were written under; a reader sees the session's current id,
// as rekeyLocked retrofits resident events.
func TestPage_RewritesSessionIDToTheCurrentID(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	newRecorder(a).record("pending:x", synthSession(73, 1, 512)...)
	a.Rekeyed("pending:x", "s1")
	tick(a)
	var ids []string
	if _, ok, err := a.Page("s1", 0, 10, func(e *pipeline.SessionEvent) bool {
		ids = append(ids, e.SessionID)
		return true
	}); err != nil || !ok {
		t.Fatalf("Page(s1): ok=%v err=%v", ok, err)
	}
	for _, id := range ids {
		if id != "s1" {
			t.Fatalf("an archived event reads as %q, want s1", id)
		}
	}
}

func TestEvent_FindsOneEventAndNotAMissingOne(t *testing.T) {
	a, _ := twoSegments(t)
	tick(a)
	for _, seq := range []uint64{1, 6, 7, 12} {
		e, ok, err := a.Event("s1", seq)
		if err != nil || !ok || e.Seq != seq {
			t.Fatalf("Event(%d) = %+v, %v, %v", seq, e, ok, err)
		}
	}
	if _, ok, _ := a.Event("s1", 13); ok {
		t.Fatal("Event found a seq past the session's last")
	}
}

// The list's disk-only rows carry the archive's summary: the same figures a resident row would.
func TestSummaries_CarryTheFoldsFiguresAndTimes(t *testing.T) {
	a, clk := twoSegments(t)
	tick(a)
	var found bool
	for _, s := range a.Summaries() {
		if s.ID != "s1" {
			continue
		}
		found = true
		if s.EventCount != 12 || s.CreatedAt.IsZero() || !s.UpdatedAt.Equal(clk.now()) {
			t.Fatalf("summary = %+v", s)
		}
	}
	if !found {
		t.Fatal("s1 missing from Summaries")
	}
}

// A rename that parts an id's history shows readers each part under its own id, before
// anything else is written to either: the renamed entry's events under the new id, the earlier
// entry's under the old one.
func TestPage_FollowsARenameThatPartsHistory(t *testing.T) {
	from := session.DefaultSessionID
	a, st := restarted(t, t.TempDir(), newClock(), from, synthSession(60, 1, 512)) // seqs 1-3
	defer a.Close()
	defer st.Close()
	st.Append(from, synthSession(61, 1, 512)[0]) // seq 4, the new entry's
	st.Rekey(from, "Y")
	settle(a)
	for _, c := range []struct {
		id   string
		want []uint64
	}{{"Y", []uint64{4}}, {from, []uint64{3, 2, 1}}} {
		seqs, info, ok := page(t, a, c.id, 0, 10)
		if !ok || !equalSeqs(seqs, c.want) || info.TotalEvents != len(c.want) {
			t.Errorf("Page(%s) = %v %+v (ok=%v), want %v", c.id, seqs, info, ok, c.want)
		}
	}
	if _, ok, _ := a.Event("Y", 4); !ok {
		t.Error("Event(Y, 4) not found under the new id")
	}
	counts := map[string]int{}
	for _, s := range a.Summaries() {
		counts[s.ID] = s.EventCount
	}
	if counts["Y"] != 1 || counts[from] != 3 {
		t.Errorf("Summaries count Y=%d %s=%d, want 1 and 3", counts["Y"], from, counts[from])
	}
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
