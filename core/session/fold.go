package session

import (
	"encoding/json"
	"strings"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// applyTitle is the title rule: FIRST-WINS, EXCEPT THAT A /rename ALWAYS OVERRIDES. It returns
// the rank and title a session holds after a candidate of candRank and candText.
//
// Shared by appendLocked and SummaryFold, so that a resident session and an archived one can
// never be titled by two rules.
//
// THE SECOND DISJUNCT IS THE WHOLE RULE, not a tie-break detail: without it a re-rename is
// silently ignored, because the equal rank never beats the one already held. Widening it to a
// plain `<=` instead is the opposite failure — the title then shifts on every turn, since each
// new prose message equals the rank of the last.
//
// THE BLANK SCREEN IS REDUNDANT AND NO TEST CAN SHOW IT, which is worth saying rather than
// leaving a surviving mutant for the next reader to re-derive. titleCandidate returns "" only
// ever paired with rankNone (every blank-folding shape — empty, whitespace, reminder-only, an
// empty <user_query> — comes back as rankNone), and a fresh fold initialises to rankNone, so
// `rankNone < rankNone` is false and the rank test alone rejects it. Kept as a local statement
// of what the fold requires: a future titleCandidate returning a blank at a real rank fails
// here instead of storing one.
func applyTitle(rank int, title string, candRank int, candText string) (int, string) {
	if candText != "" && (candRank < rank || (candRank == rank && candRank == rankRename)) {
		// sanitizeTitle IS SAFE UNDER THE STORE'S LOCK, where appendLocked calls this, because it
		// is O(maxTitleLen) rather than O(candidate): it stops at 80 emitted runes, so a 190KB
		// candidate costs 328ns, not 916µs.
		//
		// A CALL-COUNTING ARGUMENT IS NOT ENOUGH HERE, which is why the cap and not the count is
		// what this rests on. "At most once per rank improvement" bounds the fold at three times
		// per session under first-wins — except for /rename, which the second disjunct lets win
		// repeatedly and which the client controls. Flooding /rename with a 190KB argument paid
		// ~938µs of write-lock hold per append, unbounded in repetitions; measured on that flood
		// with a concurrent reader, mean ListSessions latency is 205µs uncapped against 16.4µs
		// capped.
		//
		// Hoisting it above the lock would still be wrong, just cheaply so: it would fold every
		// event's candidate including the ones about to be discarded on rank.
		if t := sanitizeTitle(candText); t != "" {
			return candRank, t
		}
	}
	return rank, title
}

// applyAgent records the first label a coding agent sent into a session. The default and
// pending buckets name no agent: they collect traffic for no one session. Shared by
// appendLocked and SummaryFold for applyTitle's reason.
func applyAgent(agents []sessionAgent, sessionID, agentName string, c *pipeline.EventClient) []sessionAgent {
	if agentName == "" || sessionID == DefaultSessionID || strings.HasPrefix(sessionID, PendingPrefix) ||
		agentLabelIn(agents, agentName) != "" {
		return agents
	}
	return append(agents, sessionAgent{name: agentName, label: usage.AgentLabel(c)})
}

// agentLabelIn is the first label the agent named name sent, or "".
func agentLabelIn(agents []sessionAgent, name string) string {
	for _, a := range agents {
		if a.name == name {
			return a.label
		}
	}
	return ""
}

// SummaryFold is ListSessions' per-session figures folded one event at a time and never shed:
// the session archive's running summary for a session the store may no longer hold.
//
// It uses the store's own rules — moneyOf through prepareAppend, sumTokens' response-only
// token rule, applyTitle, applyAgent — so a fold over an untrimmed session's events reports
// what ListSessions does. TestSummaryFold_AgreesWithListSessions holds that.
//
// ONE DIFFERENCE IS INHERENT: Agent is the first known agent's label, while the store prefers
// the label of the agent that CLAIMED the session, which only the store knows. A resident
// session's row therefore always comes from the store.
//
// The zero value is not ready — rankRename is 0, so a zero titleRank would claim the session
// had been renamed. Use NewSummaryFold. Not safe for concurrent use.
type SummaryFold struct {
	events    int
	tokens    int
	cost      usage.CostSum
	avoided   usage.CostSum
	units     map[string]int
	titleRank int
	title     string
	agents    []sessionAgent
}

// NewSummaryFold returns an empty fold.
func NewSummaryFold() *SummaryFold { return &SummaryFold{titleRank: rankNone} }

// Add folds one event recorded under sessionID. It does the work appendLocked hoists above the
// store's lock — a plugin-map decode and a scan of message content — so call it from a
// goroutine of your own, never from Record.
func (f *SummaryFold) Add(sessionID string, e *pipeline.SessionEvent) {
	p := prepareAppend(e)
	f.events++
	if e.Phase == pipeline.SessionResponse && e.Inference != nil {
		f.tokens += e.Inference.TotalTokens
	}
	f.cost.Add(p.money.cost)
	f.avoided.Add(p.money.avoided)
	if p.money.priced || p.money.avoided > 0 {
		if f.units == nil {
			f.units = make(map[string]int, 1)
		}
		f.units[p.money.unit]++
	}
	f.titleRank, f.title = applyTitle(f.titleRank, f.title, p.titleRank, p.titleText)
	f.agents = applyAgent(f.agents, sessionID, p.agentName, e.Client)
}

// Summary sets EventCount, Title, Agent, TotalTokens, CostMicros, AvoidedMicros, Currencies
// and Saturated on a SessionSummary for sessionID, and nothing else: the timestamps and the
// live-only fields (Active, Adopted, PromptContext) are the caller's.
func (f *SummaryFold) Summary(sessionID string) SessionSummary {
	sum := SessionSummary{
		ID:            sessionID,
		EventCount:    f.events,
		Title:         f.title,
		TotalTokens:   f.tokens,
		CostMicros:    f.cost.Micros,
		AvoidedMicros: f.avoided.Micros,
		Currencies:    currenciesOf(f.units),
		Saturated:     f.cost.Saturated || f.avoided.Saturated,
	}
	if sessionID != DefaultSessionID && !strings.HasPrefix(sessionID, PendingPrefix) && len(f.agents) > 0 {
		sum.Agent = f.agents[0].label
	}
	return sum
}

// foldJSON is SummaryFold's state as the session archive persists it in session.json, so a
// fold resumes after a restart instead of being rebuilt by decoding every segment.
//
// TitleRank IS A POINTER because rankRename is 0: a document without it must decode to
// rankNone, not to a rename that would lock the title against every later candidate.
type foldJSON struct {
	EventCount       int            `json:"eventCount"`
	Title            string         `json:"title,omitempty"`
	TitleRank        *int           `json:"titleRank,omitempty"`
	Agents           []foldAgent    `json:"agents,omitempty"`
	TotalTokens      int            `json:"totalTokens,omitempty"`
	CostMicros       int64          `json:"costMicros,omitempty"`
	CostSaturated    bool           `json:"costSaturated,omitempty"`
	AvoidedMicros    int64          `json:"avoidedMicros,omitempty"`
	AvoidedSaturated bool           `json:"avoidedSaturated,omitempty"`
	Units            map[string]int `json:"units,omitempty"`
}

type foldAgent struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// MarshalJSON persists the fold's whole state, not just its Summary: resuming needs the title
// rank and each currency's count, which a summary does not carry.
func (f *SummaryFold) MarshalJSON() ([]byte, error) {
	j := foldJSON{
		EventCount:       f.events,
		Title:            f.title,
		TitleRank:        &f.titleRank,
		TotalTokens:      f.tokens,
		CostMicros:       f.cost.Micros,
		CostSaturated:    f.cost.Saturated,
		AvoidedMicros:    f.avoided.Micros,
		AvoidedSaturated: f.avoided.Saturated,
		Units:            f.units,
	}
	for _, a := range f.agents {
		j.Agents = append(j.Agents, foldAgent{Name: a.name, Label: a.label})
	}
	return json.Marshal(j)
}

// UnmarshalJSON restores a fold MarshalJSON wrote; a missing titleRank is rankNone.
func (f *SummaryFold) UnmarshalJSON(b []byte) error {
	var j foldJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*f = SummaryFold{
		events:    j.EventCount,
		tokens:    j.TotalTokens,
		cost:      usage.CostSum{Micros: j.CostMicros, Saturated: j.CostSaturated},
		avoided:   usage.CostSum{Micros: j.AvoidedMicros, Saturated: j.AvoidedSaturated},
		units:     j.Units,
		titleRank: rankNone,
		title:     j.Title,
	}
	if j.TitleRank != nil {
		f.titleRank = *j.TitleRank
	}
	for _, a := range j.Agents {
		f.agents = append(f.agents, sessionAgent{name: a.Name, label: a.Label})
	}
	return nil
}
