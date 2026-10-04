package archive

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

const (
	// dataDirName holds one directory per session. Sessions live a level below the root so that
	// clearing all history can be one rename of this directory.
	dataDirName = "data"

	// defaultQueueDepth is how far the writer may fall behind the request path before events are
	// dropped and counted. reserve slots of it are held back for renames, so a burst of events can
	// never be the reason a rename is lost.
	defaultQueueDepth = 4096
	reserve           = 64

	// defaultIdleClose and defaultMaxOpen bound the heap open writers hold — about 3 MB each.
	// The idle window is long because the cap already bounds the worst case, and every reopened
	// session re-emits its current strings into a new segment (0.16–0.34 MB measured).
	defaultIdleClose = 60 * time.Minute
	defaultMaxOpen   = 16

	// Cadences, from the writer goroutine's tick: segments flush and fsync every tick, so an
	// unclean kill loses at most about a second; session.json is rewritten every metaEvery while
	// dirty; retention runs every pruneEvery.
	defaultTickEvery = time.Second
	metaEvery        = 5 * time.Second
	pruneEvery       = time.Hour

	defaultRetentionDays = 30
	// MaxRetentionDays is the cost ledger's ceiling, for its reason: a retention large enough to
	// wrap time arithmetic must not turn into a cutoff in the future.
	MaxRetentionDays = 3650
	defaultMaxBytes  = 2 << 30
)

// Option configures an Archive.
type Option func(*Archive)

// WithClock replaces the archive's time source, for tests.
func WithClock(now func() time.Time) Option { return func(a *Archive) { a.now = now } }

// WithRetentionDays sets how long a segment is kept after its last write. Zero or negative is
// the default, 30; past MaxRetentionDays it is clamped.
func WithRetentionDays(days int) Option { return func(a *Archive) { a.retainDays = days } }

// WithMaxBytes sets the archive's size bound. Zero or negative is the default, 2 GiB.
func WithMaxBytes(n int64) Option { return func(a *Archive) { a.maxBytes = n } }

// WithQueueDepth sets the request path's queue depth, for tests.
func WithQueueDepth(n int) Option { return func(a *Archive) { a.depth = n } }

// WithIdleClose sets how long a session's segment stays open without a write.
func WithIdleClose(d time.Duration) Option { return func(a *Archive) { a.idleClose = d } }

// WithMaxOpenWriters caps how many segments are open at once.
func WithMaxOpenWriters(n int) Option { return func(a *Archive) { a.maxOpen = n } }

// Stats is what the archive reports about itself.
type Stats struct {
	Bytes         int64
	MaxBytes      int64
	RetentionDays int
	// DroppedEvents counts events that never reached disk: a full queue, or a pause after the
	// disk filled. WriteErrors counts failed writes. DroppedRenames counts renames lost to a
	// queue full even past its reserve.
	DroppedEvents  uint64
	WriteErrors    uint64
	DroppedRenames uint64
	// Paused is true after ENOSPC, until the next retention pass.
	Paused bool
}

// Archive is the session archive: a session.Recorder that writes every event the store appends
// to disk, a session.Rekeyer that follows the store's renames, and a session.SeqSeeder that lets
// a re-created session continue its numbering.
//
// NOTHING ON THE REQUEST PATH TOUCHES DISK. Record and Rekeyed run under the store's write lock,
// in front of live proxied traffic: they update a map and make a non-blocking send. One writer
// goroutine owns every file, every segment writer and every session.json, and the state below
// marked "writer goroutine only" is never touched from anywhere else.
type Archive struct {
	root, data string
	now        func() time.Time
	retainDays int
	maxBytes   int64
	depth      int
	idleClose  time.Duration
	maxOpen    int
	tickEvery  time.Duration

	// Test seams: gate holds the writer before each op; segHook sees each new segment writer.
	gate    chan struct{}
	segHook func(*segmentWriter)

	ops       chan op
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	// lastSeq is the highest seq recorded per session id, for SeqSeeder. Updated synchronously
	// by Record and Rekeyed, so it is right before the writer has caught up.
	mu      sync.Mutex
	lastSeq map[string]uint64

	dropped, writeErrors, droppedRenames atomic.Uint64
	bytes                                atomic.Int64
	paused                               atomic.Bool

	// Writer goroutine only.
	sessions      map[string]*sessionState
	lastMeta      time.Time
	lastPrune     time.Time
	reportedDrops uint64
	lastDropLog   time.Time
}

