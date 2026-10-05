package tui

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// H shows sessions the proxy no longer holds — the session archive's — and only from the
// sessions pane, where that list is.
func TestHKey_TogglesHistoryOnTheSessionsPaneOnly(t *testing.T) {
	m := &model{pane: paneSessions, client: deadClient()}
	if cmd := m.handleKey(keyRune('H')); !m.showHistory || cmd == nil {
		t.Fatalf("H on sessions: showHistory=%v cmd=%v, want on and a reload", m.showHistory, cmd != nil)
	}
	m.handleKey(keyRune('H'))
	if m.showHistory {
		t.Fatal("a second H did not turn history off")
	}
	m.pane = paneEvents
	m.handleKey(keyRune('H'))
	if m.showHistory {
		t.Fatal("H toggled history from the events pane")
	}
}

// A disk-only row says so in the column where cached-only rows already say "cached": it is the
// one cell answering why the row has no live session behind it.
func TestHistoryLoaded_MarksDiskOnlyRowsArchived(t *testing.T) {
	m := newTitleModel(t, nil)
	m.showHistory = true
	no := false
	now := time.Now()
	m.Update(historyLoadedMsg{list: apiclient.SessionList{
		Sessions: []session.SessionSummary{
			{ID: "live-1", UpdatedAt: now},
			{ID: "old-2", UpdatedAt: now.Add(-48 * time.Hour), Resident: &no, EventCount: 7},
		},
		Archive: &pipeline.ArchiveUsage{Bytes: 4 << 20, MaxBytes: 2 << 30, RetentionDays: 30},
	}})
	if got := strings.Join(m.sessionRowIDs, ","); got != "live-1,old-2" {
		t.Fatalf("rows %s, want live-1,old-2", got)
	}
	rows := m.sessionsTbl.Rows()
	if !rowHas(rows[1], archivedMarker) || rowHas(rows[0], archivedMarker) {
		t.Fatalf("archived marker on the wrong rows: %v / %v", rows[0], rows[1])
	}
	if m.archiveUsage == nil || m.archiveUsage.Bytes != 4<<20 {
		t.Fatalf("archive usage not kept: %+v", m.archiveUsage)
	}
}

// Against a proxy that has no archive — or predates it, and ignores ?archived=true — the list
// comes back with no archive object. Say so once, instead of showing an unchanged list as if
// history were on.
func TestHistoryLoaded_SaysOnceWhenTheProxyHasNoArchive(t *testing.T) {
	m := newTitleModel(t, nil)
	m.showHistory = true
	m.Update(historyLoadedMsg{list: apiclient.SessionList{Sessions: []session.SessionSummary{{ID: "live-1"}}}})
	if !strings.Contains(m.flash, "no session archive") {
		t.Fatalf("flash = %q, want it to say there is no archive", m.flash)
	}
	m.flash = ""
	m.Update(historyLoadedMsg{list: apiclient.SessionList{Sessions: []session.SessionSummary{{ID: "live-1"}}}})
	if m.flash != "" {
		t.Fatalf("the notice repeated on the next poll: %q", m.flash)
	}
}

func rowHas(row []string, cell string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) == cell {
			return true
		}
	}
	return false
}

// The whole path, against a real session API with a real archive: history from a previous
// process, H to list it, Enter on its row, and the timeline arrives — through the same snapshot
// agentop uses for a live session, since the server pages into the archive for it.
func TestHistory_EnterOnAnArchivedRowOpensItsTimeline(t *testing.T) {
	resetSettingsForTest(t) // New seeds the filter from Settings; another test's must not hide the row
	root := t.TempDir()
	prev, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prevStore := session.New(0, 0, 0)
	prevStore.AddRecorder(prev)
	for range 4 {
		prevStore.Append("old", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Host: "api.example.com"})
	}
	prevStore.Close()
	prev.Close()

	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.AddRecorder(a)
	ts := httptest.NewServer(sessionapi.New(":0", store, sessionapi.WithArchive(a)).Server().Handler)
	defer ts.Close()

	m := New(context.Background(), apiclient.New(ts.URL)).(*model)
	m.width, m.height = 120, 40
	m.pane = paneSessions
	m.Update(m.handleKey(keyRune('H'))())
	idx := slices.Index(m.sessionRowIDs, "old")
	if idx < 0 {
		t.Fatalf("the archived session is not listed with history on: %v", m.sessionRowIDs)
	}
	m.sessionsTbl.SetCursor(idx)
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on an archived row fetched nothing")
	}
	m.Update(cmd())
	if n := len(m.events["old"]); n != 4 {
		t.Fatalf("the archived timeline holds %d events, want 4", n)
	}
}

// Adding [H] must not cost the cost keys their place at 80 columns: helpView's comments promise
// [u] and [$] survive that cut, and fitHintLine drops hints from the front.
func TestSessionsFooter_HDoesNotPushTheCostKeysOffAt80Columns(t *testing.T) {
	m := &model{pane: paneSessions}
	line := fitHintLine(m.helpView(), 80)
	for _, want := range []string{"[u] usage", "[$] spend", "[?] keys", "[q] quit"} {
		if !strings.Contains(line, want) {
			t.Errorf("at 80 columns the sessions footer lost %q: %q", want, line)
		}
	}
	if wide := fitHintLine(m.helpView(), 120); !strings.Contains(wide, "[H] history") {
		t.Errorf("at 120 columns the footer does not advertise [H]: %q", wide)
	}
}
