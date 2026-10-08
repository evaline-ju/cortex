// Package inferencerouter is the inference-router plugin: it sends a coding agent's
// new sessions to the inference server chosen for that agent, and keeps every
// session on the server it started on.
//
// The choice is plugin config — servers by name, and agents by name to a server —
// changed by editing it and letting the proxy hot-reload. Sessions are pinned in the
// process-scoped store (pctx.Shared), which a reload does not replace, so changing
// an agent's server moves only the sessions it has not started yet.
//
// Place it last in the outbound chain, which is where agentop puts it. A redirect
// moves pctx.Host, so the plugins before it decide on the host the client asked for
// and any plugin after it would see the server's host instead. Nothing should follow
// it that keys on the host.
//
// A routed request is always redirected to its server's own scheme and host, even
// when it already names that host. pctx.Host is the request's Host header, the
// client's word, and on a TLS-bridged request the forward proxy dials the CONNECT
// authority, which need not be that host. A router that skipped the redirect because
// the Host header already named the server would hand the server's key to whatever
// the client CONNECTed to. With the redirect the listener dials RedirectTarget, the
// server, whatever the Host header or the CONNECT said.
//
// The server's key replaces the client's only when pctx.Redirected() reports that the
// redirect took effect. Under on_error: observe, Redirect returns nil and moves
// nothing, so a key set on the strength of that nil would go to the host the client
// named.
//
// A session is pinned by where its first request went, not by the choice made for
// it: to the server when the request was redirected there; to "not routed" when it
// stayed where the client sent it, because the agent is not routed or because the
// router runs under on_error: observe; and not at all when the redirect failed, so
// the session's next request decides again. A pin by choice would hold a session
// started under observe to a server it never used, and turning enforce on would then
// move it there mid-conversation, which is the switch the pin exists to prevent.
//
// The first request the router sees is not always a session's first. A session
// already running when routing is first configured, quiet while it was, has no pin,
// and neither has one whose pin lapsed. The session store's history tells such a
// session from a new one: on a pin miss for a routed agent, the latest earlier
// inference request that agent sent in the session to a server's host keeps the
// session on that server, and only a session with no such request is new and goes
// to its agent's current server. Without this, the documented first setup — add
// the servers, then route Claude Code — would move every conversation that sent
// nothing between the router's arrival and the route to the new server on its next
// turn. A request to any other host is no evidence: the router never routes one, so
// it says nothing about which server the session is on. Reading it as "not routed"
// would leave an agent that switches providers inside a session, as OpenCode does,
// unrouted for good once its first request went elsewhere, still sending its own
// key to the server it then addresses. Only a non-empty history is evidence: a
// view is empty for a session the store has recorded nothing of yet. After a
// restart the store holds nothing from before it, and the pins, which live in
// memory, are gone too, so a running session's next request then looks new and can
// move; so can a quiet one the store has evicted before the router pinned it.
//
// A pin is its agent's. The session id is the listener's answer, and another
// agent's request can be filed under it — by process attribution, which files a
// command an agent runs under that agent's session and is on by default on a
// laptop; by the ActiveSession fallback with client affinity off; or by a header id
// two clients share. Such a request is decided as if the session were unpinned, from
// its own agent's history and choice, and leaves the pin alone. Honouring the pin for
// it would hand it the session's server and key.
//
// The listener's synthetic sessions are never pinned: the default bucket, and the
// pending:<agent> buckets an agent's calls collect in before its session is known.
// Each holds many conversations rather than one, so a pin would hold every later
// conversation filed there to the first one's server. Their requests follow the
// agent's current server, unpinned, as a request with no session does.
package inferencerouter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

// Name is the plugin's registered name.
const Name = "inference-router"

const (
	// pinPrefix namespaces the plugin's keys in the shared store.
	pinPrefix = Name + "/pin/"

	// pinTTL is how long a pin outlives a session's last request. It slides: every
	// request renews it, so only a session idle this long forgets its server.
	pinTTL = 30 * 24 * time.Hour

	// codeUnavailable maps to 503 in pipeline's code table. A session whose server
	// cannot be reached through the router is the server being unavailable to it,
	// not the client's mistake.
	codeUnavailable = "upstream.unreachable"
)

// The pin detail on every record that resolved a server.
const (
	pinNew      = "new"      // this request pinned the session
	pinExisting = "existing" // the session was already pinned
	pinNone     = "none"     // nothing was pinned: no session, a synthetic one, no store, a failed redirect, or another agent's pin
)

// route is one configured server, ready to redirect to.
type route struct {
	endpoint routerconfig.Endpoint
	key      string
}

// target is the server's own scheme and host, where every request routed to it is
// redirected.
func (r route) target() *url.URL {
	return &url.URL{Scheme: r.endpoint.Scheme, Host: r.endpoint.Host}
}

// Router is the plugin. Built by Configure; the zero value routes nothing.
type Router struct {
	servers map[string]route  // by server name
	byHost  map[string]string // every server's Endpoint.Hostname, to its name
	agents  map[string]string // agent name to server name

	// noStore makes the "pins are off" warning once per instance rather than once
	// per request.
	noStore sync.Once
}

