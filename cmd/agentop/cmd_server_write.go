package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/x/term"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// serverConfirm asks before `server add` replaces a server. Its own var, as
// claudeCodeConfirm is, so a test stubbing it cannot disarm another command's prompt.
var serverConfirm = confirm

// writePluginConfig is edit.WritePluginConfig. A var so a test can stand in for a
// proxy that stops answering mid-poll, which the real poller takes about 16s of
// backoff to give up on.
var writePluginConfig = edit.WritePluginConfig

// readServerKey reads a server's API key at a prompt that does not echo, on the
// controlling terminal rather than stdin, so it works when stdin is a pipe and the
// prompt is not written into redirected output. A var so tests can stand in for the
// terminal a test process does not have.
var readServerKey = func(name string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to read the key from; pipe it in with --key-stdin")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "API key for %s: ", name)
	b, err := term.ReadPassword(tty.Fd())
	fmt.Fprintln(tty)
	if err != nil {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	return string(b), nil
}

// stdinIsTerminal reports whether stdin is a terminal. A var so a test can say it
// is: a test's stdin never is.
var stdinIsTerminal = func(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// readKey is the key for server name: from stdin with --key-stdin, else at the
// hidden prompt. Never from an argument, which would put it in shell history.
func readKey(name string, fromStdin bool, stdin io.Reader) (string, error) {
	if !fromStdin {
		return readServerKey(name)
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return "", fmt.Errorf("reading the key from stdin: %w", err)
	}
	key := strings.TrimRight(string(b), "\r\n")
	if strings.ContainsAny(key, "\r\n") {
		return "", errors.New("stdin holds more than one line; --key-stdin reads the key alone")
	}
	return key, nil
}

func serverAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server add", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	keyStdin := flags.Bool("key-stdin", false, "read the API key from stdin instead of a prompt")
	yes := flags.Bool("yes", false, "replace an existing server without asking")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "agentop server add: want a name and a URL: agentop server add <name> <url>")
		return 2
	}
	name, rawURL := pos[0], pos[1]
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	// ParseURL's error, never rawURL: a refused URL may carry a key anywhere in it,
	// and ParseURL's errors quote no part of the URL.
	ep, err := routerconfig.ParseURL(rawURL)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	// Before anything is asked or read: --key-stdin reads stdin as typed, so on a
	// terminal the key would echo, character by character, onto the screen and into
	// any recording of it. The prompt without the flag reads it without echo.
	if *keyStdin && stdinIsTerminal(stdin) {
		fmt.Fprintln(stderr, "agentop server add: --key-stdin reads the key from stdin, and stdin is a terminal, "+
			"so the key would echo as you typed it. Pipe the key in, or drop --key-stdin to be asked for it without echo.")
		return 1
	}

	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	for _, other := range slices.Sorted(maps.Keys(c.Servers)) {
		if oep, err := routerconfig.ParseURL(c.Servers[other].URL); other != name && err == nil && oep.Hostname == ep.Hostname {
			fmt.Fprintf(stderr, "agentop server add: %s is already on %s. Each server needs a host of its own, "+
				"because agentop names a session's server from the host its requests went to.\n", other, ep.Hostname)
			return 1
		}
	}
	old, replacing := c.Servers[name]
	if replacing {
		fmt.Fprintf(stdout, "%s is already configured (%s). Replacing it gives it this URL and key, "+
			"and the sessions on it use them from their next request.\n", name, serverHost(old))
		if !*yes && !serverConfirm(stdout) {
			return exitDeclined
		}
	}

	key, err := readKey(name, *keyStdin, stdin)
	if err == nil {
		err = routerconfig.CheckKey(key)
	}
	if err == nil && strings.Contains(key, "$") {
		// config.Load expands $NAME and ${NAME} across the whole file, so a literal $
		// in a key would reach the server as something else.
		err = errors.New("the key contains $, which Cortex reads as an environment variable when it loads the config; " +
			"put the key in an environment variable the proxy has, and write key: ${NAME} by hand")
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	if ep.PlaintextRemote() {
		fmt.Fprintf(stderr, "agentop server add: warning: %s is plain http on another machine, so requests routed there "+
			"cross the network decrypted, key and prompts included.\n", ep.URL())
	}

	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"servers", name},
		Value: edit.MapValue("url", ep.URL(), "key", key), CreatePlugin: true}
	done := "Added " + name + "."
	if replacing {
		done = "Replaced " + name + "."
	}
	if len(agentsOn(c, name)) == 0 {
		done += " No agent uses it yet. Route one with\n  agentop server use " + name + " --agent claude-code"
	}
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done:      done,
		unchanged: name + " already has that URL and key; nothing to change.",
		note:      routerInactive(cfg, path),
	})
}

func serverRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server remove", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "agentop server remove: want a server name: agentop server remove <name>")
		return 2
	}
	name := pos[0]
	// First, as add does: every message below echoes name, and a name CheckName
	// accepts carries nothing a terminal would act on. Its own refusal quotes it.
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[name]; !ok {
		fmt.Fprintf(stderr, "agentop server remove: no server named %s%s\n", name, configuredServers(c))
		return 1
	}
	agents := agentsOn(c, name)
	// The only server first, since no command removes it. With agents routed to it
	// the refusal names the step that makes it carry no traffic too, rather than
	// naming that step alone and refusing again once it is taken.
	if len(c.Servers) == 1 {
		fmt.Fprintf(stderr, "agentop server remove: %s is the only server, and %s needs one. ", name, routerName)
		if len(agents) > 0 {
			fmt.Fprintf(stderr, "It is %s's server for new sessions; stop routing to it with\n", strings.Join(agents, " and "))
			for _, a := range agents {
				fmt.Fprintf(stderr, "  agentop server reset --agent %s\n", a)
			}
			fmt.Fprint(stderr, "and it changes no traffic. ")
		} else {
			fmt.Fprint(stderr, "With no agent routed to it it changes no traffic. ")
		}
		fmt.Fprintf(stderr, "To take the router out altogether, delete its entry from %s.\n", homeTilde(path))
		return 1
	}
	if len(agents) > 0 {
		var other string
		for _, s := range slices.Sorted(maps.Keys(c.Servers)) {
			if s != name {
				other = s
				break
			}
		}
		fmt.Fprintf(stderr, "%s is %s's server for new sessions. Pick another first:\n", name, strings.Join(agents, " and "))
		for _, a := range agents {
			fmt.Fprintf(stderr, "  agentop server use %s --agent %s\n", other, a)
		}
		return 1
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"servers", name}}
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done: "Removed " + name + ".",
		live: fmt.Sprintf("A session that started on it now gets an error asking for a new session; adding %s back restores it.", name),
	})
}

func serverUse(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server use", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	agent := flags.String("agent", "", "the agent whose new sessions go to the server, such as claude-code")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 || *agent == "" {
		fmt.Fprintln(stderr, "agentop server use: want a server and an agent: agentop server use <name> --agent <agent>")
		return 2
	}
	name := pos[0]
	// First, as add does: see serverRemove.
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 2
	}
	if err := routerconfig.CheckAgent(*agent); err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[name]; !ok {
		fmt.Fprintf(stderr, "agentop server use: no server named %s%s. Add it first:\n  agentop server add %s <url>\n",
			name, configuredServers(c), name)
		return 1
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"agents", *agent}, Value: edit.ScalarValue(name)}
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done:      fmt.Sprintf("New %s sessions → %s.", *agent, name),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("New %s sessions already go to %s.", *agent, name),
		note:      routerInactive(cfg, path),
	})
}

func serverReset(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server reset", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	agent := flags.String("agent", "", "the agent to stop routing, such as claude-code")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 0 || *agent == "" {
		fmt.Fprintln(stderr, "agentop server reset: want an agent: agentop server reset --agent <agent>")
		return 2
	}
	if err := routerconfig.CheckAgent(*agent); err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 1
	}
	if c.Agents[*agent] == "" {
		fmt.Fprintf(stdout, "%s is not routed; nothing to change.\n", *agent)
		return 0
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"agents", *agent}}
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done:      fmt.Sprintf("New %s sessions are no longer routed.", *agent),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("%s is not routed; nothing to change.", *agent),
	})
}

