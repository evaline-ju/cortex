package inferencerouter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

const (
	claudeUA   = "claude-cli/2.1.286 (external, cli)"
	opencodeUA = "opencode/latest/2.0.21/cli"

	eteHost = "ete.example.com"
	glmHost = "glm.example.com:8443"
)

// routerConfig is two servers and the agents block given, which may be empty.
func routerConfig(agents string) string {
	return `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key"},
			"glm": {"url": "https://glm.example.com:8443", "key": "glm-key"}
		},
		"agents": {` + agents + `}
	}`
}

// build configures a router and wraps it in a one-plugin pipeline. A redirect is
// accepted only from inside Pipeline.Run, so OnRequest is never called directly.
func build(t *testing.T, config string, opts ...pipeline.Option) *pipeline.Pipeline {
	t.Helper()
	r := New()
	if err := r.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{r}, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

func newStore(t *testing.T) *memstore.Store {
	t.Helper()
	s := memstore.New()
	t.Cleanup(s.Close)
	return s
}

// request is a context the forward proxy would build for a decrypted request to
// host from an agent with User-Agent ua, in session (none when ""), carrying the
// client's own key as a bearer token.
func request(store pipeline.SharedStore, host, ua, session string) *pipeline.Context {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    http.MethodPost,
		Scheme:    "https",
		Host:      host,
		Path:      "/v1/messages",
		Headers:   http.Header{"User-Agent": {ua}, "Authorization": {"Bearer client-key"}},
	}
	if store != nil {
		pctx.Shared = store
	}
	if session != "" {
		pctx.Session = &pipeline.SessionView{ID: session}
	}
	pctx.ResolveClient()
	pctx.MarkRedirectable()
	return pctx
}

func run(t *testing.T, p *pipeline.Pipeline, pctx *pipeline.Context) pipeline.Action {
	t.Helper()
	return p.Run(context.Background(), pctx)
}

// routerRecord is the router's own invocation: the last one on the outbound pass,
// after the framework's modify/redirected when there was a redirect.
func routerRecord(t *testing.T, pctx *pipeline.Context) pipeline.Invocation {
	t.Helper()
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) == 0 {
		t.Fatal("the router recorded nothing")
	}
	invs := pctx.Extensions.Invocations.Outbound
	return invs[len(invs)-1]
}

func assertRecord(t *testing.T, pctx *pipeline.Context, action pipeline.InvocationAction, reason string, details map[string]string) {
	t.Helper()
	inv := routerRecord(t, pctx)
	if inv.Plugin != Name || inv.Action != action || inv.Reason != reason {
		t.Errorf("record = %s %s/%s, want %s %s/%s", inv.Plugin, inv.Action, inv.Reason, Name, action, reason)
	}
	for k, want := range details {
		if got := inv.Details[k]; got != want {
			t.Errorf("Details[%q] = %q, want %q (all: %v)", k, got, want, inv.Details)
		}
	}
}

// assertUntouched fails unless pctx still goes to host with the client's own key.
func assertUntouched(t *testing.T, pctx *pipeline.Context, host string) {
	t.Helper()
	if pctx.Host != host || pctx.Redirected() {
		t.Errorf("Host = %q, Redirected = %v; want %q and no redirect", pctx.Host, pctx.Redirected(), host)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer client-key" {
		t.Errorf("Authorization = %q, want the client's own key", got)
	}
}

// assertRouted fails unless pctx goes to host with key as its bearer token.
func assertRouted(t *testing.T, pctx *pipeline.Context, host, key string) {
	t.Helper()
	if pctx.Host != host {
		t.Errorf("Host = %q, want %q", pctx.Host, host)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer "+key {
		t.Errorf("Authorization = %q, want Bearer %s", got, key)
	}
}

func pinOf(t *testing.T, store *memstore.Store, session string) (string, bool) {
	t.Helper()
	v, ok := store.Get(pinPrefix + session)
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

func TestRouter_IgnoresAHostThatIsNoServer(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, "api.anthropic.com", claudeUA, "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, "api.anthropic.com")
	assertRecord(t, pctx, pipeline.ActionSkip, "not_an_inference_server", nil)
	if _, ok := pinOf(t, store, "s1"); ok {
		t.Error("a request to no server pinned its session")
	}
}

// A CONNECT is dialed where the client chose. Pinning on it would pin a session on
// a request that cannot be routed.
func TestRouter_IgnoresAContextTheListenerCannotRedirect(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: http.MethodConnect, Scheme: "tcp", Host: eteHost + ":443",
		Headers: http.Header{"User-Agent": {claudeUA}}, Shared: store, Session: &pipeline.SessionView{ID: "s1"},
	}
	run(t, p, pctx)

	assertRecord(t, pctx, pipeline.ActionSkip, "not_redirectable", nil)
	if _, ok := pinOf(t, store, "s1"); ok {
		t.Error("a CONNECT pinned its session")
	}
}

