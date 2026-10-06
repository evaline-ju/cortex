package tui

import (
	"math"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1199: an opaque tunnel now records a response row when it closes, carrying
// a status, how long the tunnel stayed open, and the bytes it moved each way.

// A close row carries the Tunnel marker too, so the fold must key on the open only.
// Otherwise a close followed by a request to the same host — a client that tunnels and
// then makes a plain call — would swallow that request into a row keyed on it, and the
// close would vanish from the timeline.
func TestBuildEventRows_TunnelCloseNeverFolds(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com:443",
			Tunnel: true, RequestID: "t"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "api.example.com:443",
			Tunnel: true, RequestID: "t", StatusCode: 200},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com", RequestID: "r"},
	}
	rows := buildEventRows(events)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 — nothing here is a bridged pair", len(rows))
	}
	for i, r := range rows {
		if r.tunnel != nil {
			t.Errorf("row %d folded a tunnel into itself", i)
		}
	}
}

// Only a tunnel that carried a decrypted request folds into one. An open whose reason says
// why the bytes stayed opaque carried none, and neither did one with a close row: the proxy
// records a close only for a tunnel no decrypted request answered. Either kind is its own
// exchange even when another connection's request to the same host lands right after it.
func TestBuildEventRows_OnlyABridgedOpenFolds(t *testing.T) {
	open := func(reason pipeline.TunnelReason) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host: "api.example.com:443", Tunnel: true, TunnelReason: reason, RequestID: "t"}
	}
	close := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: "api.example.com:443", Tunnel: true, RequestID: "t", StatusCode: 200}
	other := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "api.example.com", RequestID: "r"}

	for _, tc := range []struct {
		name     string
		events   []pipeline.SessionEvent
		wantFold bool
	}{
		{"opaque open, still open", []pipeline.SessionEvent{open(pipeline.TunnelPassthroughHost), other}, false},
		{"opaque open, closed", []pipeline.SessionEvent{open(pipeline.TunnelSkipCached), other, close}, false},
		// The transparent listener records every open with no reason, so the close row is
		// the only thing that says this one stayed opaque.
		{"reasonless open with a close", []pipeline.SessionEvent{open(""), other, close}, false},
		{"bridged open", []pipeline.SessionEvent{open(""), other}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := buildEventRows(tc.events)
			folded := len(rows) > 0 && rows[0].tunnel != nil
			if folded != tc.wantFold {
				t.Fatalf("folded = %v, want %v (%d rows)", folded, tc.wantFold, len(rows))
			}
			if tc.wantFold || len(rows) < 3 {
				return
			}
			ids, _ := computeEventPairs(rows)
			if ids[rows[0].event] != ids[rows[2].event] {
				t.Errorf("the open is exchange %d and its close %d; want one exchange",
					ids[rows[0].event], ids[rows[2].event])
			}
		})
	}
}

// A tunnel's close lands when it ends, which can be long after its session last spoke.
// It is not that session speaking, so a streamed close must not move the session up the
// list or refresh its age, just as the proxy's own list does not.
func TestHandleStreamEvent_TunnelCloseDoesNotRefreshTheSession(t *testing.T) {
	m := newRetentionModel(t, "sess-A", 0)
	m.pane = paneSessions
	t0 := time.Now().Add(-time.Hour)
	t1 := t0.Add(time.Minute)
	m.sessions = []session.SessionSummary{{ID: "sess-B", UpdatedAt: t1}, {ID: "sess-A", UpdatedAt: t0}}

	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
		At: time.Now(), SessionID: "sess-A", Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: "kube:6443", Tunnel: true, RequestID: "tun", StatusCode: 200,
	}})
	// The list is re-sorted on the tick, not on the event. Without the flush nothing sorts it
	// and the order below would hold whatever the close did.
	m.flushSessionsTable()

	if m.sessions[0].ID != "sess-B" {
		t.Errorf("session list leads with %q after a tunnel close; want sess-B, which spoke last", m.sessions[0].ID)
	}
	for _, s := range m.sessions {
		if s.ID == "sess-A" && !s.UpdatedAt.Equal(t0) {
			t.Errorf("sess-A UpdatedAt moved from %v to %v on a tunnel close", t0, s.UpdatedAt)
		}
	}
	if got := len(m.events["sess-A"]); got != 1 {
		t.Errorf("sess-A holds %d events after the close streamed in, want 1: the row itself still lands", got)
	}
}

// What makes the close row useful with no agentop-side pairing work: it shares the open's
// RequestID, and the # column pairs on that across whatever landed in between. Pinned
// because the proxy side of #1199 relies on it rather than adding anything here.
func TestComputeEventPairs_TunnelCloseJoinsItsOpen(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Phase: pipeline.SessionRequest, Host: "kube:6443", Tunnel: true, RequestID: "tun"},
		{Phase: pipeline.SessionRequest, Host: "github.example.com", RequestID: "a"},
		{Phase: pipeline.SessionResponse, Host: "github.example.com", RequestID: "a", StatusCode: 200},
		{Phase: pipeline.SessionResponse, Host: "kube:6443", Tunnel: true, RequestID: "tun", StatusCode: 200},
	}
	rows := buildEventRows(events)
	ids, _ := computeEventPairs(rows)
	if ids[rows[0].event] != ids[rows[3].event] {
		t.Errorf("tunnel open is exchange %d and its close %d; want one exchange",
			ids[rows[0].event], ids[rows[3].event])
	}
	if ids[rows[0].event] == ids[rows[1].event] {
		t.Error("the tunnel and the request that interleaved with it share an exchange number")
	}
}

