package pipeline

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureWarnings is every log line New writes while building plugins.
func captureWarnings(t *testing.T, plugins ...Plugin) string {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := New(plugins); err != nil {
		t.Fatalf("New: %v", err)
	}
	return logs.String()
}

const readerWarning = "body reader precedes a response mutator"

// The warning is about the response pass running in reverse, so it needs a
// response mutator after the reader. Without one there is nothing to warn about —
// yet every non-streaming reader used to log it, so tool-prune and the
// inference-router, which Normalize counts as readers for writing the request
// body, warned on every build and reload of the laptop chain.
func TestNew_DoesNotWarnWithoutAResponseMutator(t *testing.T) {
	out := captureWarnings(t,
		&stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}},
		&stubPlugin{name: "pruner", caps: PluginCapabilities{WritesRequestBody: true}},
		&stubPlugin{name: "router", caps: PluginCapabilities{WritesRequestBody: true, WritesDestination: true}},
	)
	if strings.Contains(out, readerWarning) {
		t.Errorf("warned with no response mutator in the chain:\n%s", out)
	}
}

// A reader before the response mutator sees the mutator's bytes on the response
// pass, and is warned about. (One after it is refused outright, by the reader rule.)
func TestNew_WarnsAboutAReaderBeforeAResponseMutator(t *testing.T) {
	out := captureWarnings(t,
		&stubPlugin{name: "before", caps: PluginCapabilities{ReadsBody: true}},
		&stubPlugin{name: "filter", caps: PluginCapabilities{WritesResponseBody: true}},
	)
	if !strings.Contains(out, readerWarning) || !strings.Contains(out, "reader=before") {
		t.Errorf("no warning for the reader before the response mutator:\n%s", out)
	}
}

// A request-only writer does not read the response — NeedsResponseBody does not
// even buffer it for one — so it is no reader for this warning, wherever it sits.
func TestNew_DoesNotWarnAboutARequestOnlyWriter(t *testing.T) {
	out := captureWarnings(t,
		&stubPlugin{name: "pruner", caps: PluginCapabilities{WritesRequestBody: true}},
		&stubPlugin{name: "filter", caps: PluginCapabilities{WritesResponseBody: true}},
	)
	if strings.Contains(out, readerWarning) {
		t.Errorf("warned about a request-only writer:\n%s", out)
	}
}
