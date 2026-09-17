package session

import (
	"slices"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// Repeated message content is stored once per session, not once per event.
//
// WHY: an LLM request carries the whole conversation, so every turn re-sends every
// earlier message. The store keeps one event per request, each holding its own copy of
// that array, which makes retention quadratic in turns while the conversation itself
// grows linearly. Measured on a real 96-turn session: 27.7MB of message text held, 7.0MB
// distinct — 3.9x, and the factor rises with turn count because the duplicated prefix
// gets longer. The proxy reached 3.37GB resident on a laptop in 18 hours, against
// ~833MB for every Claude Code transcript on the same disk. The transcripts are the same
// conversations, appended once each; the difference was all duplication.
//
// The fix costs nothing at read time and changes nothing observable: Go strings are
// immutable, so two events sharing one backing array cannot tell, and a consumer sees
// the same bytes it always did.
//
// SCOPE: string fields only — inference messages and completions, A2A part content, and
// tool descriptions. The tool manifest earns its place: a client re-sends it on every
// request, so it duplicates harder than the conversation does. Measured on a live
// 272-event session, 10.3MB of tool JSON held against 0.1MB distinct — 154x, where the
// conversation itself was 6.5x. Interning the descriptions takes that session's tool
// retention from 7.12MB to 0.37MB.
//
// What is still duplicated, both because they are map[string]any and need a recursive walk
// of arbitrary JSON, and in ascending order of how much they cost: InferenceTool.Parameters
// (the JSON schema — 34.7% of that 10.3MB, so ~3.6MB per session of the same tool
// signatures over and over) and MCP Params/Result (unmeasured on this workload). Deferred
// rather than guessed at: a walk that rewrites map values cannot lean on string
// immutability the way this does, so it needs its own reasoning about aliasing.
const (
	// internMinLen is the shortest string worth a map lookup.
	//
	// A role ("user"), a finish reason ("end_turn") and an empty completion all repeat
	// constantly and all cost less to duplicate than to hash. The savings live in
	// message bodies, which are orders of magnitude past this.
	internMinLen = 64
)

// interner maps a string to the one copy the session keeps of it.
//
// It holds only the strings of the LAST event interned, which is enough to collapse the
// whole history: turn N's array is turn N-1's array plus a message or two, so interning N
// against N-1 makes them share; N-1 already shares with N-2, and so on back to the first
// turn that carried the string.
//
// That is one event's worth of keys, which on this workload is NOT small — an LLM request
// carries the whole conversation, so the last event's strings are roughly the whole
// distinct set. What the rolling table buys is not a small table but a table that costs
// nothing extra: every key is a string some event already references, so it pins no
// content of its own and needs no pruning in step with event eviction. A table
// accumulating across events would hold strings whose events had been evicted.
//
// The tradeoff is deliberate: content that disappears from the conversation and comes
// back later (a compaction that rewrites history, say) misses the table and gets a second
// copy. Best-effort dedup with a bounded table beats exact dedup with a table that
// outlives what it describes.
type interner struct {
	prev map[string]string
}

// intern returns the session's copy of s, recording s as canonical if it is new.
//
// The table is keyed on the CANONICAL string, never on the duplicate that was looked
// up. Keying on the duplicate would work identically for lookups — equal strings hash
// equally — while pinning the copy the event just stopped referencing, so the table
// would hold a copy of every message the last interned event carried.
//
// Bounded to one event's worth, because the table is rolled forward on every Append — so
// on BenchmarkRetainedHeap it is 2.358MB keyed on the duplicate against 2.205MB keyed on
// the canonical, a 0.15MB difference and not the multiplier it first looks like. Worth
// fixing because it is free, and worth measuring before claiming more than that.
func (in *interner) intern(s string, next map[string]string) string {
	if len(s) < internMinLen {
		return s
	}
	canon, ok := in.prev[s]
	if !ok {
		canon = s
	}
	next[canon] = canon
	return canon
}

// internEvent gives the event its own extension and message slice, with content
// pointing at the session's existing copies, then rolls the table forward.
//
// It CLONES rather than rewriting in place, and that is not defensive habit — writing in
// place is unsound here for three separate reasons:
//
//   - pipeline.SnapshotInference and SnapshotA2A are shallow copies, and their contract
//     says so: "Slice fields are reused intentionally — they are only assigned, never
//     mutated in place, after the parser completes." Interning in place breaks the
//     invariant the rest of the pipeline is written against.
//   - the request-phase and response-phase events of one request alias the same backing
//     array, because both snapshot the same live pctx.Extensions.Inference
//     (forwardproxy/server.go:377 and :880). So the second Append would rewrite an
//     array the first event already published.
//   - publishLocked hands the event — extension pointers included — to subscriber
//     channels, and the SSE goroutine encodes it outside the store's mutex. Mutating
//     those arrays is a straight data race against an in-flight encode.
//
// "Every replacement is a string equal to the one it replaced" does not rescue it
// either. With two requests interleaved in one session bucket, the second Append rolls
// prev forward before the first request's response-phase event is interned, so that
// pass writes a genuinely different pointer over memory the store has already handed
// out.
//
// Cloning costs one slice copy per event and nothing in steady state: the original array
// becomes garbage when the request completes, and the store then owns everything it
// mutates.
func (in *interner) internEvent(e *pipeline.SessionEvent) {
	next := make(map[string]string, len(in.prev))

	if e.Inference != nil {
		cp := *e.Inference
		cp.Messages = slices.Clone(e.Inference.Messages)
		for i := range cp.Messages {
			cp.Messages[i].Content = in.intern(cp.Messages[i].Content, next)
		}
		cp.Completion = in.intern(cp.Completion, next)
		// Tools, like Messages, must be cloned before any field is rewritten: the same
		// array is aliased by this request's response-phase event. Parameters is left
		// alone — see SCOPE above.
		cp.Tools = slices.Clone(e.Inference.Tools)
		for i := range cp.Tools {
			cp.Tools[i].Description = in.intern(cp.Tools[i].Description, next)
		}
		e.Inference = &cp
	}
	if e.A2A != nil {
		cp := *e.A2A
		cp.Parts = slices.Clone(e.A2A.Parts)
		for i := range cp.Parts {
			cp.Parts[i].Content = in.intern(cp.Parts[i].Content, next)
		}
		e.A2A = &cp
	}

	in.prev = next
}
