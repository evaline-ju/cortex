package usage

import (
	"encoding/json"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Replay records an event from before this process started — the session archive's, at startup —
// and reports whether it was in range. It is Record with one guard: an event whose minute is in the
// future, or not among the newest NumBuckets-1 minutes, is dropped.
//
// THE GUARD IS WHAT MAKES OLD EVENTS SAFE TO FEED. A slot is a minute modulo NumBuckets, and a
// write to a slot holding a different minute resets it (see Aggregator): an event exactly
// NumBuckets minutes old shares its slot with the current minute and would erase it. Record never
// meets one, because live events are current; a replay is made of nothing else.
//
// ONE MINUTE OF MARGIN, so a minute boundary passing between this check and Record's lock cannot
// carry an event over the edge. A check outside the lock is enough because a replay runs before any
// listener starts: nothing else writes the ring meanwhile.
//
// Feed events in ascending At order. Record pairs a request with the response after it, and
// reclaims the least recently written session ring at the cap, so that order is what makes the
// replayed ring the one the old process had.
func (a *Aggregator) Replay(sessionID string, e *pipeline.SessionEvent) bool {
	if e == nil || e.At.IsZero() {
		return false
	}
	now := a.now().Truncate(BucketWidth)
	m := e.At.Truncate(BucketWidth)
	if m.After(now) || !m.After(now.Add(-(NumBuckets-1)*BucketWidth)) {
		return false
	}
	a.Record(sessionID, e)
	return true
}

// ReplayCopy is e reduced to what Record reads, for a caller that holds many events before
// replaying them: an archived inference event carries its whole conversation, and six hours of
// those would otherwise sit in memory at once to be sorted. A copy costs about 1KB, however long
// the conversation was.
//
// A COPY WITH THE HEAVY FIELDS CLEARED, NOT A COPY OF THE FIELDS RECORD READS, so a field Record
// starts reading later is kept without anyone remembering to add it here. What is cleared is what
// Record has no use for: the protocol payloads, the conversation, the tool manifest, the completion
// and its tool calls, and every plugin entry but the cost record. TestReplayCopy_RecordsWhatTheEventDoes
// holds that.
func ReplayCopy(e *pipeline.SessionEvent) pipeline.SessionEvent {
	c := *e
	c.A2A, c.MCP = nil, nil
	if e.Inference != nil {
		inf := *e.Inference
		inf.Messages, inf.Tools, inf.ToolChoice, inf.ToolCalls, inf.Completion = nil, nil, nil, nil, ""
		c.Inference = &inf
	}
	c.Plugins = nil
	for _, k := range [...]string{event.Key, event.PluginName} {
		if raw, ok := e.Plugins[k]; ok {
			if c.Plugins == nil {
				c.Plugins = make(map[string]json.RawMessage, 1)
			}
			c.Plugins[k] = raw
		}
	}
	return c
}
