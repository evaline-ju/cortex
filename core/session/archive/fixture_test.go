package archive

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// synthSession builds a deterministic session shaped like the live traffic the design
// measured: a large system prompt and tool manifest re-sent on every request, a conversation
// that grows by a message on each side per turn, and a CONNECT tunnel-open between each request
// and its response — the contentless event gotcha #14 warns a clean run of turns leaves out.
//
// The text is generated from a seeded vocabulary, never taken from real traffic: fixtures in
// tree must not carry anyone's prompts. promptBytes sizes the system prompt and manifest
// together, so tests can stay small and benchmarks can match the 0.5–1.3MB measured live.
func synthSession(seed int64, turns, promptBytes int) []pipeline.SessionEvent {
	r := rand.New(rand.NewSource(seed))
	vocab := make([]string, 2000)
	for i := range vocab {
		b := make([]byte, 3+r.Intn(8))
		for j := range b {
			b[j] = byte('a' + r.Intn(26))
		}
		vocab[i] = string(b)
	}
	text := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(vocab[r.Intn(len(vocab))])
			b.WriteByte(' ')
		}
		return b.String()
	}

	tools := make([]pipeline.InferenceTool, 8)
	for i := range tools {
		tools[i] = pipeline.InferenceTool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: text(promptBytes / 48),
			Parameters: pipeline.RawJSON(fmt.Sprintf(
				`{"type":"object","properties":{"arg%d":{"type":"string","description":%q}}}`, i, text(promptBytes/48))),
		}
	}
	convo := []pipeline.InferenceMessage{{Role: "system", Content: text(promptBytes * 2 / 3)}}

	var evs []pipeline.SessionEvent
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var seq uint64
	add := func(e pipeline.SessionEvent) {
		seq++
		e.Seq, e.At, e.SessionID = seq, at, "synth"
		at = at.Add(time.Second)
		evs = append(evs, e)
	}
	for i := range turns {
		convo = append(convo, pipeline.InferenceMessage{Role: "user", Content: text(80 + r.Intn(400))})
		msgs := slices.Clone(convo)
		add(pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com",
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: msgs, Tools: tools}})
		add(pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com:443",
			Tunnel: true, HTTPMethod: "CONNECT"})
		reply := text(100 + r.Intn(800))
		add(pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, StatusCode: 200,
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: msgs, Tools: tools, Completion: reply, TotalTokens: 1000 + i}})
		convo = append(convo, pipeline.InferenceMessage{Role: "assistant", Content: reply})
	}
	return evs
}

// edgeEvents are the shapes synthSession does not produce: A2A parts, a denial, strings on
// either side of the dedup threshold, an empty schema, a NUL byte, and invalid UTF-8.
func edgeEvents() []pipeline.SessionEvent {
	long := strings.Repeat("an a2a part well past the threshold ", 3)
	return []pipeline.SessionEvent{
		{Seq: 100, Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{
			Method: "message/send", Parts: []pipeline.A2APart{{Kind: "text", Content: long}, {Kind: "text", Content: "short"}}}},
		{Seq: 101, Phase: pipeline.SessionDenied, StatusCode: 401, Error: &pipeline.EventError{Kind: "auth", Message: "no token"}},
		{Seq: 102, Phase: pipeline.SessionResponse, Inference: &pipeline.InferenceExtension{
			Completion: strings.Repeat("c", 63),
			Messages:   []pipeline.InferenceMessage{{Role: "user", Content: strings.Repeat("m", 64)}},
			Tools:      []pipeline.InferenceTool{{Name: "no_schema"}}}},
		{Seq: 103, Phase: pipeline.SessionRequest, Inference: &pipeline.InferenceExtension{
			Messages: []pipeline.InferenceMessage{
				{Role: "user", Content: "a NUL \x00 inside a message long enough to be referenced by hash, past sixty-four"},
				{Role: "user", Content: "invalid utf-8 \xff\xfe in a message long enough to be referenced by hash, past 64"}}}},
	}
}
