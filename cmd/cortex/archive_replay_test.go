package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// recordTraffic runs one process's worth of traffic — two sessions interleaved, each turn a
// request carrying a request-phase plugin and a priced response — through a store whose recorders
// are agg and an archive at root, then stops that process.
func recordTraffic(t *testing.T, root string, agg *usage.Aggregator, now time.Time) {
	t.Helper()
	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	if agg != nil {
		store.AddRecorder(agg)
	}
	store.AddRecorder(arch)
	for i, s := range []string{"a", "b", "a", "b"} {
		at := now.Add(time.Duration(i-5) * time.Minute)
		rid := fmt.Sprintf("r%d", i)
		raw, err := json.Marshal(event.Event{CostUSD: 0.01 * float64(i+1), Settled: true, Provenance: "configured"})
		if err != nil {
			t.Fatal(err)
		}
		store.Append(s, pipeline.SessionEvent{At: at, Phase: pipeline.SessionRequest, RequestID: rid,
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "tool-prune", Phase: pipeline.InvocationPhaseRequest}}}})
		store.Append(s, pipeline.SessionEvent{At: at.Add(time.Second), Phase: pipeline.SessionResponse,
			RequestID: rid, StatusCode: 200, Duration: time.Second, Host: "api.anthropic.com",
			Client:    &pipeline.EventClient{Name: "claude-code", Version: "2.1.0"},
			Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 100 * (i + 1), InputTokens: 10},
			Plugins:   map[string]json.RawMessage{event.Key: raw}})
	}
	store.Close()
	if err := arch.Close(); err != nil {
		t.Fatal(err)
	}
}

// The round trip the replay exists for: traffic recorded by one process, which then stops, reads
// back into the next process's ring as the first process's ring had it — in every grouping, for
// every session, with request plugins paired to their responses across interleaved sessions.
//
// Run twice: with the default cap, and with a cap of one session ring, where every new session
// evicts the coldest — so the surviving per-session ring is only right if the replay fed events in
// the order they happened.
func TestReplayUsage_TheNextProcessHasTheRingTheLastOneHad(t *testing.T) {
	for _, maxSess := range []int{-1, 1} { // -1 keeps the default
		t.Run(fmt.Sprint("maxSessions=", maxSess), func(t *testing.T) {
			root := t.TempDir()
			now := time.Now().Truncate(time.Minute).Add(30 * time.Second)
			clock := func() time.Time { return now }
			first := usage.New(usage.WithClock(clock), usage.WithMaxSessions(maxSess))
			recordTraffic(t, root, first, now)

			reopened, err := archive.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			second := usage.New(usage.WithClock(clock), usage.WithMaxSessions(maxSess))
			events, sessions, skipped, err := replayUsage(context.Background(), reopened, second, now)
			if err != nil || events != 8 || sessions != 2 || skipped != 0 {
				t.Fatalf("replayUsage = %d events, %d sessions, %d skipped, %v; want 8, 2, 0, nil", events, sessions, skipped, err)
			}
			for _, g := range []usage.Group{usage.GroupNone, usage.GroupModel, usage.GroupPlugin, usage.GroupSession,
				usage.GroupAgent, usage.GroupHost, usage.GroupStatus} {
				for _, id := range []string{"", "a", "b"} {
					w := first.Snapshot(time.Hour, time.Minute, id, g)
					got := second.Snapshot(time.Hour, time.Minute, id, g)
					if !reflect.DeepEqual(w, got) {
						t.Errorf("group %s session %q: replayed ring differs:\nwant %+v\ngot  %+v", g, id, w, got)
					}
				}
			}
		})
	}
}

// errAfter is a context whose Err is nil for its first n calls and context.DeadlineExceeded after,
// so a test can run a deadline out part-way through a read instead of before it starts. ReplaySince
// asks Err and nothing else, once per segment and once per event.
type errAfter struct {
	context.Context
	n int
}

func (c *errAfter) Err() error {
	if c.n > 0 {
		c.n--
		return nil
	}
	return context.DeadlineExceeded
}

// ALL OR NOTHING: a replay that runs out of time feeds nothing, and the ring starts empty as it
// always did, rather than holding a partial LAST 1H that reads as real.
//
// THE DEADLINE LANDS MID-READ, after ReplaySince has delivered events, not before it starts. Err
// answers nil three times: the first segment's check, then its first turn's request and response.
// A replay that fed the ring from inside the callback has fed that turn by then, so this fails it,
// where a context already done would deliver nothing and let it pass.
func TestReplayUsage_AnExhaustedBudgetFeedsNothing(t *testing.T) {
	const allowed = 3
	root := t.TempDir()
	now := time.Now()
	recordTraffic(t, root, nil, now)
	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()

	// The premise: this deadline interrupts a read that has started delivering.
	delivered := 0
	if _, err := arch.ReplaySince(&errAfter{context.Background(), allowed}, now.Add(-usage.MaxWindow),
		func(string, *pipeline.SessionEvent) { delivered++ }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReplaySince err = %v, want context.DeadlineExceeded", err)
	}
	if delivered == 0 || delivered >= 8 {
		t.Fatalf("ReplaySince delivered %d of 8 events before the deadline; want some but not all", delivered)
	}

	agg := usage.New(usage.WithClock(func() time.Time { return now }))
	ctx := &errAfter{context.Background(), allowed}
	if _, _, _, err := replayUsage(ctx, arch, agg, now); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if n := agg.Snapshot(usage.MaxWindow, usage.MaxWindow, "", usage.GroupNone).Totals.Requests; n != 0 {
		t.Fatalf("an abandoned replay fed %d requests", n)
	}
}
