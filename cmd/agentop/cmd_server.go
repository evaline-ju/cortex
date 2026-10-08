package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// routerName is the inference-router plugin's registered name: the outbound entry
// every `agentop server` form reads and writes.
const routerName = "inference-router"

const serverUsage = `agentop server — choose the inference server each coding agent's new sessions use

Usage:
  agentop server [--config PATH]
  agentop server add <name> <url> [--key-stdin] [--yes] [--config PATH]
  agentop server remove <name> [--config PATH]
  agentop server use <name> --agent <agent> [--config PATH]
  agentop server reset --agent <agent> [--config PATH]

With no action, lists the servers and the agents routed to each, then checks
that Claude Code's settings let it be routed.

A server is a gateway, such as LiteLLM, with its own URL and key. Routing is per
agent and opt-in: until "use" gives an agent a server, its traffic goes wherever
the agent sends it. A session stays on the server it started on, so a change
applies to new sessions only.

"add" reads the server's API key at a prompt that does not echo, or from stdin
with --key-stdin; it is never an argument. Adding a name that exists asks before
replacing it (--yes skips the question), which is also how a key is rotated.

Every change is written to ~/.cortex/config.yaml, or to --config PATH, and returns
once the proxy has reloaded it. Nothing restarts. Flags go after the action.
`

// runServer handles the `server` subcommand. Returns the process exit code.
func runServer(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	switch action {
	case "":
		return serverList(args, stdout, stderr)
	case "help":
		fmt.Fprint(stdout, serverUsage)
		return 0
	case "add":
		return serverAdd(args, stdin, stdout, stderr)
	case "remove":
		return serverRemove(args, stdout, stderr)
	case "use":
		return serverUse(args, stdout, stderr)
	case "reset":
		return serverReset(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agentop server: unknown action %q\n\n", action)
	fmt.Fprint(stderr, serverUsage)
	return 2
}

// serverFlags parses flags and positional arguments in any order, so
// `agentop server use glm --agent claude-code` reads as written; the flag package
// alone stops at the first positional argument and would leave --agent unparsed.
// ok is false when the command should stop, with code its exit status: 0 for a help
// request, which goes to stdout, and 2 for a bad flag.
func serverFlags(flags *flag.FlagSet, args []string, stdout, stderr io.Writer) (pos []string, code int, ok bool) {
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(stdout, serverUsage)
				return nil, 0, false
			}
			fmt.Fprint(stderr, serverUsage)
			return nil, 2, false
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return pos, 0, true
		}
		// Parse consumed a "--": everything after it is positional.
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(pos, rest...), 0, true
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// serverTarget is the config `agentop server` reads and writes, and the stats URL
// whose /reload/status confirms a write — "" when no proxy answers there.
//
// Not localEditTargets, which it otherwise mirrors: that returns no path at all when
// the proxy is down, and a write with no proxy running is still worth making, since
// it applies at the next start. And --config names a file localEditTargets cannot.
func serverTarget(configPath string) (cfg *config.Config, path, statsURL string, err error) {
	if configPath == "" {
		cfg, path, err = localCortexConfig()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", "", errors.New("no Cortex config at ~/.cortex/config.yaml; install Cortex with `agentop setup`, or name a config with --config")
		}
	} else {
		path = configPath
		cfg, err = config.Load(path)
	}
	if err != nil {
		return nil, "", "", err
	}
	if statsURL = dialURL(cfg.Stats.StatsAddress); statsURL != "" && !localStatsUp(statsURL) {
		statsURL = ""
	}
	return cfg, path, statsURL, nil
}

// readRouter is the router entry's config, decoded without the plugin's validation
// so a listing still works on a config the proxy would refuse. present is false
// when the outbound chain has no router entry.
func readRouter(cfg *config.Config) (c routerconfig.Config, present bool, err error) {
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name != routerName {
			continue
		}
		if len(e.Config) > 0 {
			if err := json.Unmarshal(e.Config, &c); err != nil {
				return routerconfig.Config{}, true, fmt.Errorf("the %s entry's config: %w", routerName, err)
			}
		}
		return c, true, nil
	}
	return routerconfig.Config{}, false, nil
}