// sessionState is one archived session as the writer goroutine holds it.
type sessionState struct {
	dir       string
	meta      *sessionMeta // closed segments in Segments; the open one, if any, is w
	w         *segmentWriter
	lastWrite time.Time
	metaDirty bool
}

type opKind uint8

const (
	opEvent opKind = iota
	opRename
	opFunc
)

// op is one unit of work for the writer goroutine. Events and renames share one FIFO, so a
// rename applies in order with the events around it.
type op struct {
	kind   opKind
	id, to string
	ev     pipeline.SessionEvent
	f      func()
	done   chan struct{}
}

// Open opens the archive rooted at root, creating root/data if needed. It loads every session's
// session.json, reconciles each with its files, prunes to the retention bounds — all before
// returning, so nothing is served against stale state — and starts the writer goroutine.
func Open(root string, opts ...Option) (*Archive, error) {
	a := &Archive{
		root:       root,
		data:       filepath.Join(root, dataDirName),
		now:        time.Now,
		retainDays: defaultRetentionDays,
		maxBytes:   defaultMaxBytes,
		depth:      defaultQueueDepth,
		idleClose:  defaultIdleClose,
		maxOpen:    defaultMaxOpen,
		tickEvery:  defaultTickEvery,
		lastSeq:    map[string]uint64{},
		sessions:   map[string]*sessionState{},
	}
	for _, o := range opts {
		o(a)
	}
	if a.retainDays <= 0 {
		a.retainDays = defaultRetentionDays
	}
	if a.retainDays > MaxRetentionDays {
		slog.Warn("session archive: retention_days is past the maximum; clamping",
			"requested", a.retainDays, "using", MaxRetentionDays)
		a.retainDays = MaxRetentionDays
	}
	if a.maxBytes <= 0 {
		a.maxBytes = defaultMaxBytes
	}
	if a.depth <= reserve {
		a.depth = reserve + 1
	}
	if a.maxOpen <= 0 {
		a.maxOpen = defaultMaxOpen
	}
	if a.idleClose <= 0 {
		a.idleClose = defaultIdleClose
	}
	if err := os.MkdirAll(a.data, dirMode); err != nil {
		return nil, err
	}

	loadedSessions, skipped, err := loadMetas(a.data)
	if err != nil {
		return nil, err
	}
	for _, l := range loadedSessions {
		a.adopt(l)
	}
	if skipped > 0 {
		slog.Warn("session archive: some sessions could not be read and are not served", "skipped", skipped)
	}
	now := a.now()
	a.prune(now)
	a.lastPrune, a.lastMeta = now, now
	a.refreshBytes()

	a.ops = make(chan op, a.depth)
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	go a.run()
	return a, nil
}

// adopt takes one loaded session into the archive at startup: reconciled with its files, its
// directory named for the id its session.json holds, and its last seq known to the store.
func (a *Archive) adopt(l loaded) {
	changed, err := reconcile(l.dir, l.meta)
	if err != nil {
		slog.Warn("session archive: could not reconcile a session with its files", "dir", l.dir, "error", err)
	}
	// A crash between rewriting session.json for a rename and renaming the directory leaves the
	// file naming the new id under the old directory. Finish the rename.
	if want := filepath.Join(a.data, dirName(l.meta.ID)); l.dir != want {
		if _, err := os.Stat(want); errors.Is(err, os.ErrNotExist) && os.Rename(l.dir, want) == nil {
			l.dir = want
		}
	}
	if prev, dup := a.sessions[l.meta.ID]; dup {
		slog.Warn("session archive: two directories claim one session; keeping the longer history",
			"session", l.meta.ID, "dirs", []string{prev.dir, l.dir})
		if prev.meta.lastSeq() >= l.meta.lastSeq() {
			return
		}
	}
	s := &sessionState{dir: l.dir, meta: l.meta}
	a.sessions[l.meta.ID] = s
	a.lastSeq[l.meta.ID] = l.meta.lastSeq()
	if changed {
		a.writeMeta(s)
	}
}

