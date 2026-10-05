package usage

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestCleared_DropsPerSessionFiguresAndKeepsGlobalTotals pins what DELETE /v1/sessions relies on.
// After a clear, no figure is answerable by session id — not the per-session ring and not the
// session breakdown of the all-sessions or any agent's ring — because the ids are what the user
// erased. The totals stay: what was spent was spent, and the cost ledger keeps the same figures
// on disk anyway.
func TestCleared_DropsPerSessionFiguresAndKeepsGlobalTotals(t *testing.T) {
	a := New()
	e := inferenceEvent("m", 13, 0, 0, 5, 0, 0b1001)
	e.Client = &pipeline.EventClient{Name: "claude-code", Version: "1.0"}
	a.Record("s1", e)
	a.Record("s2", inferenceEvent("m", 29, 0, 0, 7, 0, 0b1001))

	a.Cleared()

	if got := a.Snapshot(10*time.Minute, BucketWidth, "", GroupNone).Totals.InputTokens; got != 42 {
		t.Errorf("all-sessions InputTokens = %d, want 42 kept", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone).Totals.InputTokens; got != 0 {
		t.Errorf("session=s1 InputTokens = %d, want 0 after a clear", got)
	}
	if all := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets); len(all) != 0 {
		t.Errorf("all-sessions breakdown still names sessions: %v", all)
	}
	if s := mergeSeries(a.AgentSnapshot(10*time.Minute, BucketWidth, "", "claude-code", GroupSession).Buckets); len(s) != 0 {
		t.Errorf("claude-code's ring still names sessions: %v", s)
	}
	if got := a.AgentSnapshot(10*time.Minute, BucketWidth, "", "claude-code", GroupNone).Totals.InputTokens; got != 13 {
		t.Errorf("claude-code's total = %d, want 13 kept", got)
	}

	// A session recorded after the clear is answerable as usual.
	a.Record("s1", inferenceEvent("m", 3, 0, 0, 1, 0, 0b1001))
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone).Totals.InputTokens; got != 3 {
		t.Errorf("session=s1 after the clear = %d, want 3", got)
	}
}