// runningSessionsStay is what use and reset mean for sessions already running,
// said once the running proxy has the change. It holds a session by its pin or by
// the request to a server its history records. One that has sent nothing since the
// proxy started has neither, and a quiet one the store has evicted (session.max_sessions,
// least recently used first, or a configured session.ttl) has no history, so either
// looks new unless the router pinned it; hence "can be".
const runningSessionsStay = "Sessions already running stay where they are, except one that has sent nothing " +
	"since the proxy last started, or that the proxy has dropped from memory (it keeps the most recently used, " +
	"100 by default), which can be treated as a new one."

// verifyRouter is the check every `agentop server` write makes of its result: the
// router entry must pass the plugin's own validation, so agentop never writes a
// config the proxy will refuse to reload.
func verifyRouter(cfg *config.Config) error {
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name == routerName {
			if _, err := routerconfig.Decode(e.Config); err != nil {
				return fmt.Errorf("%s config: %w", routerName, err)
			}
		}
	}
	return nil
}

// writeReport is what runServerWrite says about a change, by how it landed.
type writeReport struct {
	// done is the change, said once the proxy has it or will load it at start.
	done string
	// live is what the change means for sessions already running. Said only when
	// the running proxy reloaded it: that proxy's pins and history are what hold a
	// running session where it is, and a proxy that starts later has neither.
	live string
	// unchanged is said when there was nothing to write.
	unchanged string
	// note, when set, follows any of those: why the router will route nothing
	// whatever was changed (see routerInactive).
	note string
}

// runServerWrite makes one change to the router entry and reports how it landed.
func runServerWrite(stdout, stderr io.Writer, path, statsURL string, ch edit.ConfigChange, r writeReport) int {
	res, err := writePluginConfig(context.Background(), edit.ConfigWrite{
		Path: path, StatsURL: statsURL, Changes: []edit.ConfigChange{ch}, Verify: verifyRouter,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	say := func(lines ...string) {
		for _, l := range lines {
			if l != "" {
				fmt.Fprintln(stdout, l)
			}
		}
	}
	shown := homeTilde(path)
	switch res.Outcome {
	case edit.WriteUnchanged:
		say(r.unchanged, r.note)
	case edit.WriteReloaded:
		say(r.done, r.live, r.note)
	case edit.WriteNotRunning:
		next := fmt.Sprintf("Written to %s. No Cortex answered at its stats address, so this applies when the proxy next starts.", shown)
		if r.live != "" {
			// Not r.live: a proxy that starts holds no pins and no history.
			next += " A proxy that starts knows where no session is, so it treats every session it sees as a new one, " +
				"the ones running now included."
		}
		say(r.done, next, r.note)
	case edit.WriteReloadFailed:
		// WritePluginConfig has put the file back, so it does not hold the change;
		// saying it was written would send the user looking for an edit that is not
		// there. Where it could not, it returned an error saying so, handled above.
		fmt.Fprintf(stderr, "agentop server: the change was not applied: the proxy refused it and keeps its previous configuration, "+
			"so %s was put back as it was:\n  %s\n", shown, res.ReloadError)
		return 1
	case edit.WriteStatusUnreachable:
		// Not a refusal: nothing said no. The proxy went away, and whether it took
		// the change before it did is unknown, so the file was put back to what it
		// last ran, which is what it starts from if it was stopped.
		fmt.Fprintf(stderr, "agentop server: the change was not confirmed: the proxy stopped answering at %s before it reported the reload, "+
			"so %s was put back as it was; check the proxy is running and try again:\n  %s\n", statsURL, shown, res.ReloadError)
		return 1
	case edit.WriteReloadTimedOut:
		fmt.Fprintf(stderr, "agentop server: wrote %s, but the proxy reported no reload within %s; check %s/reload/status\n",
			shown, edit.LocalPollDeadline, statsURL)
		return 1
	}
	return 0
}

// configuredServers is "; configured: a, b" for an error naming a missing server,
// or "" when there are none.
func configuredServers(c routerconfig.Config) string {
	if len(c.Servers) == 0 {
		return ""
	}
	return "; configured: " + strings.Join(slices.Sorted(maps.Keys(c.Servers)), ", ")
}