// agentsOn lists the agents routed to server, sorted.
func agentsOn(c routerconfig.Config, server string) []string {
	var out []string
	for agent, s := range c.Agents {
		if s == server {
			out = append(out, agent)
		}
	}
	slices.Sort(out)
	return out
}

func serverList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "agentop server: unexpected argument %q; the action comes first: agentop server <action> [flags]\n", pos[0])
		return 2
	}
	cfg, _, _, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	if len(c.Servers) == 0 {
		fmt.Fprintln(stdout, "No inference servers yet. Add one:")
		fmt.Fprintln(stdout, "  agentop server add <name> <url>")
		return 0
	}

	// Every row has all four cells, the agents one empty when no agent is routed
	// there: tabwriter aligns a column only across consecutive rows that have it, so
	// a short row would restart the alignment below it. The padding that leaves after
	// the mapping of such a row is trimmed.
	var table strings.Builder
	tw := tabwriter.NewWriter(&table, 0, 0, 3, ' ', 0)
	for _, name := range slices.Sorted(maps.Keys(c.Servers)) {
		s := c.Servers[name]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, serverHost(s), mappingText(s), strings.Join(agentsOn(c, name), ", "))
	}
	tw.Flush()
	for line := range strings.Lines(table.String()) {
		fmt.Fprintln(stdout, strings.TrimRight(line, " \n"))
	}
	fmt.Fprintln(stdout)

	for _, ck := range claudeCodeChecksAtHome(c) {
		printCheck(stdout, ck.ok, ck.text)
	}
	return 0
}

// serverHost is how a server's URL is listed: its host, or the whole URL for plain
// http, so a server whose traffic crosses the network unencrypted says so.
//
// A URL the router refuses is listed by its host alone, never as written: the
// likeliest reason it is refused is a key pasted in as user info.
func serverHost(s routerconfig.Server) string {
	ep, err := routerconfig.ParseURL(s.URL)
	if err != nil {
		if u, perr := url.Parse(s.URL); perr == nil && u.Host != "" {
			return u.Host + " (not a valid URL)"
		}
		return "(not a valid URL)"
	}
	if ep.Scheme == "http" {
		return ep.URL()
	}
	return ep.Host
}

// mappingText is a server's model mapping as one cell.
func mappingText(s routerconfig.Server) string {
	switch {
	case s.Opus == "" && s.Sonnet == "" && s.Haiku == "":
		return "uses Claude Code's names"
	case s.Opus == s.Sonnet && s.Sonnet == s.Haiku:
		return "all → " + s.Opus
	}
	or := func(m string) string {
		if m == "" {
			return "—"
		}
		return m
	}
	return fmt.Sprintf("opus → %s · sonnet → %s · haiku → %s", or(s.Opus), or(s.Sonnet), or(s.Haiku))
}

// settingsCheck is one line of `agentop server`'s check of Claude Code's settings.
type settingsCheck struct {
	ok   bool
	text string
}

