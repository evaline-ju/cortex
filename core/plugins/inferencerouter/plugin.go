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
package inferencerouter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
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
	pinNone     = "none"     // no session, or no store: nothing was pinned
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
	servers   map[string]route  // by server name
	hostnames map[string]bool   // every server's Endpoint.Hostname
	agents    map[string]string // agent name to server name

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
	hostnames := make(map[string]bool, len(c.Servers))
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
		hostnames[ep.Hostname] = true
	}
	p.servers, p.hostnames, p.agents = servers, hostnames, c.Agents
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
	if !p.hostnames[routerconfig.Hostname(pctx.Host)] {
		pctx.Skip("not_an_inference_server")
		return cont
	}

	name, pin := p.serverFor(pctx)
	if name == "" {
		// Not routed: the request, its key included, stays exactly as the client sent it.
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "not_routed",
			Details: map[string]string{"pin": pin}})
		return cont
	}
	details := map[string]string{"server": name, "pin": pin}
	srv, ok := p.servers[name]
	if !ok {
		// Removed by a hand edit since the session was pinned. Moving the conversation
		// to whatever the agent uses now is the switch this plugin exists to prevent.
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "pinned_server_removed", Details: details})
		return pipeline.Deny(codeUnavailable, fmt.Sprintf(
			"this session is pinned to inference server %q, which is no longer configured; add it back, or start a new session", name))
	}

	// Redirected even when the Host header already names the server: the listener
	// dials the redirect target, and without one it dials what the client chose,
	// which on a bridged request is the CONNECT authority and not the Host header.
	if err := pctx.Redirect(srv.target()); err != nil {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "redirect_failed", Details: details})
		return pipeline.Deny(codeUnavailable, fmt.Sprintf("inference-router could not send this request to %q: %v", name, err))
	}
	// Redirect returns nil under on_error: observe without moving anything. The
	// server's key must then stay off the request, which still goes where the client
	// sent it. Redirected is the signal that it moved, and it is this router's
	// redirect: a pipeline admits one WritesDestination plugin.
	if !pctx.Redirected() {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionObserve, Reason: "would_route", Details: details})
		return cont
	}
	setKey(pctx, srv.key)
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "routed", Details: details})
	return cont
}

func (p *Router) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// serverFor is the server the request's session uses, "" for not routed, and how
// that was decided.
//
// A session's first request pins the agent's choice at that moment, "not routed"
// included, so routing an agent later does not move the sessions it already has
// running. Every request renews the pin. Without a session there is nothing to pin
// and the agent's current choice applies.
//
// Two first requests of one session racing can both miss and both store; they store
// the same choice unless a reload lands between them, which is the case the pin
// cannot rule out and does not need to.
func (p *Router) serverFor(pctx *pipeline.Context) (server, pin string) {
	choice := p.agents[agentOf(pctx)]
	if pctx.Session == nil || pctx.Session.ID == "" {
		return choice, pinNone
	}
	if pctx.Shared == nil {
		p.noStore.Do(func() {
			slog.Warn("inference-router: this binary wires no process store, so sessions are not pinned: " +
				"each request follows its agent's current server, and changing it moves running sessions")
		})
		return choice, pinNone
	}
	key := pinPrefix + pctx.Session.ID
	if v, ok := pctx.Shared.Get(key); ok {
		if s, ok := v.(string); ok {
			pctx.Shared.Put(key, s, pinTTL)
			return s, pinExisting
		}
	}
	pctx.Shared.Put(key, choice, pinTTL)
	return choice, pinNew
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
