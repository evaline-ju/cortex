package archive

import (
	"slices"
	"testing"
	"time"
)

var pruneNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func seg(file string, daysAgo float64, bytes int64, open bool) segmentRef {
	return segmentRef{
		SessionDir: "d",
		Info:       SegmentInfo{File: file, Bytes: bytes, LastWrite: pruneNow.Add(-time.Duration(daysAgo * 24 * float64(time.Hour)))},
		Open:       open,
	}
}

func files(refs []segmentRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Info.File
	}
	return out
}

// Age first, then size: a segment past retention goes whatever the total, and the size bound
// then takes the oldest of what is left until the archive fits.
func TestPlanPrune_AgeThenBytes(t *testing.T) {
	segs := []segmentRef{
		seg("old", 40, 100, false),
		seg("older", 45, 100, false),
		seg("mid", 10, 300, false),
		seg("new", 1, 300, false),
		seg("newest", 0.1, 300, false),
	}
	got := files(planPrune(segs, pruneNow, 30, 700))
	want := []string{"older", "old", "mid"} // age: older, old; then 900 > 700 → mid
	if !slices.Equal(got, want) {
		t.Fatalf("pruned %v, want %v", got, want)
	}
}

// An open segment is being written: deleting it would lose what the writer flushes next and
// leave the writer appending to an unlinked file. It survives both bounds.
func TestPlanPrune_NeverAnOpenSegment(t *testing.T) {
	segs := []segmentRef{
		seg("ancient-but-open", 90, 1000, true),
		seg("closed", 5, 100, false),
	}
	got := files(planPrune(segs, pruneNow, 30, 10))
	if !slices.Equal(got, []string{"closed"}) {
		t.Fatalf("pruned %v, want [closed]", got)
	}
}

// Under size pressure the oldest go first, whatever order the caller listed them in.
func TestPlanPrune_OldestByLastWriteFirst(t *testing.T) {
	segs := []segmentRef{
		seg("b", 2, 100, false),
		seg("c", 1, 100, false),
		seg("a", 3, 100, false),
	}
	got := files(planPrune(segs, pruneNow, 30, 150))
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("pruned %v, want [a b]", got)
	}
}

func TestPlanPrune_NothingWhenUnderBothBounds(t *testing.T) {
	segs := []segmentRef{seg("a", 29, 100, false), seg("b", 1, 100, false)}
	if got := planPrune(segs, pruneNow, 30, 200); len(got) != 0 {
		t.Fatalf("pruned %v, want nothing", files(got))
	}
}

// A zero bound is no bound: the archive's options turn 0 into the defaults before calling, so
// reaching here with 0 means the caller asked for none, and deleting everything would be the
// one wrong answer.
func TestPlanPrune_ZeroBoundsMeanUnbounded(t *testing.T) {
	segs := []segmentRef{seg("a", 400, 1<<40, false)}
	if got := planPrune(segs, pruneNow, 0, 0); len(got) != 0 {
		t.Fatalf("pruned %v, want nothing", files(got))
	}
}
