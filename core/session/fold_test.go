package session

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// foldRecorder folds exactly what a Recorder is handed, which is what the archive will fold.
type foldRecorder struct{ folds map[string]*SummaryFold }

func (r *foldRecorder) Record(id string, e *pipeline.SessionEvent) {
	f := r.folds[id]
	if f == nil {
		f = NewSummaryFold()
		r.folds[id] = f
	}
	f.Add(id, e)
}

// One realistic mixture rather than one fixture per rule, for TestSumCost's reason: the rules
// interact, and what matters is the sum. Contentless events sit between the turns because live
// traffic interleaves them (gotcha #14).
func foldFixture(t *testing.T) []pipeline.SessionEvent {
	t.Helper()
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	turn := func(content string, tokens int, rec event.Event, c *pipeline.EventClient) []pipeline.SessionEvent {
		req := titleEvent(content)
		req.Phase, req.Client = pipeline.SessionRequest, c
		tunnel := pipeline.SessionEvent{Phase: pipeline.SessionRequest, Tunnel: true, HTTPMethod: "CONNECT"}
		resp := pipeline.SessionEvent{
			Phase:     pipeline.SessionResponse,
			Client:    c,
			Inference: &pipeline.InferenceExtension{TotalTokens: tokens},
			Plugins:   costRecord(t, rec),
		}
		return []pipeline.SessionEvent{req, tunnel, resp}
	}
	var evs []pipeline.SessionEvent
	evs = append(evs, turn("fix the flaky test in the session store", 1200,
		event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured",
			Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 100, USD: 0.01, Tier: "input"}}}, claude)...)
	evs = append(evs, turn("now explain why it was flaky", 800,
		event.Event{CostUSD: 0.03, Settled: true, Currency: "Bobcoins"}, bob)...)
	evs = append(evs, pipeline.SessionEvent{Phase: pipeline.SessionDenied,
		Plugins: costRecord(t, event.Event{CostUSD: 0.05, Settled: true, Provenance: "authoritative"})})
	evs = append(evs, turn("and the third thing", 0,
		event.Event{Source: event.SourceUsageFallback,
			Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 200, USD: 0.02, Tier: "input"}}}, claude)...)
	return evs
}

// The archive reports a disk-only session's figures from a SummaryFold, and the session API
// shows them beside resident sessions' ListSessions figures. For an untrimmed, unclaimed
// session the two must be equal, or one column means two things depending on where a row
// came from.
func TestSummaryFold_AgreesWithListSessions(t *testing.T) {
	s := New(5*time.Minute, 0, 0)
	defer s.Close()
	r := &foldRecorder{folds: map[string]*SummaryFold{}}
	s.AddRecorder(r)
	for _, e := range foldFixture(t) {
		s.Append("s1", e)
	}

	want := summaryOf(t, s, "s1")
	got := r.folds["s1"].Summary("s1")

	if got.EventCount != want.EventCount || got.Title != want.Title || got.Agent != want.Agent ||
		got.TotalTokens != want.TotalTokens || got.CostMicros != want.CostMicros ||
		got.AvoidedMicros != want.AvoidedMicros || got.Saturated != want.Saturated ||
		!slices.Equal(got.Currencies, want.Currencies) {
		t.Fatalf("fold  = %+v\nstore = %+v", got, want)
	}
	if want.CostMicros == 0 || want.AvoidedMicros == 0 || want.Title == "" || want.Agent == "" ||
		len(want.Currencies) != 2 {
		t.Fatalf("fixture no longer exercises every figure: %+v", want)
	}
}

// The default and pending buckets name no agent, in the fold as in the store.
func TestSummaryFold_NamesNoAgentForTheDefaultAndPendingBuckets(t *testing.T) {
	for _, id := range []string{DefaultSessionID, PendingSessionID("claude-code")} {
		f := NewSummaryFold()
		f.Add(id, &pipeline.SessionEvent{Client: &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}})
		if a := f.Summary(id).Agent; a != "" {
			t.Errorf("%s: Agent = %q, want empty", id, a)
		}
	}
}

// The archive persists a fold in session.json and resumes it after a restart, so a fold carried
// through JSON and then given more events must report exactly what one fold over all of them
// does — including the title rank, which decides whether a later /rename may replace the title.
func TestSummaryFold_ResumesFromJSON(t *testing.T) {
	evs := foldFixture(t)
	whole := NewSummaryFold()
	for i := range evs {
		whole.Add("s1", &evs[i])
	}

	first := NewSummaryFold()
	for i := range evs[:4] {
		first.Add("s1", &evs[i])
	}
	b, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	resumed := NewSummaryFold()
	if err := json.Unmarshal(b, resumed); err != nil {
		t.Fatal(err)
	}
	for i := range evs[4:] {
		resumed.Add("s1", &evs[4+i])
	}
	w, g := whole.Summary("s1"), resumed.Summary("s1")
	if w.EventCount != g.EventCount || w.Title != g.Title || w.Agent != g.Agent || w.TotalTokens != g.TotalTokens ||
		w.CostMicros != g.CostMicros || w.AvoidedMicros != g.AvoidedMicros || !slices.Equal(w.Currencies, g.Currencies) {
		t.Fatalf("resumed = %+v\nwhole   = %+v", g, w)
	}
}

// rankRename is 0, so a decoded fold with no titleRank must not claim it was renamed — that
// would lock the title against every later candidate but a /rename.
func TestSummaryFold_AMissingTitleRankIsNotARename(t *testing.T) {
	f := NewSummaryFold()
	if err := json.Unmarshal([]byte(`{"eventCount":3}`), f); err != nil {
		t.Fatal(err)
	}
	f.Add("s1", ptr(titleEvent("a title that should win")))
	if got := f.Summary("s1").Title; got != "a title that should win" {
		t.Fatalf("Title = %q, want the candidate", got)
	}
}

func ptr[T any](v T) *T { return &v }
