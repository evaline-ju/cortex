package pipeline

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// capPlugin declares a capability set and does nothing, for build-time rules.
type capPlugin struct {
	name string
	caps PluginCapabilities
}

func (p *capPlugin) Name() string                     { return p.name }
func (p *capPlugin) Capabilities() PluginCapabilities { return p.caps }
func (p *capPlugin) OnRequest(context.Context, *Context) Action {
	return Action{Type: Continue}
}
func (p *capPlugin) OnResponse(context.Context, *Context) Action {
	return Action{Type: Continue}
}

func destWriter(name string) *capPlugin {
	return &capPlugin{name: name, caps: PluginCapabilities{WritesDestination: true, Description: "test"}}
}

func TestNew_RejectsTwoDestinationWriters(t *testing.T) {
	_, err := New([]Plugin{destWriter("first"), destWriter("second")})
	if err == nil {
		t.Fatal("New accepted two WritesDestination plugins; the request can go to only one place")
	}
	for _, want := range []string{"WritesDestination", `"first"`, `"second"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestNew_AcceptsOneDestinationWriter(t *testing.T) {
	bystander := &capPlugin{name: "bystander", caps: PluginCapabilities{Description: "test"}}
	if _, err := New([]Plugin{destWriter("router"), bystander}); err != nil {
		t.Fatalf("New rejected a single WritesDestination plugin: %v", err)
	}
}

// A plugin that rewrites a body AND chooses a destination is still one destination
// writer: the mutator branch of validateCapabilities ends in a continue, and the
// destination check must not sit behind it.
func TestNew_CountsADestinationWriterThatAlsoMutates(t *testing.T) {
	both := &capPlugin{name: "both", caps: PluginCapabilities{WritesDestination: true, WritesRequestBody: true, Description: "test"}}
	if _, err := New([]Plugin{both, destWriter("second")}); err == nil {
		t.Fatal("New accepted a second WritesDestination plugin after one that also writes the body")
	}
}

// fnPlugin runs closures from its hooks, so each test states exactly what the
// plugin does and from where.
type fnPlugin struct {
	name       string
	caps       PluginCapabilities
	onRequest  func(*Context)
	onResponse func(*Context)
}

func (p *fnPlugin) Name() string                     { return p.name }
func (p *fnPlugin) Capabilities() PluginCapabilities { return p.caps }
func (p *fnPlugin) OnRequest(_ context.Context, pctx *Context) Action {
	if p.onRequest != nil {
		p.onRequest(pctx)
	}
	return Action{Type: Continue}
}
func (p *fnPlugin) OnResponse(_ context.Context, pctx *Context) Action {
	if p.onResponse != nil {
		p.onResponse(pctx)
	}
	return Action{Type: Continue}
}

// finishPlugin adds an OnFinish hook to fnPlugin.
type finishPlugin struct {
	fnPlugin
	onFinish func(*Context)
}

func (p *finishPlugin) OnFinish(_ context.Context, pctx *Context) { p.onFinish(pctx) }

// redirector is a plugin whose OnRequest runs do; declares sets WritesDestination.
func redirector(declares bool, do func(*Context)) *fnPlugin {
	return &fnPlugin{
		name:      "router",
		caps:      PluginCapabilities{WritesDestination: declares, Description: "test"},
		onRequest: do,
	}
}

// mustURL parses s. Every malformed target these tests use parses as a URL; it is
// Redirect that must refuse it.
func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// runRequest runs a one-plugin pipeline's request pass over an outbound context for
// http://a.example, marked redirectable when redirectable is true.
func runRequest(t *testing.T, plugin Plugin, redirectable bool, opts ...Option) (*Pipeline, *Context) {
	t.Helper()
	p, err := New([]Plugin{plugin}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Direction: Outbound, Scheme: "http", Host: "a.example"}
	if redirectable {
		pctx.MarkRedirectable()
	}
	p.Run(context.Background(), pctx)
	return p, pctx
}

func outbound(pctx *Context) []Invocation {
	if pctx.Extensions.Invocations == nil {
		return nil
	}
	return pctx.Extensions.Invocations.Outbound
}

// assertUntouched fails unless pctx still points where the client asked.
func assertUntouched(t *testing.T, pctx *Context) {
	t.Helper()
	if pctx.Scheme != "http" || pctx.Host != "a.example" {
		t.Errorf("destination = %s://%s, want the client's http://a.example", pctx.Scheme, pctx.Host)
	}
	if pctx.Redirected() || pctx.RequestedHost() != "" {
		t.Errorf("Redirected() = %v, RequestedHost() = %q; want false and empty", pctx.Redirected(), pctx.RequestedHost())
	}
}

func TestRedirect_SendsTheRequestToTarget(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		err = c.Redirect(mustURL("https://b.example:8443"))
	}), true)

	if err != nil {
		t.Fatalf("Redirect: %v", err)
	}
	if pctx.Scheme != "https" || pctx.Host != "b.example:8443" {
		t.Errorf("destination = %s://%s, want https://b.example:8443", pctx.Scheme, pctx.Host)
	}
	if !pctx.Redirected() {
		t.Error("Redirected() = false after a redirect took effect")
	}
	if got := pctx.RequestedHost(); got != "a.example" {
		t.Errorf("RequestedHost() = %q, want a.example", got)
	}
	invs := outbound(pctx)
	if len(invs) != 1 {
		t.Fatalf("invocations = %+v, want exactly the framework's redirect record", invs)
	}
	inv := invs[0]
	if inv.Plugin != "router" || inv.Action != ActionModify || inv.Reason != "redirected" || inv.Shadow ||
		inv.Details["from"] != "a.example" || inv.Details["to"] != "b.example:8443" {
		t.Errorf("invocation = %+v, want router modify/redirected from a.example to b.example:8443", inv)
	}
}

func TestRedirect_ASecondRedirectKeepsTheFirstRequestedHost(t *testing.T) {
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		_ = c.Redirect(mustURL("https://b.example"))
		_ = c.Redirect(mustURL("https://c.example"))
	}), true)
	if pctx.Host != "c.example" {
		t.Errorf("Host = %q, want the last target c.example", pctx.Host)
	}
	if got := pctx.RequestedHost(); got != "a.example" {
		t.Errorf("RequestedHost() = %q, want the host the CLIENT named, a.example", got)
	}
}

func TestRedirect_BackToTheRequestedHostReportsNoRequestedHost(t *testing.T) {
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		_ = c.Redirect(mustURL("https://b.example"))
		_ = c.Redirect(mustURL("http://a.example"))
	}), true)
	if !pctx.Redirected() {
		t.Error("Redirected() = false; the listener must still apply the final scheme and host")
	}
	if got := pctx.RequestedHost(); got != "" {
		t.Errorf("RequestedHost() = %q; the request goes where the client asked, so there is nothing to record", got)
	}
}

func TestRedirect_RefusesMalformedTargets(t *testing.T) {
	for _, target := range []string{
		"ftp://b.example",           // scheme
		"b.example",                 // no scheme: parses as a path
		"https://",                  // no host
		"https:b.example",           // opaque, no host
		"https://user:pw@b.example", // credentials belong in headers
		"https://b.example/v1",      // a redirect moves the host, never the path
		"https://b.example?x=1",     // query
		"https://b.example#top",     // fragment
	} {
		t.Run(target, func(t *testing.T) {
			var err error
			_, pctx := runRequest(t, redirector(true, func(c *Context) {
				err = c.Redirect(mustURL(target))
			}), true)
			if err == nil {
				t.Fatal("Redirect accepted a malformed target")
			}
			assertUntouched(t, pctx)
			if invs := outbound(pctx); len(invs) != 0 {
				t.Errorf("a refused redirect recorded %+v", invs)
			}
		})
	}
}

func TestRedirect_RefusesANilTarget(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) { err = c.Redirect(nil) }), true)
	if err == nil {
		t.Fatal("Redirect(nil) succeeded")
	}
	assertUntouched(t, pctx)
}

func TestRedirect_RefusedWithoutTheCapability(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(false, func(c *Context) {
		err = c.Redirect(mustURL("https://b.example"))
	}), true)
	if err == nil || !strings.Contains(err.Error(), "WritesDestination") {
		t.Fatalf("Redirect from an undeclared plugin = %v, want a refusal naming WritesDestination", err)
	}
	assertUntouched(t, pctx)
}

func TestRedirect_RefusedWhereTheListenerCannotHonorIt(t *testing.T) {
	var err error
	var redirectable bool
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		redirectable = c.Redirectable()
		err = c.Redirect(mustURL("https://b.example"))
	}), false)
	if redirectable {
		t.Error("Redirectable() = true on a context no listener marked")
	}
	if err == nil || !strings.Contains(err.Error(), "cannot be redirected") {
		t.Fatalf("Redirect on an unmarked context = %v, want a refusal", err)
	}
	assertUntouched(t, pctx)
}

func TestRedirect_UnderObserveRecordsAShadowAndChangesNothing(t *testing.T) {
	var err error
	_, pctx := runRequest(t, redirector(true, func(c *Context) {
		err = c.Redirect(mustURL("https://b.example"))
	}), true, WithPolicies(ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("Redirect under observe: %v; plugin code must not see a difference", err)
	}
	assertUntouched(t, pctx)
	invs := outbound(pctx)
	if len(invs) != 1 || invs[0].Reason != "redirected" || !invs[0].Shadow {
		t.Errorf("invocations = %+v, want one shadow modify/redirected", invs)
	}
}

func TestRedirect_RefusedInTheResponsePass(t *testing.T) {
	var err error
	plug := redirector(true, nil)
	plug.onResponse = func(c *Context) { err = c.Redirect(mustURL("https://b.example")) }
	p, pctx := runRequest(t, plug, true)
	p.RunResponse(context.Background(), pctx)
	if err == nil || !strings.Contains(err.Error(), "OnRequest") {
		t.Fatalf("Redirect from OnResponse = %v, want a refusal", err)
	}
	assertUntouched(t, pctx)
}

func TestRedirect_RefusedInOnFinish(t *testing.T) {
	var err error
	plug := &finishPlugin{
		fnPlugin: *redirector(true, nil),
		onFinish: func(c *Context) { err = c.Redirect(mustURL("https://b.example")) },
	}
	p, pctx := runRequest(t, plug, true)
	p.RunFinish(context.Background(), pctx, Outcome{})
	if err == nil || !strings.Contains(err.Error(), "OnFinish") {
		t.Fatalf("Redirect from OnFinish = %v, want a refusal", err)
	}
	assertUntouched(t, pctx)
}
