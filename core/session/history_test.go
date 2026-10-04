package session

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// fakeArchive is a Recorder that remembers each session's last seq for longer than the store
// does, which is all the store asks of the session archive.
type fakeArchive struct{ last map[string]uint64 }

func newFakeArchive() *fakeArchive { return &fakeArchive{last: map[string]uint64{}} }

func (f *fakeArchive) Record(id string, e *pipeline.SessionEvent) { f.last[id] = e.Seq }
func (f *fakeArchive) LastSeq(id string) uint64                   { return f.last[id] }
func (f *fakeArchive) Rekeyed(o, n string) {
	f.last[n] = f.last[o]
	delete(f.last, o)
}

func seqsOf(t *testing.T, s *Store, id string) []uint64 {
	t.Helper()
	v := s.View(id)
	if v == nil {
		t.Fatalf("no session %q", id)
	}
	out := make([]uint64, len(v.Events))
	for i, e := range v.Events {
		out[i] = e.Seq
	}
	return out
}

func TestAppend_NoSeederStartsAtOne(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Append("s1", ev())
	s.Append("s1", ev())
	if got := seqsOf(t, s, "s1"); got[0] != 1 || got[1] != 2 {
		t.Fatalf("seqs = %v, want [1 2]", got)
	}
}

func TestAppend_NumbersANewEntryAfterTheSeedersLastSeq(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	a := newFakeArchive()
	a.last["s1"] = 41
	s.AddRecorder(a)
	s.Append("s1", ev())
	if got := seqsOf(t, s, "s1"); got[0] != 42 {
		t.Fatalf("first seq = %d, want 42 (after the archived 41)", got[0])
	}
}

func TestAppend_TakesTheHighestSeederAnswer(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	a, b := newFakeArchive(), newFakeArchive()
	a.last["s1"], b.last["s1"] = 5, 9
	s.AddRecorder(a)
	s.AddRecorder(b)
	s.Append("s1", ev())
	if got := seqsOf(t, s, "s1"); got[0] != 10 {
		t.Fatalf("first seq = %d, want 10", got[0])
	}
}

// The case the hook exists for: max_sessions evicts a session, the same id speaks again, and
// its events continue the numbering the archive already holds instead of restarting at 1 —
// which would give the session two events numbered 1 and make ?before= loop.
func TestAppend_ASessionReCreatedAfterEvictionContinuesItsSeq(t *testing.T) {
	s := New(0, 0, 1) // max_sessions 1
	defer s.Close()
	s.AddRecorder(newFakeArchive())
	s.Append("s1", ev())
	s.Append("s1", ev())
	s.Append("s2", ev()) // evicts s1
	if s.View("s1") != nil {
		t.Fatal("s1 still resident; the test needs it evicted")
	}
	s.Append("s1", ev())
	if got := seqsOf(t, s, "s1"); len(got) != 1 || got[0] != 3 {
		t.Fatalf("re-created s1 seqs = %v, want [3]", got)
	}
}
