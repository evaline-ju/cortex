package inferencerouter

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"

	// The laptop's outbound chain, which the router joins last.
	_ "github.com/rossoctl/cortex/core/plugins/a2aparser"
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	_ "github.com/rossoctl/cortex/core/plugins/mcpparser"
	_ "github.com/rossoctl/cortex/core/plugins/toolprune"
)

// mappedConfig is routerConfig with glm naming a model of its own for each family,
// a different one each, so a test can tell which family a request mapped to. ete
// still serves Claude Code's names.
func mappedConfig(agents string) string {
	return `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key"},
			"glm": {"url": "https://glm.example.com:8443", "key": "glm-key",
			        "opus": "glm-big", "sonnet": "glm-mid", "haiku": "glm-small"}
		},
		"agents": {` + agents + `}
	}`
}

// messagesBody is a Claude Code request for model.
func messagesBody(model string) string {
	return `{"model":"` + model + `","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
}

// withModel gives pctx a body asking for model, and the inference record the
// parser would have built from it.
func withModel(pctx *pipeline.Context, model string) *pipeline.Context {
	pctx.Body = []byte(messagesBody(model))
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: model}
	return pctx
}

// outboundReasons is every record on the outbound pass as action/reason, in order.
func outboundReasons(pctx *pipeline.Context) []string {
	var out []string
	if pctx.Extensions.Invocations == nil {
		return out
	}
	for _, inv := range pctx.Extensions.Invocations.Outbound {
		out = append(out, string(inv.Action)+"/"+inv.Reason)
	}
	return out
}

// assertRefused fails unless a is a Reject with status whose message contains
// want, and names neither glm's key nor its URL: a refusal is shown to the client
// and logged, and may name models and servers but never a secret or an address.
func assertRefused(t *testing.T, a pipeline.Action, status int, want string) {
	t.Helper()
	if a.Type != pipeline.Reject {
		t.Fatalf("action = %+v, want Reject", a)
	}
	got, _, body := a.Violation.Render()
	if got != status || !strings.Contains(string(body), want) {
		t.Errorf("status %d, body %s; want %d saying %q", got, body, status, want)
	}
	for _, secret := range []string{"glm-key", "glm.example.com"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the refusal names %q: %s", secret, body)
		}
	}
}

// assertNotSent fails unless a refused pctx kept the client's key and body, and
// its session pinned nothing.
func assertNotSent(t *testing.T, pctx *pipeline.Context, store *memstore.Store, body string) {
	t.Helper()
	if got := pctx.Headers.Get("Authorization"); got != "Bearer client-key" {
		t.Errorf("Authorization = %q, want the client's own key on a refused request", got)
	}
	if got := string(pctx.Body); got != body || pctx.BodyMutated() {
		t.Errorf("body = %s, BodyMutated = %v; want it untouched", got, pctx.BodyMutated())
	}
	if pin, ok := pinOf(t, store, "s1"); ok {
		t.Errorf("pin = %q; a session whose first request was refused pins nothing", pin)
	}
}

func TestRouter_MapsEachFamilyToItsServersModel(t *testing.T) {
	for requested, want := range map[string]string{
		"claude-opus-5-5":           "glm-big",
		"claude-sonnet-5":           "glm-mid",
		"claude-haiku-4-5-20251001": "glm-small",
	} {
		t.Run(requested, func(t *testing.T) {
			p := build(t, mappedConfig(`"claude-code": "glm"`))
			pctx := withModel(request(newStore(t), eteHost, claudeUA, "s1"), requested)
			if a := run(t, p, pctx); a.Type != pipeline.Continue {
				t.Fatalf("action = %+v, want Continue", a)
			}

			assertRouted(t, pctx, glmHost, "glm-key")
			if got := string(pctx.Body); got != messagesBody(want) {
				t.Errorf("body = %s, want %s", got, messagesBody(want))
			}
			if ext := pctx.Extensions.Inference; ext.Model != want || ext.RequestedModel != requested {
				t.Errorf("Model = %q, RequestedModel = %q; want %q and %q", ext.Model, ext.RequestedModel, want, requested)
			}
			assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
			wantTimeline := []string{"modify/redirected", "modify/body_rewritten", "modify/model_rewritten", "modify/routed"}
			if got := outboundReasons(pctx); !slices.Equal(got, wantTimeline) {
				t.Errorf("timeline = %v, want %v", got, wantTimeline)
			}
		})
	}
}

// A family the server has no model for — one picked with /model, say — is
// refused, not guessed, and the server's key never goes on the request. So is a
// name with no family at all, the server's own model included: the mapping is
// from Claude Code's names, and nothing is passed through on a guess.
func TestRouter_RefusesAFamilyTheServerHasNoModelFor(t *testing.T) {
	for _, requested := range []string{"claude-fable-5-1", "glm-big", "claude-opus-haiku"} {
		t.Run(requested, func(t *testing.T) {
			store := newStore(t)
			p := build(t, mappedConfig(`"claude-code": "glm"`))
			pctx := withModel(request(store, eteHost, claudeUA, "s1"), requested)
			a := run(t, p, pctx)

			assertRefused(t, a, http.StatusBadRequest, "glm has no model for "+requested)
			assertRecord(t, pctx, pipeline.ActionDeny, "no_model_for_family",
				map[string]string{"server": "glm", "pin": pinNone, "model": requested})
			assertNotSent(t, pctx, store, messagesBody(requested))
		})
	}
}

// A refusal leaves a session's pin alone: the session is still on its server, and
// it is the request, not the conversation, that was refused.
func TestRouter_ARefusalKeepsAnExistingPin(t *testing.T) {
	store := newStore(t)
	p := build(t, mappedConfig(`"claude-code": "glm"`))
	run(t, p, withModel(request(store, eteHost, claudeUA, "s1"), "claude-opus-5-5"))

	pctx := withModel(request(store, eteHost, claudeUA, "s1"), "claude-fable-5-1")
	if a := run(t, p, pctx); a.Type != pipeline.Reject {
		t.Fatalf("action = %+v, want Reject", a)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "no_model_for_family",
		map[string]string{"server": "glm", "pin": pinExisting, "model": "claude-fable-5-1"})
	if pin, ok := pinOf(t, store, "s1"); !ok || pin != "glm" {
		t.Errorf("pin = %q, %v; want the session still pinned to glm", pin, ok)
	}
}

// A server that serves Claude Code's names gets every name, any family or none.
func TestRouter_PassesEveryNameThroughToAServerWithoutModels(t *testing.T) {
	p := build(t, mappedConfig(`"claude-code": "ete"`))
	pctx := withModel(request(newStore(t), eteHost, claudeUA, "s1"), "claude-fable-5-1")
	run(t, p, pctx)

	assertRouted(t, pctx, eteHost, "ete-key")
	if got := string(pctx.Body); got != messagesBody("claude-fable-5-1") || pctx.BodyMutated() {
		t.Errorf("body = %s, BodyMutated = %v; want it untouched", got, pctx.BodyMutated())
	}
	if ext := pctx.Extensions.Inference; ext.Model != "claude-fable-5-1" || ext.RequestedModel != "" {
		t.Errorf("Model = %q, RequestedModel = %q; want the client's name and none", ext.Model, ext.RequestedModel)
	}
}

// A request with no model in its body — GET /v1/models — has nothing to map and is
// routed as it is.
func TestRouter_RoutesARequestWithNoModelAsItIs(t *testing.T) {
	p := build(t, mappedConfig(`"claude-code": "glm"`))
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	pctx.Method, pctx.Path = http.MethodGet, "/v1/models"
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	if pctx.BodyMutated() {
		t.Error("a request with no body was rewritten")
	}
}

// A body that names no model is routed as it is too: one that is not JSON, a file
// upload say, or JSON with no "model" key in any letter case.
func TestRouter_RoutesABodyThatNamesNoModelAsItIs(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":      "--boundary\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nmodel\r\n--boundary--\r\n",
		"no model key":  `{"events":[{"model":"claude-opus-5-5"}]}`,
		"not an object": `["model","claude-opus-5-5"]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := build(t, mappedConfig(`"claude-code": "glm"`))
			pctx := request(newStore(t), eteHost, claudeUA, "s1")
			pctx.Body = []byte(body)
			if a := run(t, p, pctx); a.Type != pipeline.Continue {
				t.Fatalf("action = %+v, want Continue", a)
			}
			assertRouted(t, pctx, glmHost, "glm-key")
			if got := string(pctx.Body); got != body || pctx.BodyMutated() {
				t.Errorf("body = %q, BodyMutated = %v; want it untouched", got, pctx.BodyMutated())
			}
		})
	}
}

