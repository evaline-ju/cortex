package session

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// benchTurns is a session long enough for the quadratic term to dominate, short enough
// to run in a second.
const benchTurns = 300

// benchConversation builds the messages one turn re-sends: the whole conversation so
// far. Rebuilt per turn, as a parser does, so nothing is shared by accident.
func benchConversation(turn int) []pipeline.InferenceMessage {
	out := make([]pipeline.InferenceMessage, 0, turn)
	for i := 0; i < turn; i++ {
		out = append(out, pipeline.InferenceMessage{
			Role:    "user",
			Content: fmt.Sprintf("message %04d: %s", i, strings.Repeat("prompt text ", 40)),
		})
	}
	return out
}

// BenchmarkRetainedHeap reports the heap a finished session holds, which is the number
// this package's interning exists to move.
//
// Committed because the figure is otherwise unreproducible after any change to how
// content is stored — and the review of that change found exactly such a regression
// hiding behind a plausible number: keying the intern table on the duplicate rather than
// the canonical string, which reads correctly and silently retains a second copy of the
// conversation.
//
// Reported rather than asserted. A threshold either flakes or is loose enough to miss
// the regressions that matter, so the deterministic properties are pinned by tests
// (TestIntern_TableKeysAreTheStringsTheEventsReference,
// TestAppend_SharesRepeatedMessageContent) and this reports the consequence:
//
//	go test ./session/ -bench RetainedHeap -run '^$' -benchtime 1x
func BenchmarkRetainedHeap(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		s := New(0, 0, 100)
		for turn := 1; turn <= benchTurns; turn++ {
			s.Append("s1", pipeline.SessionEvent{
				Inference: &pipeline.InferenceExtension{Messages: benchConversation(turn)},
			})
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Signed: HeapAlloc is uint64, and if a GC nets the heap below the baseline the
		// unsigned difference wraps to ~1.8e19 and reports ~1.7e13 MB/session. A small
		// negative is the honest answer and reads as one.
		delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		b.ReportMetric(float64(delta)/1048576, "MB/session")
		b.ReportMetric(float64(benchTurns), "turns")

		s.Close()
		runtime.KeepAlive(s)
	}
}