func TestRouter_LeavesAnUnlistedAgentAloneAndPinsItAsNotRouted(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, opencodeUA, "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNew})
	if pin, ok := pinOf(t, store, "s1"); !ok || pin != "" {
		t.Errorf("pin = %q, %v; want the session pinned as not routed (\"\")", pin, ok)
	}
}

func TestRouter_DoesNotRouteARequestWithNoUserAgent(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(newStore(t), eteHost, "", "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", nil)
}

func TestRouter_RoutesAListedAgentsSessionToItsServer(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	if a := run(t, p, pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %+v, want Continue", a)
	}

	assertRouted(t, pctx, glmHost, "glm-key")
	if !pctx.Redirected() || pctx.RequestedHost() != eteHost {
		t.Errorf("Redirected = %v, RequestedHost = %q; want a redirect from %s", pctx.Redirected(), pctx.RequestedHost(), eteHost)
	}
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
	if pin, _ := pinOf(t, store, "s1"); pin != "glm" {
		t.Errorf("pin = %q, want glm", pin)
	}
}

// Every path on a server's host belongs to the session's server, not only the
// inference call.
func TestRouter_RoutesEveryPathOnAServersHost(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	pctx.Method, pctx.Path = http.MethodGet, "/v1/models"
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
}

func TestRouter_PutsTheKeyInTheHeaderTheClientSent(t *testing.T) {
	for _, tc := range []struct {
		name               string
		sent               http.Header
		wantAPIKey, wantAu string
	}{
		{"x-api-key only", http.Header{"X-Api-Key": {"client-key"}}, "glm-key", ""},
		{"authorization only", http.Header{"Authorization": {"Bearer client-key"}}, "", "Bearer glm-key"},
		{"both", http.Header{"X-Api-Key": {"client-key"}, "Authorization": {"Bearer client-key"}}, "glm-key", "Bearer glm-key"},
		{"neither", http.Header{}, "", "Bearer glm-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, routerConfig(`"claude-code": "glm"`))
			pctx := request(newStore(t), eteHost, claudeUA, "s1")
			pctx.Headers = tc.sent
			pctx.Headers.Set("User-Agent", claudeUA)
			run(t, p, pctx)

			if got := pctx.Headers.Get("X-Api-Key"); got != tc.wantAPIKey {
				t.Errorf("X-Api-Key = %q, want %q", got, tc.wantAPIKey)
			}
			if got := pctx.Headers.Get("Authorization"); got != tc.wantAu {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAu)
			}
		})
	}
}

// The switching story rests on these two: a choice made now applies only to
// sessions that have not started.
func TestRouter_RoutingAnAgentLaterDoesNotMoveASessionPinnedAsNotRouted(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(``))
	run(t, before, request(store, eteHost, claudeUA, "running"))

	after := build(t, routerConfig(`"claude-code": "glm"`))
	running := request(store, eteHost, claudeUA, "running")
	run(t, after, running)
	assertUntouched(t, running, eteHost)
	assertRecord(t, running, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinExisting})

	fresh := request(store, eteHost, claudeUA, "fresh")
	run(t, after, fresh)
	assertRouted(t, fresh, glmHost, "glm-key")
}

func TestRouter_MovingAnAgentDoesNotMoveASessionPinnedToTheFirstServer(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "running"))

	after := build(t, routerConfig(`"claude-code": "ete"`))
	running := request(store, eteHost, claudeUA, "running")
	run(t, after, running)
	assertRouted(t, running, glmHost, "glm-key")
	assertRecord(t, running, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinExisting})

	fresh := request(store, eteHost, claudeUA, "fresh")
	run(t, after, fresh)
	assertRouted(t, fresh, eteHost, "ete-key")
}

func TestRouter_ASessionlessRequestFollowsTheCurrentChoiceUnpinned(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "")
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"pin": pinNone})
}

func TestRouter_APinToARemovedServerIsA503(t *testing.T) {
	store := newStore(t)
	store.Put(pinPrefix+"s1", "east", pinTTL)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	a := run(t, p, pctx)

	if a.Type != pipeline.Reject {
		t.Fatalf("action = %+v, want Reject", a)
	}
	status, _, body := a.Violation.Render()
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	if want := `pinned to inference server \"east\", which is no longer configured`; !strings.Contains(string(body), want) {
		t.Errorf("body = %s, want it to contain %s", body, want)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "pinned_server_removed", map[string]string{"server": "east", "pin": pinExisting})
	assertUntouched(t, pctx, eteHost)
}