// A body that names a model the router cannot read is the client's to fix, a 400,
// as the rewrite SetRequestModel refuses: an empty name, "model" twice in any
// letter case, a "model" under another case only, or one that is not a string.
// Routed unmapped it would reach the server under a name the server does not
// serve, or with a second "model" the rewrite cannot reach, which the server may
// be the one to read.
func TestRouter_RefusesAModelItCannotRead(t *testing.T) {
	for name, body := range map[string]string{
		"empty":             `{"model":"","messages":[]}`,
		"named twice":       `{"model":"claude-opus-5-5","Model":"claude-fable-5-1","messages":[]}`,
		"another case only": `{"MODEL":"claude-opus-5-5","messages":[]}`,
		"not a string":      `{"model":null,"messages":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			p := build(t, mappedConfig(`"claude-code": "glm"`))
			pctx := request(store, eteHost, claudeUA, "s1")
			pctx.Body = []byte(body)
			a := run(t, p, pctx)

			assertRefused(t, a, http.StatusBadRequest, "inference-router could not give this request glm's model: "+
				`its body must name the model once, as a non-empty string under \"model\" in lowercase`)
			assertRecord(t, pctx, pipeline.ActionDeny, "model_rewrite_failed", map[string]string{"server": "glm", "pin": pinNone})
			assertNotSent(t, pctx, store, body)
		})
	}
}

