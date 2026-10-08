package pipeline

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// MarkRedirectable says the listener builds the upstream request from this context
// itself, after the pipeline, so a Redirect can take effect. The forward proxy calls
// it for every request it re-originates, plain or TLS-bridged.
//
// A listener must not call it for a context it cannot redirect. The outbound
// pipeline also runs once per CONNECT and once per transparently redirected
// connection, and both dial the address the client chose whatever a plugin does: a
// redirect accepted there would be recorded and never happen.
//
// Listener-facing, like SetCurrentPlugin. Production plugins never call this.
func (c *Context) MarkRedirectable() { c.redirectable = true }

// Redirectable reports whether the listener will honor a Redirect on this request. A
// routing plugin checks it first, so that it acts only on requests it can move.
func (c *Context) Redirectable() bool { return c.redirectable }

// Redirect sends the request to target instead of the host the client named.
//
// target must be absolute — scheme http or https, a host, an optional port — and
// carry nothing else: no user info, no path beyond "/", no query, no fragment. A
// redirect changes where a request goes and nothing about what it says; the path,
// headers and body stay the plugin's to change through the existing APIs.
//
// It sets Scheme and Host to the target's. Host following the redirect is the point:
// the session events, usage, the cost ledger and modelled pricing all key on it, and
// they must describe where the bytes went. Plugins that run after this one and key on
// Host see the new host for the same reason. The host the client named stays
// available as RequestedHost. The validated target is also kept privately, and that
// copy is what the listener applies (see RedirectTarget): Scheme and Host are any
// plugin's to write, so a later write to them cannot move the request.
//
// The framework records the redirect as a modify/redirected Invocation with "from"
// and "to" in Details, as SetBody records a body rewrite: a redirect moves the request
// and its credentials to another host, so the timeline shows it even if the plugin
// records nothing of its own. Under on_error: observe nothing moves and the
// Invocation is marked Shadow, so plugin code looks the same under enforce and observe.
//
// What a redirect does not do. The request's headers — credentials included — go to
// the new host unchanged. And it does not gate the plugin's own header writes: under
// observe nothing moves while those still apply, so a plugin that attaches
// credentials meant for the target must do so only when the request actually goes
// there — check Redirected after the call, which stays false under observe and on a
// refusal, or compare Host with the target — or the target's key goes to the host the
// client named. Plugins earlier in the chain made their decisions on the requested
// host and are not run again.
//
// Refused, with nothing changed and nothing recorded, when the calling plugin does
// not declare WritesDestination, when the listener did not mark the context
// redirectable, when the target is malformed, and outside OnRequest.
func (c *Context) Redirect(target *url.URL) error {
	if c.inFinish {
		slog.Warn("pipeline: plugin called pctx.Redirect during OnFinish — refused (the response is already sent)",
			"plugin", c.currentPlugin)
		return errors.New("pipeline: Redirect refused in OnFinish: the request has already been sent")
	}
	if c.currentPhase != InvocationPhaseRequest {
		slog.Warn("pipeline: plugin called pctx.Redirect outside OnRequest — refused",
			"plugin", c.currentPlugin, "phase", c.currentPhase)
		return fmt.Errorf("pipeline: Redirect refused in phase %q: it is accepted only from OnRequest", c.currentPhase)
	}
	if !c.currentMayRedirect {
		return fmt.Errorf("pipeline: plugin %q called Redirect without declaring WritesDestination", c.currentPlugin)
	}
	if !c.redirectable {
		return errors.New("pipeline: this request cannot be redirected: the listener does not re-originate it (a CONNECT, or a transparently redirected connection)")
	}
	if err := checkRedirectTarget(target); err != nil {
		return err
	}
	details := map[string]string{"from": c.Host, "to": target.Host}
	if c.currentPolicy == ErrorPolicyObserve {
		c.Record(Invocation{Action: ActionModify, Reason: "redirected", Shadow: true, Details: details})
		return nil
	}
	if !c.redirected {
		c.requestedHost = c.Host
		c.redirected = true
	}
	c.redirectScheme, c.redirectHost = target.Scheme, target.Host
	c.Scheme, c.Host = target.Scheme, target.Host
	c.Record(Invocation{Action: ActionModify, Reason: "redirected", Details: details})
	return nil
}

// Redirected reports whether a Redirect took effect on this request, and is the ok
// RedirectTarget reports. The listener keys on that, not on RequestedHost: a redirect
// that changed only the scheme, or that pointed back at the host the client named,
// still has to be applied.
//
// It is also the gate for a plugin that attaches credentials meant for the target.
// The request's headers go wherever the request goes, and Redirect returns nil under
// on_error: observe without moving anything, so a key set on the strength of that nil
// alone would reach the host the client named. Redirected stays false under observe
// and after a refusal.
func (c *Context) Redirected() bool { return c.redirected }

// RedirectTarget is the scheme and host the last accepted Redirect validated, with ok
// exactly when Redirected. Listeners apply this, never the exported Scheme and Host:
// any plugin may write those, declared or not, so a listener that read them back would
// let a later plugin steer a redirected request somewhere no WritesDestination plugin
// chose, and the modify/redirected record would name a host the bytes never went to.
func (c *Context) RedirectTarget() (scheme, host string, ok bool) {
	if !c.redirected {
		return "", "", false
	}
	return c.redirectScheme, c.redirectHost, true
}

// RequestedHost is the host the client named when a redirect sent the request
// somewhere else, and "" otherwise — including when the redirects ended back at
// that host. It is what a session event records beside Host. It compares against
// the target RedirectTarget reports, not the exported Host, so a later write to Host
// can neither hide a redirect nor invent one.
func (c *Context) RequestedHost() string {
	if !c.redirected || strings.EqualFold(c.requestedHost, c.redirectHost) {
		return ""
	}
	return c.requestedHost
}

// checkRedirectTarget enforces Redirect's shape rule. Errors print the target
// through Redacted so a password in user info never reaches a log.
func checkRedirectTarget(u *url.URL) error {
	if u == nil {
		return errors.New("pipeline: Redirect target is nil")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("pipeline: Redirect target %q: the scheme must be http or https", u.Redacted())
	case u.Opaque != "" || u.Hostname() == "":
		return fmt.Errorf("pipeline: Redirect target %q has no host", u.Redacted())
	case u.User != nil:
		return fmt.Errorf("pipeline: Redirect target %q carries user info; credentials belong in headers", u.Redacted())
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("pipeline: Redirect target %q has a path; a redirect changes only the scheme and host", u.Redacted())
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("pipeline: Redirect target %q has a query or fragment", u.Redacted())
	}
	return nil
}
