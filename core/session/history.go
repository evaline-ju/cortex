package session

import "time"

// SeqSeeder is optionally implemented by a Recorder that keeps a session's events for longer
// than the store does — the session archive. When the store creates an entry it asks every
// SeqSeeder, under its write lock, for the last seq it holds under that id, and numbers the
// entry's events after the highest answer.
//
// Without it a session re-created under the same id — after max_sessions evicted it, a ttl
// expired it, or the proxy restarted — numbers its events from 1 again, so one session
// would carry two events numbered 1: one on disk, one in memory. A pager walking ?before=
// across that point would loop.
//
// LastSeq is called under the store's write lock, in front of live traffic: it must be a
// memory lookup, and must not block or call back into the store.
type SeqSeeder interface {
	LastSeq(sessionID string) uint64
}

// archivedSeqLocked is the highest seq any SeqSeeder holds for id, 0 when none does. s.mu held.
func (s *Store) archivedSeqLocked(id string) uint64 {
	var last uint64
	for _, r := range s.recorders {
		if sd, ok := r.(SeqSeeder); ok {
			if n := sd.LastSeq(id); n > last {
				last = n
			}
		}
	}
	return last
}

// EntryStarter is optionally implemented by a SeqSeeder. The store calls EntryStarted under its
// write lock each time it creates an entry for sessionID, with the seq it numbers that entry's
// events after. A rename of the entry carries the events numbered after it; those at or below
// it were recorded under sessionID before this entry existed, and stay there. Like LastSeq, it
// must be a memory update.
type EntryStarter interface {
	EntryStarted(sessionID string, after uint64)
}

// entryStartedLocked tells every EntryStarter that an entry for id numbers after `after`. s.mu held.
func (s *Store) entryStartedLocked(id string, after uint64) {
	for _, r := range s.recorders {
		if es, ok := r.(EntryStarter); ok {
			es.EntryStarted(id, after)
		}
	}
}

// Clearer is optionally implemented by a Recorder that keeps state keyed by session id: the
// usage aggregator's per-session figures, the archive's numbering and files. Clear tells each one,
// under the store's write lock and after the store has dropped every session, so no append can
// land between the two halves and be kept by one side only.
//
// Cleared runs in front of live traffic, like Record: it must not block. Work that takes time —
// the archive's file deletion — is queued for the Clearer's own goroutine.
type Clearer interface {
	Cleared()
}

// Clear removes every session, with every map that names one — owners, adoptions, process
// claims and the active session — tells every Clearer, and reports how many sessions it removed.
// The next append to any id starts a new session, numbered from 1 once the archive has reset.
//
// Subscribers stay attached: a clear erases history, it does not end the stream.
func (s *Store) Clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sessions)
	s.sessions = make(map[string]*entry)
	s.activeID = ""
	// nil, as a store nothing has claimed from holds them; every writer allocates on first use.
	s.owners, s.adopted = nil, nil
	s.procs, s.lastProcClaim = nil, time.Time{}
	for _, r := range s.recorders {
		if c, ok := r.(Clearer); ok {
			c.Cleared()
		}
	}
	return n
}