// Under observe nothing moves, so nothing is mapped either: the request goes where
// the client sent it, as the client sent it.
func TestRouter_UnderObserveMapsNothing(t *testing.T) {
	p := build(t, mappedConfig(`"claude-code": "glm"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	pctx := withModel(request(newStore(t), eteHost, claudeUA, "s1"), "claude-opus-5-5")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	if got := string(pctx.Body); got != messagesBody("claude-opus-5-5") || pctx.BodyMutated() {
		t.Errorf("body = %s, BodyMutated = %v; want it untouched", got, pctx.BodyMutated())
	}
	assertRecord(t, pctx, pipeline.ActionObserve, "would_route", map[string]string{"server": "glm"})
	// Not even a shadow rewrite: the router never asks for one under observe.
	if got, want := outboundReasons(pctx), []string{"modify/redirected", "observe/would_route"}; !slices.Equal(got, want) {
		t.Errorf("timeline = %v, want %v", got, want)
	}
}

// Under observe a name the server has no model for is not refused either: an
// observed router changes nothing about where a request goes or whether it does.
func TestRouter_UnderObserveRefusesNothing(t *testing.T) {
	p := build(t, mappedConfig(`"claude-code": "glm"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	pctx := withModel(request(newStore(t), eteHost, claudeUA, "s1"), "claude-fable-5-1")
	if a := run(t, p, pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %+v, want Continue", a)
	}
	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionObserve, "would_route", map[string]string{"server": "glm"})
}

// noBodyWrite is the router without WritesRequestBody, so the pipeline refuses its
// SetRequestModel: the one way to make the rewrite of a readable body fail from a
// test.
type noBodyWrite struct{ *Router }

func (n noBodyWrite) Capabilities() pipeline.PluginCapabilities {
	caps := n.Router.Capabilities()
	caps.WritesRequestBody = false
	return caps
}

// A rewrite the framework refuses is a 400, since everything it refuses in
// production is the client's body, and pins nothing.
func TestRouter_AFailedModelRewriteIsA400AndPinsNothing(t *testing.T) {
	store := newStore(t)
	r := New()
	if err := r.Configure(json.RawMessage(mappedConfig(`"claude-code": "glm"`))); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	failing, err := pipeline.New([]pipeline.Plugin{noBodyWrite{r}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pctx := withModel(request(store, eteHost, claudeUA, "s1"), "claude-opus-5-5")
	a := run(t, failing, pctx)

	assertRefused(t, a, http.StatusBadRequest, "inference-router could not give this request glm's model")
	assertRecord(t, pctx, pipeline.ActionDeny, "model_rewrite_failed", map[string]string{"server": "glm", "pin": pinNone})
	assertNotSent(t, pctx, store, messagesBody("claude-opus-5-5"))
}

// The laptop chain with the router last builds, and the parser's record of the
// request names both models once the router has mapped it. Before chained
// request-body writers, tool-prune and the router could not share the chain.
func TestRouter_RunsLastInTheLaptopChain(t *testing.T) {
	chain := []config.PluginEntry{
		{Name: "inference-parser"}, {Name: "mcp-parser"}, {Name: "a2a-parser"},
		{Name: "tool-prune", Config: json.RawMessage(`{"remove": []}`)},
		{Name: Name, Config: json.RawMessage(mappedConfig(`"claude-code": "glm"`))},
	}
	p, err := plugins.BuildWithDeps(chain,
		plugins.Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}})
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	pctx.Body = []byte(messagesBody("claude-opus-5-5"))
	run(t, p, pctx)

	ext := pctx.Extensions.Inference
	if ext == nil || ext.Model != "glm-big" || ext.RequestedModel != "claude-opus-5-5" {
		t.Fatalf("inference record = %+v, want Model glm-big and RequestedModel claude-opus-5-5", ext)
	}
	if got := string(pctx.Body); got != messagesBody("glm-big") {
		t.Errorf("body = %s, want %s", got, messagesBody("glm-big"))
	}
}

// The framework's reader rule places the router: a body reader after it is refused,
// since it would read the server's model rather than the client's.
func TestRouter_IsRefusedBeforeABodyReader(t *testing.T) {
	_, err := plugins.BuildWithDeps([]config.PluginEntry{
		{Name: Name, Config: json.RawMessage(mappedConfig(``))},
		{Name: "inference-parser"},
	}, plugins.Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}})
	if err == nil || !strings.Contains(err.Error(), `"inference-parser" reads body after mutator "inference-router"`) {
		t.Fatalf("err = %v, want the reader rule naming both", err)
	}
}
