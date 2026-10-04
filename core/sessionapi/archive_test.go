package sessionapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// restarted builds what a proxy looks like after a restart: an archive holding s1's first ten
// events and old's three, written by a previous process, and a fresh store numbering from it.
func restarted(t *testing.T) (*httptest.Server, *session.Store, *archive.Archive) {
	t.Helper()
	root := t.TempDir()
	prev, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prevStore := session.New(0, 0, 0)
	prevStore.AddRecorder(prev)
	for i := range 10 {
		prevStore.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Host: hostN(i)})
	}
	for range 3 {
		prevStore.Append("old", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	}
	prevStore.Close()
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	srv := New(":0", store, WithArchive(a))
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
		a.Close()
	})
	return ts, store, a
}

func hostN(i int) string { return "h" + string(rune('a'+i)) }

func seqsOf(v *pipeline.SessionView) []uint64 {
	out := make([]uint64, len(v.Events))
	for i, e := range v.Events {
		out[i] = e.Seq
	}
	return out
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

// A session no longer in memory is served from disk, paged the same way a resident one is.
func TestHandleGet_ServesAnArchiveOnlySession(t *testing.T) {
	ts, _, _ := restarted(t)
	if v := getView(t, ts.URL, "/v1/sessions/s1"); !equalSeqs(seqsOf(v), []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) ||
		v.TotalEvents != 0 || v.OldestSeq != 0 {
		t.Fatalf("whole session: seqs %v total %d oldest %d", seqsOf(v), v.TotalEvents, v.OldestSeq)
	}
	v := getView(t, ts.URL, "/v1/sessions/s1?limit=4")
	if !equalSeqs(seqsOf(v), []uint64{7, 8, 9, 10}) || v.TotalEvents != 10 || v.OldestSeq != 1 {
		t.Fatalf("tail page: seqs %v total %d oldest %d", seqsOf(v), v.TotalEvents, v.OldestSeq)
	}
	if v := getView(t, ts.URL, "/v1/sessions/s1?limit=3&before=7"); !equalSeqs(seqsOf(v), []uint64{4, 5, 6}) {
		t.Fatalf("before=7: %v, want [4 5 6]", seqsOf(v))
	}
}

// The case the feature exists for: a session resumed after a restart is a few events in memory
// over a history on disk. One page spans both, oldest first, with no seq repeated.
func TestHandleGet_AResumedSessionPagesIntoItsHistory(t *testing.T) {
	ts, store, _ := restarted(t)
	for range 3 {
		store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	}
	v := getView(t, ts.URL, "/v1/sessions/s1?limit=5")
	if !equalSeqs(seqsOf(v), []uint64{9, 10, 11, 12, 13}) || v.OldestSeq != 1 || v.TotalEvents < 10 {
		t.Fatalf("seqs %v total %d oldest %d, want [9..13] from seq 1", seqsOf(v), v.TotalEvents, v.OldestSeq)
	}
	if v := getView(t, ts.URL, "/v1/sessions/s1?limit=3&before=11"); !equalSeqs(seqsOf(v), []uint64{8, 9, 10}) {
		t.Fatalf("before=11: %v, want [8 9 10] from disk", seqsOf(v))
	}
}

// The detail pane fetches one event by seq; one that is only on disk is still found, and one
// that exists nowhere is a 404, never a neighbour.
func TestHandleGetEvent_FindsAnEventOnDisk(t *testing.T) {
	ts, _, _ := restarted(t)
	code, body := getBody(t, ts.URL+"/v1/sessions/s1/events/3")
	var e pipeline.SessionEvent
	if code != http.StatusOK || json.Unmarshal(body, &e) != nil || e.Seq != 3 || e.SessionID != "s1" {
		t.Fatalf("GET events/3 = %d %s", code, body)
	}
	if code, _ := getBody(t, ts.URL+"/v1/sessions/s1/events/99"); code != http.StatusNotFound {
		t.Fatalf("GET events/99 = %d, want 404", code)
	}
}

type listBody struct {
	Sessions []session.SessionSummary `json:"sessions"`
	Archive  *pipeline.ArchiveUsage   `json:"archive"`
}

func getList(t *testing.T, url string) listBody {
	t.Helper()
	code, body := getBody(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", url, code)
	}
	var lb listBody
	if err := json.Unmarshal(body, &lb); err != nil {
		t.Fatal(err)
	}
	return lb
}

// The default list is unchanged — resident only, no archive object — so a client that does not
// ask sees exactly what it saw before.
func TestHandleList_ResidentOnlyByDefault(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	lb := getList(t, ts.URL+"/v1/sessions")
	if len(lb.Sessions) != 1 || lb.Sessions[0].ID != "live" || lb.Archive != nil {
		t.Fatalf("default list = %+v", lb)
	}
}

// ?archived=true adds what is only on disk, marked resident: false, with the archive's figures
// and usage; a session in both appears once, as its resident row.
func TestHandleList_ArchivedTrueAddsDiskOnlyRows(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	lb := getList(t, ts.URL+"/v1/sessions?archived=true")
	rows := map[string]session.SessionSummary{}
	for _, s := range lb.Sessions {
		if _, dup := rows[s.ID]; dup {
			t.Fatalf("%s listed twice", s.ID)
		}
		rows[s.ID] = s
	}
	if r, ok := rows["s1"]; !ok || r.Resident != nil {
		t.Fatalf("s1 should be its resident row: %+v", r)
	}
	old, ok := rows["old"]
	if !ok || old.Resident == nil || *old.Resident || old.EventCount != 3 {
		t.Fatalf("old should be a disk-only row with 3 events: %+v", old)
	}
	if lb.Archive == nil || lb.Archive.Bytes <= 0 || lb.Archive.MaxBytes <= 0 || lb.Archive.RetentionDays != 30 {
		t.Fatalf("archive usage = %+v", lb.Archive)
	}
}

// Without an archive the session API behaves exactly as before, ?archived=true included.
func TestHandleList_WithoutAnArchiveIgnoresArchived(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	lb := getList(t, ts.URL+"/v1/sessions?archived=true")
	if len(lb.Sessions) != 1 || lb.Archive != nil {
		t.Fatalf("list = %+v", lb)
	}
}