// Claude Code pointed at a server it is routed to: nothing to move, but the
// configured key still replaces the client's, and no requested host is recorded —
// including when the request names the default port the server's URL leaves out.
func TestRouter_AServerOnTheRequestedHostGetsItsKeyWithoutARedirect(t *testing.T) {
	for _, host := range []string{eteHost, "ETE.example.com:443"} {
		t.Run(host, func(t *testing.T) {
			p := build(t, routerConfig(`"claude-code": "ete"`))
			pctx := request(newStore(t), host, claudeUA, "s1")
			run(t, p, pctx)

			if pctx.Redirected() || pctx.RequestedHost() != "" || pctx.Host != host {
				t.Errorf("Redirected = %v, RequestedHost = %q, Host = %q; want no redirect",
					pctx.Redirected(), pctx.RequestedHost(), pctx.Host)
			}
			if got := pctx.Headers.Get("Authorization"); got != "Bearer ete-key" {
				t.Errorf("Authorization = %q, want Bearer ete-key", got)
			}
			if invs := pctx.Extensions.Invocations.Outbound; len(invs) != 1 {
				t.Errorf("invocations = %+v, want only the router's routed record", invs)
			}
		})
	}
}

// Under observe, Redirect returns nil and moves nothing. The server's key must not
// reach the host the client named.
func TestRouter_UnderObserveLeavesTheClientsKeyAlone(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionObserve, "would_route", map[string]string{"server": "glm"})
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 2 || invs[0].Reason != "redirected" || !invs[0].Shadow {
		t.Errorf("invocations = %+v, want the framework's shadow redirect, then would_route", invs)
	}
}

func TestRouter_WithoutAStoreRoutesUnpinnedAndWarnsOnce(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := build(t, routerConfig(`"claude-code": "glm"`))
	for range 2 {
		pctx := request(nil, eteHost, claudeUA, "s1")
		run(t, p, pctx)
		assertRouted(t, pctx, glmHost, "glm-key")
		assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"pin": pinNone})
	}
	if n := strings.Count(logs.String(), "sessions are not pinned"); n != 1 {
		t.Errorf("warned %d times, want once:\n%s", n, logs.String())
	}
}

func TestConfigure_WrapsErrorsInThePluginsName(t *testing.T) {
	err := New().Configure(json.RawMessage(`{}`))
	if err == nil || !strings.HasPrefix(err.Error(), "inference-router config: ") {
		t.Fatalf("err = %v, want it prefixed with inference-router config:", err)
	}
}

func TestConfigure_WarnsOnPlainHTTPToAnotherMachine(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := New().Configure(json.RawMessage(`{"servers": {
		"lan": {"url": "http://10.0.0.5:4000", "key": "k"},
		"local": {"url": "http://localhost:4000", "key": "k"}}}`))
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "server=lan") || !strings.Contains(out, "plain http") {
		t.Errorf("no plaintext warning for lan:\n%s", out)
	}
	if strings.Contains(out, "server=local") {
		t.Errorf("warned about a loopback server:\n%s", out)
	}
}

func TestCapabilities_DeclareARedirectAndNoBody(t *testing.T) {
	caps := New().Capabilities()
	if !caps.WritesDestination || caps.ReadsBody || caps.WritesRequestBody || caps.WritesResponseBody {
		t.Errorf("caps = %+v, want WritesDestination alone", caps)
	}
	if n := len(caps.Description); n == 0 || n > 80 {
		t.Errorf("description is %d chars, want 1-80", n)
	}
}

// routerconfig copies the no-User-Agent label rather than importing pipeline.
func TestNoAgentIsThePipelinesUnknownLabel(t *testing.T) {
	if routerconfig.NoAgent != pipeline.UnknownClientLabel {
		t.Errorf("routerconfig.NoAgent = %q, pipeline.UnknownClientLabel = %q", routerconfig.NoAgent, pipeline.UnknownClientLabel)
	}
}

// A listener that cannot honor a redirect refuses the plugin at build time.
func TestRouter_IsRefusedWhereTheListenerCannotRedirect(t *testing.T) {
	_, err := plugins.BuildWithDeps(entries(routerConfig(``)), plugins.Deps{})
	if err == nil || !strings.Contains(err.Error(), "WritesDestination") {
		t.Fatalf("err = %v, want a refusal naming WritesDestination", err)
	}
}