// Record implements session.Recorder. It never blocks and never touches disk; see Archive.
func (a *Archive) Record(sessionID string, e *pipeline.SessionEvent) {
	if a.closed.Load() {
		return
	}
	a.mu.Lock()
	if e.Seq > a.lastSeq[sessionID] {
		a.lastSeq[sessionID] = e.Seq
	}
	a.mu.Unlock()
	// Every send happens under the store's write lock, so this check and the send below cannot
	// race another producer; the writer only ever makes room.
	if len(a.ops) >= cap(a.ops)-reserve {
		a.dropped.Add(1)
		return
	}
	select {
	case a.ops <- op{kind: opEvent, id: sessionID, ev: *e}:
	default:
		a.dropped.Add(1)
	}
}

// Rekeyed implements session.Rekeyer: the session's history follows its new id. The rename
// goes through the same queue as events, into the reserve the events may not use.
func (a *Archive) Rekeyed(oldID, newID string) {
	if a.closed.Load() {
		return
	}
	a.mu.Lock()
	if n, ok := a.lastSeq[oldID]; ok {
		delete(a.lastSeq, oldID)
		a.lastSeq[newID] = max(a.lastSeq[newID], n)
	}
	a.mu.Unlock()
	select {
	case a.ops <- op{kind: opRename, id: oldID, to: newID}:
	default:
		a.droppedRenames.Add(1)
		slog.Error("session archive: a rename was lost to a full queue; the session's history stays under its old id",
			"from", oldID, "to", newID)
	}
}

// LastSeq implements session.SeqSeeder. A memory lookup: it runs under the store's write lock.
func (a *Archive) LastSeq(sessionID string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSeq[sessionID]
}

// Stats reports the archive's size, bounds and losses.
func (a *Archive) Stats() Stats {
	return Stats{
		Bytes:          a.bytes.Load(),
		MaxBytes:       a.maxBytes,
		RetentionDays:  a.retainDays,
		DroppedEvents:  a.dropped.Load(),
		WriteErrors:    a.writeErrors.Load(),
		DroppedRenames: a.droppedRenames.Load(),
		Paused:         a.paused.Load(),
	}
}

// Root is the directory the archive was opened at.
func (a *Archive) Root() string { return a.root }

// RetentionDays is the retention in effect, after defaulting and clamping.
func (a *Archive) RetentionDays() int { return a.retainDays }

// Close drains the queue, finishes and fsyncs every open segment, writes every changed
// session.json and stops the writer. An orderly stop loses nothing that was queued.
func (a *Archive) Close() error {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		close(a.stop)
	})
	<-a.done
	return a.closeErr
}

// inWriter runs f on the writer goroutine and waits for it — how tests read and drive state the
// writer goroutine owns without racing it.
func (a *Archive) inWriter(f func()) {
	done := make(chan struct{})
	a.ops <- op{kind: opFunc, f: f, done: done}
	<-done
}

func (a *Archive) run() {
	defer close(a.done)
	t := time.NewTicker(a.tickEvery)
	defer t.Stop()
	for {
		select {
		case o := <-a.ops:
			a.take(o)
		case <-t.C:
			a.tick()
		case <-a.stop:
			for drained := false; !drained; {
				select {
				case o := <-a.ops:
					a.take(o)
				default:
					drained = true
				}
			}
			a.closeErr = a.shutdown()
			return
		}
	}
}

func (a *Archive) take(o op) {
	if a.gate != nil {
		<-a.gate
	}
	switch o.kind {
	case opEvent:
		a.write(o.id, &o.ev)
	case opRename:
		a.rename(o.id, o.to)
	case opFunc:
		o.f()
		close(o.done)
	}
}