// A tunnel's DURATION is how long it stayed open, which for `kubectl logs -f` is
// minutes. "252.00s" makes a reader do the division; minutes and seconds do not.
func TestDurationCell_MinutesAndHours(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{12 * time.Millisecond, "12ms"},
		{1200 * time.Millisecond, "1.20s"},
		{59990 * time.Millisecond, "59.99s"},
		{time.Minute, "1m00s"},
		{102450 * time.Millisecond, "1m42s"},
		{252 * time.Second, "4m12s"},
		{time.Hour + 2*time.Minute + 5*time.Second, "1h02m"},
		// Every tier rounds to what it shows, the hours tier included.
		{time.Hour + 2*time.Minute + 59*time.Second, "1h03m"},
		{time.Hour + 59*time.Minute + 30*time.Second, "2h00m"},
		{59*time.Minute + 59500*time.Millisecond, "1h00m"},
	} {
		if got := durationCell(pipeline.SessionEvent{Duration: tc.d}); got != tc.want {
			t.Errorf("durationCell(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestBytesCell(t *testing.T) {
	for _, tc := range []struct {
		name     string
		up, down int64
		want     string
	}{
		{"not a tunnel close", 0, 0, ""},
		{"both ways", 4210, 18230, "↑4.2kB ↓18.2kB"},
		{"small counts stay exact", 5, 7, "↑5B ↓7B"},
		// Counts are absent on the wire when zero, so one side at zero is shown as 0
		// once the other proves this row carries counts at all.
		{"nothing sent", 0, 512, "↑0B ↓512B"},
		// A count formatter renders this "1.5B", which reads as one and a half bytes.
		{"gigabytes", 1_500_000_000, 1_500_000_000, "↑1.5GB ↓1.5GB"},
		{"carries at the tier boundary", 999_950, 999_949, "↑1.0MB ↓999.9kB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bytesCell(pipeline.SessionEvent{BytesUp: tc.up, BytesDown: tc.down}); got != tc.want {
				t.Errorf("bytesCell(up=%d, down=%d) = %q, want %q", tc.up, tc.down, got, tc.want)
			}
		})
	}
}

// Off by default: only opaque tunnels' close rows have a figure, so on by default it
// would spend columns of every terminal on a mostly blank column.
func TestBytesColumn_IsOptInAndSortsByTotal(t *testing.T) {
	var col *eventColumn
	for i := range eventColumns {
		if eventColumns[i].id == colBytes {
			col = &eventColumns[i]
		}
	}
	if col == nil {
		t.Fatal("no BYTES column")
	}
	if col.defaultOn {
		t.Error("BYTES is on by default; it should be opt-in")
	}
	if col.sortKey == nil {
		t.Fatal("BYTES has no sort key")
	}
	small := col.sortKey(cellContext{row: eventRow{event: &pipeline.SessionEvent{BytesUp: 1, BytesDown: 1}}})
	big := col.sortKey(cellContext{row: eventRow{event: &pipeline.SessionEvent{BytesDown: 1000}}})
	if !small.less(big) {
		t.Error("BYTES does not sort by the total carried")
	}
}

func eventColumnByID(t *testing.T, id eventColumnID) eventColumn {
	t.Helper()
	for _, c := range eventColumns {
		if c.id == id {
			return c
		}
	}
	t.Fatalf("no %s column", id)
	return eventColumn{}
}

// bubbles truncates a cell at its column's width, so BYTES must fit the widest cell any
// int64 pair can produce, on every tier.
func TestBytesColumn_FitsItsWidestCell(t *testing.T) {
	width := eventColumnByID(t, colBytes).width
	for _, n := range []int64{999, 999_949, 999_949_999, 999_949_999_999, 999_949_999_999_999,
		999_949_999_999_999_999, math.MaxInt64} {
		cell := bytesCell(pipeline.SessionEvent{BytesUp: n, BytesDown: n})
		if w := utf8.RuneCountInString(cell); w > width {
			t.Errorf("bytesCell(%d, %d) = %q is %d wide; BYTES is %d", n, n, cell, w, width)
		}
	}
}

// DURATION answers "which calls took longest". A tunnel's DURATION is how long it stayed
// open — minutes for `kubectl logs -f` — so it ranks with the blank rows rather than
// leading every descending sort.
func TestDurationColumn_RanksTunnelRowsBlank(t *testing.T) {
	col := eventColumnByID(t, colDuration)
	key := func(e pipeline.SessionEvent) sortValue { return col.sortKey(cellContext{row: eventRow{event: &e}}) }

	tunnelClose := key(pipeline.SessionEvent{Phase: pipeline.SessionResponse, Tunnel: true, Duration: time.Hour})
	blank := key(pipeline.SessionEvent{Phase: pipeline.SessionResponse})
	slowCall := key(pipeline.SessionEvent{Phase: pipeline.SessionResponse, Duration: 2 * time.Second})

	if tunnelClose.less(blank) || blank.less(tunnelClose) {
		t.Error("a tunnel close does not rank with the blank rows")
	}
	if !tunnelClose.less(slowCall) {
		t.Error("an hour-long tunnel ranks above a 2s call; a descending DURATION sort would lead with it")
	}
}
