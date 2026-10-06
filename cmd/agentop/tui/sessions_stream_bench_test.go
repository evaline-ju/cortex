package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// streamBenchModel is a sessions pane listing n sessions, newest first, its table built.
func streamBenchModel(n int) *model {
	now := time.Now()
	m := &model{pane: paneSessions, width: 200, height: 40,
		events: map[string][]pipeline.SessionEvent{}, sessionsTbl: newSessionsTable()}
	for i := range n {
		m.sessions = append(m.sessions, session.SessionSummary{
			ID: fmt.Sprintf("%08d-0000-4000-8000-000000000000", i), Title: fmt.Sprintf("session %d", i),
			UpdatedAt: now.Add(-time.Duration(i) * time.Minute), EventCount: 100 + i, TotalTokens: 1000 * i,
			Agent: "claude-code",
		})
	}
	m.rebuildSessionsTable()
	return m
}

// BenchmarkHandleStreamEvent is one streamed event against a list of n sessions. With history
// listed n is ~2,000 on a laptop (thirty days), where re-sorting and rebuilding the table on every
// event cost 6.4ms and 1.9MB, against 0.55ms at 10 rows (measured 2026-10-05). The rebuild now
// waits for the 1s tick, so its cost is gone from here. What still grows with n is the linear
// lookup of the event's session in the list, and the target sits at n/2.
func BenchmarkHandleStreamEvent(b *testing.B) {
	for _, n := range []int{10, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			m := streamBenchModel(n)
			ev := &pipeline.SessionEvent{SessionID: m.sessions[n/2].ID, Phase: pipeline.SessionRequest}
			b.ReportAllocs()
			for b.Loop() {
				ev.At = time.Now()
				m.handleStreamEvent(apiclient.StreamEvent{Event: ev})
				// Truncated, capacity kept: left to grow over millions of iterations it is GBs of heap.
				m.events[ev.SessionID] = m.events[ev.SessionID][:0]
			}
		})
	}
}

// A streamed event marks the list stale instead of rebuilding it, and the next tick re-sorts and
// rebuilds once, however many events arrived in between.
func TestStreamEvent_RebuildsTheSessionsTableOnTheNextTick(t *testing.T) {
	m := streamBenchModel(3)
	last := m.sessions[2].ID
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
		SessionID: last, At: time.Now(), Phase: pipeline.SessionRequest}})
	if m.sessionRowIDs[0] == last {
		t.Fatal("the table was rebuilt on the event itself")
	}
	m.Update(tickMsg(time.Now()))
	if m.sessionRowIDs[0] != last {
		t.Fatalf("after the tick the newest session is not first: %v", m.sessionRowIDs)
	}
}
