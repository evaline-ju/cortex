package pipeline

import (
	"context"
	"strings"
	"testing"
)

// appender is a request-body mutator that appends suffix to whatever body it is
// handed, so a test can tell from the final bytes which writers ran and in what
// order.
func appender(name, suffix string) *stubPlugin {
	return &stubPlugin{
		name: name,
		caps: PluginCapabilities{WritesRequestBody: true},
		onReq: func(_ context.Context, pctx *Context) Action {
			pctx.SetBody(append(append([]byte{}, pctx.Body...), suffix...))
			return Action{Type: Continue}
		},
	}
}

// Request mutators chain: each sees pctx.Body as the one before it left it, and
// the body the listener sends is the last one's.
func TestNew_ChainsRequestMutators(t *testing.T) {
	var seenBySecond string
	second := appender("second", "+b")
	inner := second.onReq
	second.onReq = func(ctx context.Context, pctx *Context) Action {
		seenBySecond = string(pctx.Body)
		return inner(ctx, pctx)
	}
	p, err := New([]Plugin{
		&stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}},
		appender("first", "+a"),
		second,
	})
	if err != nil {
		t.Fatalf("New refused two request mutators after a reader: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	if seenBySecond != "x+a" {
		t.Errorf("second mutator saw %q, want the first one's output x+a", seenBySecond)
	}
	if string(pctx.Body) != "x+a+b" || !pctx.BodyMutated() {
		t.Errorf("Body = %q, BodyMutated = %v; want x+a+b, true", pctx.Body, pctx.BodyMutated())
	}
}

// The reader rule is what the one-mutator rule protected, and it holds with any
// number of mutators: a reader after either of them fails, naming the first.
func TestNew_RejectsAReaderAfterAnyRequestMutator(t *testing.T) {
	reader := &stubPlugin{name: "parser", caps: PluginCapabilities{ReadsBody: true}}
	for name, chain := range map[string][]Plugin{
		"after both":      {appender("first", "+a"), appender("second", "+b"), reader},
		"between the two": {appender("first", "+a"), reader, appender("second", "+b")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(chain)
			if err == nil {
				t.Fatal("New accepted a body reader after a mutator")
			}
			if !strings.Contains(err.Error(), `plugin "parser" reads body after mutator "first"`) {
				t.Errorf("err = %v, want it to name the reader and the first mutator", err)
			}
		})
	}
}

// Under observe a mutator's write is a no-op, so the next mutator sees the body
// as it was.
func TestNew_AnObservedMutatorLeavesTheNextOneTheUnmodifiedBody(t *testing.T) {
	var seenBySecond string
	second := appender("second", "+b")
	inner := second.onReq
	second.onReq = func(ctx context.Context, pctx *Context) Action {
		seenBySecond = string(pctx.Body)
		return inner(ctx, pctx)
	}
	p, err := New([]Plugin{appender("first", "+a"), second}, WithPolicies(ErrorPolicyObserve, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	if seenBySecond != "x" || string(pctx.Body) != "x+b" {
		t.Errorf("second saw %q and the body is %q; want x and x+b", seenBySecond, pctx.Body)
	}
}

// At most one response mutator still: nothing needs more, and the response pass
// has its own ordering gap.
func TestNew_StillRejectsTwoResponseMutators(t *testing.T) {
	_, err := New([]Plugin{
		&stubPlugin{name: "a", caps: PluginCapabilities{WritesResponseBody: true}},
		&stubPlugin{name: "b", caps: PluginCapabilities{WritesResponseBody: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "WritesResponseBody") {
		t.Fatalf("err = %v, want two response mutators refused", err)
	}
}
