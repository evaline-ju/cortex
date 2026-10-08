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

// bodyMutation is the framework's record of the rewrites on pctx.
func bodyMutation(t *testing.T, pctx *Context) bodyMutationEvent {
	t.Helper()
	raw, ok := pctx.Extensions.Custom["body-mutation"+PluginEventSuffix]
	if !ok {
		t.Fatalf("no body-mutation event; keys: %v", keys(pctx.Extensions.Custom))
	}
	ev, ok := raw.(bodyMutationEvent)
	if !ok {
		t.Fatalf("event type = %T, want bodyMutationEvent", raw)
	}
	return ev
}

// assertMutation fails unless ev describes before → after, written by plugins in
// that order, with plugin naming the last of them.
func assertMutation(t *testing.T, ev bodyMutationEvent, phase, before, after string, plugins ...string) {
	t.Helper()
	if ev.Phase != phase {
		t.Errorf("phase = %q, want %q", ev.Phase, phase)
	}
	if ev.LengthBefore != len(before) || ev.SHA256Before != hashHex([]byte(before)) {
		t.Errorf("before = %d bytes %s, want %q", ev.LengthBefore, ev.SHA256Before, before)
	}
	if ev.LengthAfter != len(after) || ev.SHA256After != hashHex([]byte(after)) {
		t.Errorf("after = %d bytes %s, want %q", ev.LengthAfter, ev.SHA256After, after)
	}
	if strings.Join(ev.Plugins, ",") != strings.Join(plugins, ",") || ev.Plugin != plugins[len(plugins)-1] {
		t.Errorf("plugin = %q, plugins = %v; want %q and %v", ev.Plugin, ev.Plugins, plugins[len(plugins)-1], plugins)
	}
}

// With two writers the record keeps the bytes the client sent as before, the
// bytes sent upstream as after, and both writers in order. Each writer's own
// modify/body_rewritten stays on the timeline.
func TestBodyMutation_RecordsTheClientsBytesAndEveryWriter(t *testing.T) {
	p := mustBuild(t, appender("first", "+a"), appender("second", "+b"))
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+a+b", "first", "second")
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 2 || invs[0].Plugin != "first" || invs[1].Plugin != "second" ||
		invs[0].Reason != "body_rewritten" || invs[1].Reason != "body_rewritten" {
		t.Errorf("invocations = %+v, want one body_rewritten per writer, in order", invs)
	}
}

// A shadow write sent nothing upstream, so it must not displace the record of a
// write that did.
func TestBodyMutation_AShadowWriteLeavesAnAppliedRecordAlone(t *testing.T) {
	p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b")},
		WithPolicies(ErrorPolicyEnforce, ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+a", "first")
}

// A shadow write publishes its would-be record, as a lone observed writer always
// has, until a write takes effect and replaces it.
func TestBodyMutation_AnAppliedWriteReplacesAShadowRecord(t *testing.T) {
	p, err := New([]Plugin{appender("first", "+a"), appender("second", "+b")},
		WithPolicies(ErrorPolicyObserve, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Body: []byte("x")}
	p.Run(context.Background(), pctx)

	assertMutation(t, bodyMutation(t, pctx), "request", "x", "x+b", "second")
}

// The response side keeps its own record: its before is the response the
// upstream sent, not the request the client did.
func TestBodyMutation_TheResponseRecordStartsFromTheResponse(t *testing.T) {
	c := &Context{Direction: Outbound, Body: []byte("req")}
	c.SetCurrentPlugin("pruner", InvocationPhaseRequest)
	c.SetBody([]byte("req+a"))
	c.ResponseBody = []byte("resp")
	c.SetCurrentPlugin("filter", InvocationPhaseResponse)
	c.SetResponseBody([]byte("resp+f"))

	assertMutation(t, bodyMutation(t, c), "response", "resp", "resp+f", "filter")
}