// write appends one event to its session's open segment, opening one if needed.
func (a *Archive) write(id string, e *pipeline.SessionEvent) {
	if a.paused.Load() {
		a.dropped.Add(1)
		return
	}
	now := a.now()
	s := a.sessions[id]
	fresh := s == nil
	if fresh {
		s = &sessionState{
			dir:  filepath.Join(a.data, dirName(id)),
			meta: &sessionMeta{ID: id, CreatedAt: now, UpdatedAt: now, Summary: session.NewSummaryFold()},
		}
		a.sessions[id] = s
	}
	if s.w == nil {
		a.makeRoomForWriter()
		w, err := createSegment(s.dir, now)
		if err != nil {
			a.writeFailed(s, err)
			return
		}
		if a.segHook != nil {
			a.segHook(w)
		}
		s.w = w
	}
	if err := s.w.append(*e, now); err != nil {
		a.writeFailed(s, err)
		return
	}
	s.meta.Summary.Add(id, e)
	s.meta.UpdatedAt, s.lastWrite, s.metaDirty = now, now, true
	// A session's session.json exists from its first event, so a crash before the first
	// metaEvery still leaves a directory reconcile can read.
	if fresh {
		a.writeMeta(s)
	}
}

// makeRoomForWriter closes the least recently written segment when the cap is reached.
func (a *Archive) makeRoomForWriter() {
	var open []*sessionState
	for _, s := range a.sessions {
		if s.w != nil {
			open = append(open, s)
		}
	}
	if len(open) < a.maxOpen {
		return
	}
	oldest := slices.MinFunc(open, func(x, y *sessionState) int { return x.lastWrite.Compare(y.lastWrite) })
	a.closeWriter(oldest)
}

// closeWriter finishes s's open segment and moves it into the closed list.
func (a *Archive) closeWriter(s *sessionState) {
	if s.w == nil {
		return
	}
	err := s.w.close()
	a.retire(s)
	if err != nil {
		a.writeErrors.Add(1)
		slog.Warn("session archive: closing a segment failed", "session", s.meta.ID, "error", err)
	}
}

// retire moves s's writer, open or broken, into the closed list — or deletes its file when it
// never held an event.
func (a *Archive) retire(s *sessionState) {
	info := s.w.info()
	if info.Events > 0 {
		s.meta.Segments = append(s.meta.Segments, info)
	} else {
		os.Remove(filepath.Join(s.dir, info.File))
	}
	s.w = nil
	s.metaDirty = true
}

// writeFailed counts a failed write, closes the segment it hit, and pauses the archive when the
// disk is full. The session's next event opens a fresh segment.
func (a *Archive) writeFailed(s *sessionState, err error) {
	a.writeErrors.Add(1)
	slog.Warn("session archive: write failed; closing the segment", "session", s.meta.ID, "error", err)
	if s.w != nil {
		s.w.close()
		a.retire(s)
	}
	if errors.Is(err, syscall.ENOSPC) && !a.paused.Swap(true) {
		slog.Warn("session archive: the disk is full; pausing until the next retention pass",
			"effect", "events are not archived until then; live sessions are unaffected")
	}
}

// rename follows the store's rename of oldID to newID: session.json is rewritten with the new id
// first, then the directory moved. A crash between the two leaves a session.json naming the new
// id under the old directory, which adopt finishes at the next start.
func (a *Archive) rename(oldID, newID string) {
	s := a.sessions[oldID]
	if s == nil {
		return
	}
	newDir := filepath.Join(a.data, dirName(newID))
	if _, taken := a.sessions[newID]; taken {
		slog.Warn("session archive: rename target already has history; keeping it under the old id",
			"from", oldID, "to", newID)
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		slog.Warn("session archive: rename target directory exists; keeping history under the old id",
			"from", oldID, "to", newID)
		return
	}
	s.meta.ID = newID
	a.writeMeta(s)
	if err := os.Rename(s.dir, newDir); err != nil {
		slog.Warn("session archive: could not move a renamed session's directory; it will be moved at the next start",
			"from", oldID, "to", newID, "error", err)
	} else {
		s.dir = newDir
	}
	delete(a.sessions, oldID)
	a.sessions[newID] = s
}

