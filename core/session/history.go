package session

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
