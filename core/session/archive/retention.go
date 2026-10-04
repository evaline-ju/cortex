package archive

import (
	"cmp"
	"slices"
	"time"
)

// segmentRef is one segment as retention sees it: where it lives, its session.json entry, and
// whether a writer still has it open.
type segmentRef struct {
	SessionDir string
	Info       SegmentInfo
	Open       bool
}

// planPrune returns the segments to delete, oldest first: every closed segment last written
// more than retainDays ago, then — while what is left is over maxBytes — the oldest closed
// segments by last write. A zero or negative bound is no bound.
//
// A PURE FUNCTION OF ITS INPUT, so the policy is tested without a disk and the writer goroutine
// only executes the plan.
//
// BY LAST WRITE, NOT BY NAME OR OPEN TIME, so a segment written across days is kept until its
// newest event ages out. A wrong clock can make that early; that is accepted for history,
// where the cost ledger, guarding billing figures, pays for a two-pass delete instead.
//
// AN OPEN SEGMENT IS NEVER RETURNED. Its writer would go on appending to an unlinked file, and
// everything it flushed next would be lost. It counts toward the size bound all the same, since
// its bytes are on disk; the bound may then stay exceeded until the writer closes.
func planPrune(segs []segmentRef, now time.Time, retainDays int, maxBytes int64) []segmentRef {
	sorted := slices.Clone(segs)
	slices.SortStableFunc(sorted, func(a, b segmentRef) int {
		return cmp.Compare(a.Info.LastWrite.UnixNano(), b.Info.LastWrite.UnixNano())
	})

	var cutoff time.Time
	if retainDays > 0 {
		cutoff = now.Add(-time.Duration(retainDays) * 24 * time.Hour)
	}
	var del, keep []segmentRef
	var total int64
	for _, s := range sorted {
		if !s.Open && retainDays > 0 && s.Info.LastWrite.Before(cutoff) {
			del = append(del, s)
			continue
		}
		keep = append(keep, s)
		total += s.Info.Bytes
	}
	if maxBytes <= 0 {
		return del
	}
	for _, s := range keep {
		if total <= maxBytes {
			break
		}
		if s.Open {
			continue
		}
		del = append(del, s)
		total -= s.Info.Bytes
	}
	return del
}