// New constructs an unconfigured plugin.
func New() *Router { return &Router{} }

func init() {
	plugins.RegisterPlugin(Name, func() pipeline.Plugin { return New() })
}

func (p *Router) Name() string { return Name }

func (p *Router) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		WritesDestination: true,
		Description:       "Sends each agent's new sessions to its chosen inference server.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider. servers and agents are maps, which
// pipeline.SchemaOf renders as type "unknown".
func (p *Router) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(routerconfig.Config{})
}

// Configure decodes and validates the config, then builds the routing tables. A
// failed Configure leaves the previous tables in place; BuildWithDeps discards the
// instance anyway.
func (p *Router) Configure(raw json.RawMessage) error {
	c, err := routerconfig.Decode(raw)
	if err != nil {
		return fmt.Errorf("inference-router config: %w", err)
	}
	servers := make(map[string]route, len(c.Servers))
	byHost := make(map[string]string, len(c.Servers)) // one name per host: Decode refuses two servers on one
	for name, s := range c.Servers {
		ep, err := routerconfig.ParseURL(s.URL)
		if err != nil { // Decode already accepted it; kept so a drift between the two fails here
			return fmt.Errorf("inference-router config: servers.%s.url: %w", name, err)
		}
		if ep.PlaintextRemote() {
			slog.Warn("inference-router: server is plain http on another machine, so every request routed to it "+
				"crosses the network decrypted, its key and prompt included; use https unless the network is trusted",
				"server", name, "url", ep.URL())
		}
		servers[name] = route{endpoint: ep, key: s.Key}
		byHost[ep.Hostname] = name
	}
	p.servers, p.byHost, p.agents = servers, byHost, c.Agents
	return nil
}

// OnRequest routes one request. See the package doc for what it decides and why.
func (p *Router) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	cont := pipeline.Action{Type: pipeline.Continue}

	// A CONNECT or a transparent connection is dialed where the client chose,
	// whatever a plugin does. Pinning on one would pin a session on a request that
	// cannot be routed, so it is left before anything is decided.
	if !pctx.Redirectable() {
		pctx.Skip("not_redirectable")
		return cont
	}
	// Every path on a server's host is handled — /v1/messages, count_tokens,
	// /v1/models — because they all belong to the server the session is on.
	if _, ok := p.byHost[routerconfig.Hostname(pctx.Host)]; !ok {
		pctx.Skip("not_an_inference_server")
		return cont
	}

	name, pin := p.serverFor(pctx)
	if name == "" {
		// Not routed: the request, its key included, stays exactly as the client sent it.
		pin.settle("")
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "not_routed",
			Details: map[string]string{"pin": pin.state}})
		return cont
	}
	// Built where each record is made, since a failed redirect changes the pin.
	details := func() map[string]string { return map[string]string{"server": name, "pin": pin.state} }
	srv, ok := p.servers[name]
	if !ok {
		// Removed since the session was pinned, by agentop server remove or a hand edit.
		// Moving the conversation to whatever the agent uses now is the switch this
		// plugin exists to prevent.
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "pinned_server_removed", Details: details()})
		return pipeline.Deny(codeUnavailable, fmt.Sprintf(
			"this session is pinned to inference server %q, which is no longer configured; add it back, or start a new session", name))
	}

	// Redirected even when the Host header already names the server: the listener
	// dials the redirect target, and without one it dials what the client chose,
	// which on a bridged request is the CONNECT authority and not the Host header.
	if err := pctx.Redirect(srv.target()); err != nil {
		// The request went nowhere, so a first request pins nothing: pinning the server
		// would hold the session to one it never reached, and the next request can
		// decide again.
		pin.forgo()
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "redirect_failed", Details: details()})
		return pipeline.Deny(codeUnavailable, fmt.Sprintf("inference-router could not send this request to %q: %v", name, err))
	}
	// Redirect returns nil under on_error: observe without moving anything. The
	// server's key must then stay off the request, which still goes where the client
	// sent it. Redirected is the signal that it moved, and it is this router's
	// redirect: a pipeline admits one WritesDestination plugin.
	if !pctx.Redirected() {
		// The request stayed put, so that is what a first request pins: a session that
		// started under observe stays put once enforce is on.
		pin.settle("")
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionObserve, Reason: "would_route", Details: details()})
		return cont
	}
	pin.settle(name)
	setKey(pctx, srv.key)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "routed", Details: details()})
	return cont
}

func (p *Router) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// pin is a session's pin as the shared store holds it: the agent whose session it
// is, and the server the session is on, "" for not routed.
type pin struct {
	agent  string
	server string
}

// pinning is one request's session pin: how it was decided, and for a
// session's first request where to store the outcome once OnRequest knows it.
type pinning struct {
	state string // pinNew, pinExisting or pinNone
	store pipeline.SharedStore
	key   string
	agent string
}

// settle pins a session on its first request to where that request went: server
// when it was redirected there, "" when it stayed where the client sent it. A
// session already pinned keeps its pin, which serverFor renewed.
func (pn *pinning) settle(server string) {
	if pn.state == pinNew {
		pn.store.Put(pn.key, pin{agent: pn.agent, server: server}, pinTTL)
	}
}