// tick is the writer goroutine's clock: flush, idle close, session.json cadence, retention.
func (a *Archive) tick() {
	now := a.now()
	for _, s := range a.sessions {
		if s.w == nil {
			continue
		}
		if err := s.w.flush(); err != nil {
			a.writeFailed(s, err)
			continue
		}
		if now.Sub(s.lastWrite) >= a.idleClose {
			a.closeWriter(s)
		}
	}
	if now.Sub(a.lastMeta) >= metaEvery {
		for _, s := range a.sessions {
			if s.metaDirty {
				a.writeMeta(s)
			}
		}
		a.lastMeta = now
	}
	if now.Sub(a.lastPrune) >= pruneEvery {
		a.prune(now)
		a.lastPrune = now
		if a.paused.Swap(false) {
			slog.Info("session archive: resuming after a full disk")
		}
	}
	if d := a.dropped.Load(); d > a.reportedDrops && now.Sub(a.lastDropLog) >= time.Minute {
		slog.Warn("session archive: events were not archived", "dropped", d-a.reportedDrops, "droppedTotal", d)
		a.reportedDrops, a.lastDropLog = d, now
	}
	a.refreshBytes()
}

// prune deletes what retention says to, and every session left with nothing on disk.
func (a *Archive) prune(now time.Time) {
	var refs []segmentRef
	byDir := map[string]*sessionState{}
	for _, s := range a.sessions {
		byDir[s.dir] = s
		for _, seg := range s.meta.Segments {
			refs = append(refs, segmentRef{SessionDir: s.dir, Info: seg})
		}
		if s.w != nil {
			refs = append(refs, segmentRef{SessionDir: s.dir, Info: s.w.info(), Open: true})
		}
	}
	lastBefore := map[*sessionState]uint64{}
	for _, r := range planPrune(refs, now, a.retainDays, a.maxBytes) {
		s := byDir[r.SessionDir]
		if _, seen := lastBefore[s]; !seen {
			lastBefore[s] = s.meta.lastSeq()
		}
		if err := os.Remove(filepath.Join(r.SessionDir, r.Info.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("session archive: could not delete a segment retention expired", "file", r.Info.File, "error", err)
			continue
		}
		s.meta.Segments = slices.DeleteFunc(s.meta.Segments, func(x SegmentInfo) bool { return x.File == r.Info.File })
		s.metaDirty = true
	}
	for id, s := range a.sessions {
		if s.w != nil || len(s.meta.Segments) > 0 {
			continue
		}
		if err := os.RemoveAll(s.dir); err != nil {
			slog.Warn("session archive: could not remove an emptied session", "dir", s.dir, "error", err)
			continue
		}
		delete(a.sessions, id)
		// Forget the session's numbering only if nothing newer than what was on disk was
		// recorded since: an event still queued for it must keep its seq out of reuse.
		a.mu.Lock()
		if a.lastSeq[id] <= lastBefore[s] {
			delete(a.lastSeq, id)
		}
		a.mu.Unlock()
	}
	for _, s := range a.sessions {
		if s.metaDirty && len(lastBefore) > 0 {
			if _, touched := lastBefore[s]; touched {
				a.writeMeta(s)
			}
		}
	}
	a.refreshBytes()
}

// refreshBytes recomputes the archive's size from every session's segments.
func (a *Archive) refreshBytes() {
	var total int64
	for _, s := range a.sessions {
		for _, seg := range s.meta.Segments {
			total += seg.Bytes
		}
		if s.w != nil {
			total += s.w.info().Bytes
		}
	}
	a.bytes.Store(total)
}

// writeMeta writes s's session.json: the closed segments, the open one as it stands, and the
// summary — one snapshot, as sessionMeta requires.
func (a *Archive) writeMeta(s *sessionState) {
	m := *s.meta
	m.Segments = slices.Clone(s.meta.Segments)
	if s.w != nil {
		m.Segments = append(m.Segments, s.w.info())
	}
	if err := writeMeta(s.dir, &m); err != nil {
		a.writeErrors.Add(1)
		slog.Warn("session archive: could not write session.json", "session", s.meta.ID, "error", err)
		return
	}
	s.metaDirty = false
}

// shutdown closes every writer and writes every changed session.json.
func (a *Archive) shutdown() error {
	var errs []error
	for _, s := range a.sessions {
		if s.w == nil {
			continue
		}
		if err := s.w.close(); err != nil {
			errs = append(errs, fmt.Errorf("session %s: %w", s.meta.ID, err))
		}
		a.retire(s)
	}
	for _, s := range a.sessions {
		if s.metaDirty {
			a.writeMeta(s)
		}
	}
	return errors.Join(errs...)
}