// claudeCodeModelVars are the env settings that make Claude Code ask for a model by
// a name other than Claude's own; the top-level "model" key, which /model writes,
// is the other way. The router maps Claude's names, by family, to a server's; a
// request already carrying a server's name has no family left to map, and Claude
// Code shapes its requests for the model it believes it is using.
var claudeCodeModelVars = []string{
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// claudeCodeChecksAtHome is claudeCodeChecks on ~/.claude/settings.json. With no
// home directory it says so rather than reading .claude/settings.json from wherever
// agentop was started, which is what joining an empty home would do.
func claudeCodeChecksAtHome(c routerconfig.Config) []settingsCheck {
	home, err := os.UserHomeDir()
	if err == nil && home == "" {
		err = errors.New("it is empty")
	}
	if err != nil {
		return []settingsCheck{{false, fmt.Sprintf("Claude Code's settings are not checked: cannot determine your home directory (%v)", err)}}
	}
	return claudeCodeChecks(filepath.Join(home, settingsRel), c)
}

// claudeCodeChecks checks the settings file at settingsPath: that ANTHROPIC_BASE_URL
// is one of c's servers, and that neither the "model" key nor any model variable
// names a non-Claude model. It reads that one file, so it cannot see a file passed
// with `claude --settings`, nor a variable set in the shell.
func claudeCodeChecks(settingsPath string, c routerconfig.Config) []settingsCheck {
	shown := homeTilde(settingsPath)
	doc, err := readSettings(settingsPath)
	if err != nil {
		return []settingsCheck{{false, fmt.Sprintf("cannot read %s: %v", shown, err)}}
	}
	env := envStrings(doc)
	model, _ := doc["model"].(string)
	return append([]settingsCheck{baseURLCheck(env["ANTHROPIC_BASE_URL"], shown, c)}, modelChecks(model, env, shown)...)
}

func baseURLCheck(raw, shown string, c routerconfig.Config) settingsCheck {
	if raw == "" {
		return settingsCheck{false, fmt.Sprintf("%s sets no ANTHROPIC_BASE_URL, so Claude Code talks to Anthropic and nothing is routed. Point it at one of the servers above.", shown)}
	}
	// raw is never quoted: user info, a query and a fragment are where a key gets
	// pasted, and a URL that does not parse cannot have them taken out.
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return settingsCheck{false, fmt.Sprintf("ANTHROPIC_BASE_URL in %s is not a URL with a host, so nothing is routed. Point it at one of the servers above.", shown)}
	}
	host := routerconfig.Hostname(u.Host)
	for _, name := range slices.Sorted(maps.Keys(c.Servers)) {
		if ep, err := routerconfig.ParseURL(c.Servers[name].URL); err == nil && ep.Hostname == host {
			return settingsCheck{true, fmt.Sprintf("Claude Code points at %s (%s)", name, shown)}
		}
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return settingsCheck{false, fmt.Sprintf("Claude Code points at %s, which is not one of these servers, so nothing is routed (ANTHROPIC_BASE_URL in %s). Point it at one of the servers above.", u, shown)}
}

// modelChecks checks model, the settings' top-level "model" key, and env's model
// variables.
func modelChecks(model string, env map[string]string, shown string) []settingsCheck {
	var out []settingsCheck
	check := func(m, where string) {
		if m != "" && !isClaudeModel(m) {
			out = append(out, settingsCheck{false, fmt.Sprintf(
				"Claude Code asks for %s instead of Claude's models (%s in %s). Cortex can't map that back: remove the line, and route Claude Code only to servers that serve Claude's names.",
				m, where, shown)})
		}
	}
	check(model, `"model"`)
	for _, v := range claudeCodeModelVars {
		check(env[v], v)
	}
	if len(out) == 0 {
		out = append(out, settingsCheck{true, fmt.Sprintf("Claude Code asks for Claude's own model names (%s)", shown)})
	}
	return out
}

// isClaudeModel reports whether m is one of Claude Code's own aliases — with or
// without a context suffix such as [1m] — or a name containing "claude".
func isClaudeModel(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.IndexByte(m, '['); i > 0 {
		m = m[:i]
	}
	switch m {
	case "default", "best", "fable", "opus", "opusplan", "sonnet", "haiku":
		return true
	}
	return strings.Contains(m, "claude")
}

// printCheck writes one check as "  ✓ text", wrapped at 80 columns with the
// continuation lines under the text.
func printCheck(w io.Writer, ok bool, text string) {
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	line, words := "  "+mark, 0
	for _, word := range strings.Fields(text) {
		if words > 0 && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > 80 {
			fmt.Fprintln(w, line)
			line = "   "
		}
		line += " " + word
		words++
	}
	fmt.Fprintln(w, line)
}

// homeTilde shows path with the home directory as ~, the way the docs name it.
func homeTilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return path
}
