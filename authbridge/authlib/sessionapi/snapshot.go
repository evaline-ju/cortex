package sessionapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// snapshotWriteBuffer is how much output accumulates before reaching the ResponseWriter.
//
// Small on purpose: it exists to keep the syscall count down, not to hold the document.
// bufio passes any single write larger than this straight through, so one oversized event
// costs one large write rather than growing anything.
const snapshotWriteBuffer = 32 << 10

// writeSessionView emits exactly the JSON json.NewEncoder(w).Encode(view) emits, one
// event at a time.
//
// WHY not simply Encode the view: json.Encoder does not stream. It marshals the whole
// value into a single buffer, grown by doubling, and writes nothing until that buffer
// holds the entire document — so serving a snapshot costs heap proportional to the
// RESPONSE, not to one event. Measured by BenchmarkSnapshotHeap on the session shape a
// laptop proxy actually carries (226 events, ~950 messages each, a 105MB document): one
// Encode grows HeapSys by ~246MB, against ~14MB for this function — same bytes out. Go
// keeps the difference as idle heap rather than handing it back, so RSS ratchets once per
// request and does not recover: on the live proxy, four snapshot requests took it from
// 731MB to 1447MB, still 1447MB two minutes later. abctl requests a snapshot on every
// Enter into a session, so that was a couple hundred megabytes per keystroke.
//
// This is the read half of the memory problem; authlib/session/intern.go is the retention
// half. Interning could not have helped here, and the two are not alternatives: interning
// makes stored events SHARE one copy of a repeated message, and an encoder expands every
// share back into its own bytes on the way out.
//
// Byte-for-byte compatibility is the contract, not an approximation of it — a hand-rolled
// envelope around a generated encoding is exactly the thing that drifts silently, so
// TestWriteSessionView_MatchesTheBufferedEncoding compares the two outputs directly on
// every shape this can produce.
//
// WHAT IS GIVEN UP: the response is no longer atomic. Encode either produced the whole
// document or none of it, because it failed before the first byte left the buffer; this
// writes as it goes, so a marshal failure on event i leaves the client holding a truncated
// document under a 200 and a Content-Type that promises JSON. That is unavoidable for
// anything streaming — the status line is long gone by then — and the client's own decoder
// reports it as a parse error rather than as silently short data.
//
// Reachable only in theory today: the sole marshal-error source in the tree is an invalid
// json.RawMessage in SessionEvent.Plugins, and those are produced by marshaling in the
// first place. It is written down because it is a real difference in failure behavior and
// the rest of this file accounts for its tradeoffs.
func writeSessionView(w io.Writer, view *pipeline.SessionView) error {
	id, err := json.Marshal(view.ID)
	if err != nil {
		return err
	}

	// bufio's error is sticky — the first failure is returned by every later write and by
	// Flush — so the writes below need no individual checks and Flush reports whatever
	// they hit.
	bw := bufio.NewWriterSize(w, snapshotWriteBuffer)
	_, _ = bw.WriteString(`{"id":`)
	_, _ = bw.Write(id)
	_, _ = bw.WriteString(`,"events":`)

	if view.Events == nil {
		// A nil slice encodes as null, and matching the encoder means matching that too
		// rather than improving on it here.
		_, _ = bw.WriteString("null")
	} else {
		_ = bw.WriteByte('[')
		for i := range view.Events {
			if i > 0 {
				_ = bw.WriteByte(',')
			}
			// Marshal per event, not Encode: Encode appends a newline to each value,
			// which would land inside the array and break byte-compatibility. The peak
			// buffer is this one event.
			b, err := json.Marshal(&view.Events[i])
			if err != nil {
				// Flush before returning, deliberately. The document is already
				// truncated either way, so the choice is only whether the client's cut
				// falls where this failed or wherever the buffer boundary happened to
				// be — up to snapshotWriteBuffer of committed events earlier, and not
				// reproducibly. Cutting at the failure makes the truncation point mean
				// something, and pairs with the event index in the error the caller logs.
				_ = bw.Flush()
				return fmt.Errorf("marshal event %d of %d: %w", i, len(view.Events), err)
			}
			_, _ = bw.Write(b)
		}
		_ = bw.WriteByte(']')
	}

	if view.TotalEvents != 0 { // omitempty
		_, _ = bw.WriteString(`,"totalEvents":`)
		_, _ = bw.WriteString(strconv.Itoa(view.TotalEvents))
	}
	_, _ = bw.WriteString("}\n") // Encode terminates every value with a newline.
	return bw.Flush()
}