// forgo leaves a session unpinned after a first request that went nowhere, so its
// next request decides again. A session already pinned keeps its pin.
func (pn *pinning) forgo() {
	if pn.state == pinNew {
		pn.state = pinNone
	}
}

// serverFor is the server the request's session uses, "" for not routed, and the
// session's pin.
//
// A session its agent already pinned uses its pin, "not routed" included, so routing
// an agent later does not move the sessions it already has running; every request
// renews the pin. Otherwise the session is decided by unpinned, and the pin comes
// back pinNew for OnRequest to settle by where the request went (see the package
// doc) — except when the pin is another agent's, which this request must neither
// follow nor overwrite, so it comes back pinNone. Without a session to pin, whether
// none, a synthetic one or no store, the agent's choice applies and nothing is
// pinned: a synthetic session's history is many conversations', not this one's.
//
// Two first requests of one session racing can both miss and both store; they store
// the same outcome unless a reload lands between them, which is the case the pin
// cannot rule out and does not need to.
func (p *Router) serverFor(pctx *pipeline.Context) (string, pinning) {
	agent := agentOf(pctx)
	choice := p.agents[agent]
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return choice, pinning{state: pinNone}
	}
	if pctx.Shared == nil {
		p.noStore.Do(func() {
			slog.Warn("inference-router: this binary wires no process store, so sessions are not pinned: " +
				"each request follows its agent's current server, and changing it moves running sessions")
		})
		return choice, pinning{state: pinNone}
	}
	key := pinPrefix + pctx.Session.ID
	if v, ok := pctx.Shared.Get(key); ok {
		if pn, ok := v.(pin); ok {
			if pn.agent != agent {
				return p.unpinned(pctx.Session, agent, choice), pinning{state: pinNone}
			}
			pctx.Shared.Put(key, pn, pinTTL)
			return pn.server, pinning{state: pinExisting}
		}
	}
	return p.unpinned(pctx.Session, agent, choice), pinning{state: pinNew, store: pctx.Shared, key: key, agent: agent}
}

// unpinned is the server for a request in a session its agent holds no pin on: the
// server the session's history shows it already uses, or for a new session — no
// earlier request to any server — the agent's current choice. An agent that is not
// routed is left alone, whatever its history.
func (p *Router) unpinned(s *pipeline.SessionView, agent, choice string) string {
	if choice == "" {
		return ""
	}
	if server, ok := p.wentTo(s.Events, agent); ok {
		return server
	}
	return choice
}

// wentTo is the server the latest earlier inference request agent sent in events
// to a server's host went to; ok is false when there is no such request, which is
// what makes a session new.
//
// Only an outbound request row a parser read as inference counts, and only one that
// reached a server. A tunnel row, a count_tokens or a /v1/models request no parser
// claimed, and a denied request, which went nowhere, say nothing about where the
// conversation is; nor does a request to any other host, which the router never
// routes (see the package doc). Another agent's row says nothing about this agent's
// conversation, and following it would hand this request that agent's server and
// key. A row's Host is where the bytes went, the server's host for a routed request,
// because the listener records it after the redirect; RequestedHost, where the
// client asked to go, is not where the session is.
func (p *Router) wentTo(events []pipeline.SessionEvent, agent string) (server string, ok bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := &events[i]
		if e.Direction != pipeline.Outbound || e.Phase != pipeline.SessionRequest || e.Inference == nil ||
			pipeline.AgentName(e.Client.Label()) != agent {
			continue
		}
		if server, ok := p.byHost[routerconfig.Hostname(e.Host)]; ok {
			return server, true
		}
	}
	return "", false
}

// synthetic reports a session id the listener files traffic under when it knows no
// one conversation: the default bucket, or an agent's pending bucket. See the
// package doc for why those are never pinned.
func synthetic(id string) bool {
	return id == session.DefaultSessionID || strings.HasPrefix(id, session.PendingPrefix)
}

// agentOf is the request's agent as the session store and agentop name it, or ""
// when the request carried no User-Agent.
func agentOf(pctx *pipeline.Context) string {
	agent := pipeline.AgentName(pctx.ClientInfo().Label())
	if agent == pipeline.UnknownClientLabel {
		return ""
	}
	return agent
}

// setKey puts key in the header the client authenticated with: X-Api-Key when it
// sent one, Authorization otherwise, both when it sent both. That keeps the header
// Claude Code uses whichever of ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN set it up.
func setKey(pctx *pipeline.Context, key string) {
	if pctx.Headers == nil {
		pctx.Headers = http.Header{}
	}
	h := pctx.Headers
	apiKey := len(h.Values("X-Api-Key")) > 0
	if apiKey {
		h.Set("X-Api-Key", key)
	}
	if !apiKey || len(h.Values("Authorization")) > 0 {
		h.Set("Authorization", "Bearer "+key)
	}
}

var (
	_ pipeline.Plugin         = (*Router)(nil)
	_ pipeline.Configurable   = (*Router)(nil)
	_ pipeline.SchemaProvider = (*Router)(nil)
)
