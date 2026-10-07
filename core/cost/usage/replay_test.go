package usage

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The guard: an event NumBuckets minutes old shares the current minute's slot, and replaying it
// must not erase that minute; nor may the margin minute or a future event land.
func TestReplay_DropsWhatWouldResetAVisibleBucket(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", respEvent(now, 200, time.Second, "m", 100))
	for _, at := range []time.Time{
		now.Add(-NumBuckets * BucketWidth), now.Add(-(NumBuckets - 1) * BucketWidth), now.Add(BucketWidth),
	} {
		if a.Replay("s1", respEvent(at, 200, time.Second, "m", 7)) {
			t.Errorf("replayed an event at %v", at)
		}
	}
	if got := a.Snapshot(MaxWindow, MaxWindow, "", GroupNone).Totals.Tokens; got != 100 {
		t.Fatalf("tokens = %d, want only the live event's 100", got)
	}
	if !a.Replay("s1", respEvent(now.Add(-(NumBuckets-2)*BucketWidth), 200, time.Second, "m", 7)) {
		t.Fatal("an event inside the window was dropped")
	}
	if a.Replay("s1", &pipeline.SessionEvent{Phase: pipeline.SessionResponse}) {
		t.Fatal("replayed an event with no time")
	}
}

// ReplayCopy keeps everything Record reads: a ring fed the copies of a fixture that sets every
// bucket field — with a conversation and a stray plugin entry added, which it must drop — is the
// ring fed the events, in every grouping.
func TestReplayCopy_RecordsWhatTheEventDoes(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	full, slim := New(WithClock(clock)), New(WithClock(clock))
	evs := append(richTurn(t, now.Add(-2*time.Minute), "m1", "claude-code", "api.anthropic.com", 40),
		richTurn(t, now.Add(-time.Minute), "m2", "opencode", "api.openai.com", 70)...)
	for _, e := range evs {
		if e.Inference != nil {
			e.Inference.Messages = []pipeline.InferenceMessage{{Role: "user", Content: "a conversation"}}
		}
		if e.Plugins == nil {
			e.Plugins = map[string]json.RawMessage{}
		}
		e.Plugins["mcp-parser"] = json.RawMessage(`{"x":1}`)
		c := ReplayCopy(e)
		if c.Inference != nil && c.Inference.Messages != nil {
			t.Fatal("ReplayCopy kept the conversation")
		}
		if _, ok := c.Plugins["mcp-parser"]; ok {
			t.Fatal("ReplayCopy kept a plugin entry Record does not read")
		}
		full.Record("s1", e)
		slim.Record("s1", &c)
	}
	for _, g := range []Group{GroupNone, GroupModel, GroupEndpoint, GroupSession, GroupAgent,
		GroupCurrency, GroupStatus, GroupPlugin, GroupHost} {
		f := full.Snapshot(10*time.Minute, BucketWidth, "", g)
		s := slim.Snapshot(10*time.Minute, BucketWidth, "", g)
		if !reflect.DeepEqual(f, s) {
			t.Errorf("group %s: the copies record differently:\nfull %+v\nslim %+v", g, f, s)
		}
	}
}
