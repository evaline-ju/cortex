package archive

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// ReplaySince calls fn for every archived event at or after cutoff that is not a tunnel row,
// stamped with the id its session has now. Order is per segment in seq order; segments and
// sessions come in any order, so a caller that needs time order sorts what it collects. It
// reports how many segments could not be read; the rest still replay.
//
// FOR STARTUP, BEFORE ANY LISTENER. It reads the index as it stands when called. Called before
// the store serves traffic, it sees exactly what earlier processes wrote, since a process never
// appends to a segment it did not open.
//
// A segment last written before cutoff is skipped unread: it can hold nothing newer. Every other
// one is decoded whole, since segments record no event times. Each event fn receives is the
// reader's own, freshly decoded; a caller that keeps many should keep a reduced copy, not the
// event (see usage.ReplayCopy).
//
// It returns ctx.Err() as soon as ctx is done, checked between events, so a deadline bounds it.
func (a *Archive) ReplaySince(ctx context.Context, cutoff time.Time, fn func(sessionID string, e *pipeline.SessionEvent)) (skipped int, err error) {
	type source struct{ id, path string }
	var srcs []source
	a.idxMu.RLock()
	for id, e := range a.index {
		for _, seg := range e.segments {
			if seg.Events > 0 && !seg.LastWrite.Before(cutoff) {
				srcs = append(srcs, source{id, filepath.Join(e.dir, seg.File)})
			}
		}
	}
	a.idxMu.RUnlock()
	for _, src := range srcs {
		if err := ctx.Err(); err != nil {
			return skipped, err
		}
		var stop error
		_, rerr := readSegment(src.path, func(ev *pipeline.SessionEvent) bool {
			if stop = ctx.Err(); stop != nil {
				return false
			}
			if ev.Tunnel || ev.At.IsZero() || ev.At.Before(cutoff) {
				return true
			}
			ev.SessionID = src.id
			fn(src.id, ev)
			return true
		})
		switch {
		case stop != nil:
			return skipped, stop
		case rerr != nil && !errors.Is(rerr, fs.ErrNotExist):
			skipped++ // a segment pruned or renamed since the snapshot is ErrNotExist, and not a loss
		}
	}
	return skipped, nil
}
