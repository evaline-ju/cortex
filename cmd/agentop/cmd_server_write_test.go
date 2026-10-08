package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
)

// fakeStats is a stats server whose /reload/status reports a reload at every poll,
// or, with failAfter > 0, a failed one from that poll on. It counts the polls.
type fakeStats struct {
	srv       *httptest.Server
	polls     atomic.Int32
	failAfter int32
}

func newFakeStats(t *testing.T, failAfter int32) *fakeStats {
	t.Helper()
	f := &fakeStats{failAfter: failAfter}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reload/status" {
			http.NotFound(w, r)
			return
		}
		n := f.polls.Add(1)
		st := map[string]any{"last_success": time.Now(), "reloads_ok": 1, "reloads_failed": 0}
		if f.failAfter > 0 {
			st["last_success"] = time.Now().Add(-time.Hour)
			if n >= f.failAfter {
				st["reloads_failed"], st["last_error"] = 1, `configure "inference-router": refused`
			}
		}
		_ = json.NewEncoder(w).Encode(st)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// addr is the stats address a config names for this server.
func (f *fakeStats) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func readConfig(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stubKeyPrompt stands in for the terminal: it returns key and records the server
// name it was asked for. A test that must not prompt passes key "" and checks the
// name stayed empty.
func stubKeyPrompt(t *testing.T, key string) *string {
	t.Helper()
	var asked string
	prev := readServerKey
	readServerKey = func(name string) (string, error) { asked = name; return key, nil }
	t.Cleanup(func() { readServerKey = prev })
	return &asked
}

// stubConfirm answers the replace question with answer and counts the asks.
func stubConfirm(t *testing.T, answer bool) *int {
	t.Helper()
	var asks int
	prev := serverConfirm
	serverConfirm = func(io.Writer) bool { asks++; return answer }
	t.Cleanup(func() { serverConfirm = prev })
	return &asks
}

func TestServerAdd_CreatesTheRouterAtTheEndOfTheChain(t *testing.T) {
	stats := newFakeStats(t, 0)
	path := serverEnv(t, stats.addr(), "")
	code, out, errOut := runServerCmd(t, "sk-ete\n", "add", "ete", "https://ete.example.com/", "--key-stdin", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Added ete. No agent uses it yet") || !strings.Contains(out, "agentop server use ete --agent claude-code") {
		t.Errorf("stdout:\n%s", out)
	}
	want := `      - name: tool-prune
        config:
          remove: []
      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete
`
	if got := readConfig(t, path); !strings.HasSuffix(got, want) {
		t.Errorf("config does not end with the new entry:\n%s", got)
	}
}

func TestServerAdd_ReadsTheKeyAtTheHiddenPrompt(t *testing.T) {
	asked := stubKeyPrompt(t, "sk-typed")
	path := serverEnv(t, newFakeStats(t, 0).addr(), "")
	if code, _, errOut := runServerCmd(t, "", "add", "ete", "https://ete.example.com", "--config", path); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if *asked != "ete" {
		t.Errorf("prompted for %q, want ete", *asked)
	}
	if !strings.Contains(readConfig(t, path), "key: sk-typed") {
		t.Error("the typed key was not written")
	}
}

func TestServerAdd_ReplacingAsksFirst(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	before := readConfig(t, path)

	asks := stubConfirm(t, false)
	asked := stubKeyPrompt(t, "sk-rotated")
	code, out, _ := runServerCmd(t, "", "add", "ete", "https://ete.example.com", "--config", path)
	if code != exitDeclined || *asks != 1 || !strings.Contains(out, "ete is already configured (ete.example.com)") {
		t.Errorf("declined: exit %d, %d asks, stdout:\n%s", code, *asks, out)
	}
	if *asked != "" || readConfig(t, path) != before {
		t.Error("a declined replace read a key or wrote the file")
	}

	code, out, errOut := runServerCmd(t, "", "add", "ete", "https://ete.example.com", "--yes", "--config", path)
	if code != 0 || *asks != 1 || !strings.Contains(out, "Replaced ete.") {
		t.Errorf("--yes: exit %d, %d asks, stdout:\n%s%s", code, *asks, out, errOut)
	}
	if got := readConfig(t, path); !strings.Contains(got, "key: sk-rotated") || strings.Contains(got, "key: sk-ete") {
		t.Errorf("the key was not rotated:\n%s", got)
	}
}

func TestServerAdd_AnUnchangedServerIsNotRewritten(t *testing.T) {
	stubConfirm(t, true)
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, out, _ := runServerCmd(t, "sk-ete", "add", "ete", "https://ete.example.com:443", "--key-stdin", "--yes", "--config", path)
	if code != 0 || !strings.Contains(out, "ete already has that URL and key; nothing to change.") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

func TestServerAdd_RefusesASecondServerOnOneHost(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, _, errOut := runServerCmd(t, "sk-x", "add", "east", "http://ETE.example.com:4000", "--key-stdin", "--config", path)
	if code != 1 || !strings.Contains(errOut, "ete is already on ete.example.com") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServerAdd_RefusesABadNameOrURLBeforeAskingForAKey(t *testing.T) {
	asked := stubKeyPrompt(t, "sk")
	path := serverEnv(t, newFakeStats(t, 0).addr(), "")
	for _, args := range [][]string{
		{"add", "ETE", "https://ete.example.com"},
		{"add", "ete", "https://ete.example.com/v1"},
		{"add", "ete", "https://ete.example.com:99999"},
		{"add", "ete"},
	} {
		if code, _, _ := runServerCmd(t, "", append(args, "--config", path)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if *asked != "" {
		t.Error("asked for a key for an invalid server")
	}
}

func TestServerAdd_RefusesAKeyTheConfigLoaderWouldExpand(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), "")
	before := readConfig(t, path)
	code, _, errOut := runServerCmd(t, "sk-$HOME", "add", "ete", "https://ete.example.com", "--key-stdin", "--config", path)
	if code != 1 || !strings.Contains(errOut, "the key contains $") || strings.Contains(errOut, "sk-$HOME") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
	if readConfig(t, path) != before {
		t.Error("the config was written")
	}
}

func TestServerAdd_WarnsOnPlainHTTPToAnotherMachine(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), "")
	_, _, errOut := runServerCmd(t, "sk", "add", "lan", "http://10.0.0.5:4000", "--key-stdin", "--config", path)
	if !strings.Contains(errOut, "plain http on another machine") {
		t.Errorf("stderr:\n%s", errOut)
	}
}

// The model flags arrive with model mapping; until then an unknown flag is the
// honest answer.
func TestServerAdd_DoesNotOfferModelFlagsYet(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), "")
	code, _, errOut := runServerCmd(t, "sk", "add", "glm", "https://glm.example.com", "--opus", "glm-5.3", "--key-stdin", "--config", path)
	if code != 2 || !strings.Contains(errOut, "flag provided but not defined: -opus") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServerAdd_WithNoProxyRunningWritesForTheNextStart(t *testing.T) {
	path := serverEnv(t, closedAddr(t), "")
	code, out, errOut := runServerCmd(t, "sk-ete", "add", "ete", "https://ete.example.com", "--key-stdin", "--config", path)
	if code != 0 || !strings.Contains(out, "applies when the proxy next starts") {
		t.Errorf("exit %d, stdout:\n%s%s", code, out, errOut)
	}
	if !strings.Contains(readConfig(t, path), "key: sk-ete") {
		t.Error("the config was not written")
	}
}

func TestServerAdd_ReportsAReloadTheProxyRefused(t *testing.T) {
	// Poll 1 is serverTarget's probe and poll 2 the baseline; the reload fails after.
	path := serverEnv(t, newFakeStats(t, 3).addr(), "")
	before := readConfig(t, path)
	code, _, errOut := runServerCmd(t, "sk-ete", "add", "ete", "https://ete.example.com", "--key-stdin", "--config", path)
	if code != 1 || !strings.Contains(errOut, "the proxy refused it and keeps its previous configuration") ||
		!strings.Contains(errOut, `configure "inference-router": refused`) {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
	// The refused change was taken back out, so the message must not claim the
	// file holds it, and the file must not: a refused config left on disk is what
	// the proxy would next start from.
	if strings.Contains(errOut, "wrote") || !strings.Contains(errOut, "was put back as it was") {
		t.Errorf("want the file reported as put back, not as written:\n%s", errOut)
	}
	if got := readConfig(t, path); strings.Contains(got, "key: sk-ete") || got != before {
		t.Errorf("the refused change is still in the config:\n%s", got)
	}
}

func TestServerRemove_RefusesAServerAnAgentIsRoutedTo(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	before := readConfig(t, path)
	code, _, errOut := runServerCmd(t, "", "remove", "glm", "--config", path)
	if code != 1 || !strings.Contains(errOut, "glm is claude-code's server for new sessions. Pick another first:\n  agentop server use ete --agent claude-code") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
	if readConfig(t, path) != before {
		t.Error("the config was written")
	}
}

func TestServerRemove_RemovesAServerNoAgentUses(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, out, errOut := runServerCmd(t, "", "remove", "ete", "--config", path)
	if code != 0 || !strings.Contains(out, "Removed ete.") {
		t.Fatalf("exit %d, stdout:\n%s%s", code, out, errOut)
	}
	got := readConfig(t, path)
	if strings.Contains(got, "ete.example.com") || !strings.Contains(got, "glm.example.com") {
		t.Errorf("want only ete gone:\n%s", got)
	}
}

func TestServerRemove_RefusesTheOnlyServer(t *testing.T) {
	only := strings.Replace(routerBlock, `            glm:
              url: https://glm.example.com:8443
              key: sk-glm
          agents:
            claude-code: glm
`, "", 1)
	path := serverEnv(t, newFakeStats(t, 0).addr(), only)
	code, _, errOut := runServerCmd(t, "", "remove", "ete", "--config", path)
	if code != 1 || !strings.Contains(errOut, "ete is the only server") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServerRemove_NamesTheServersThatExist(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, _, errOut := runServerCmd(t, "", "remove", "east", "--config", path)
	if code != 1 || !strings.Contains(errOut, "no server named east; configured: ete, glm") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServerUse_RoutesTheAgentsNewSessions(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, out, errOut := runServerCmd(t, "", "use", "ete", "--agent", "opencode", "--config", path)
	// A running session stays because the proxy holds its pin or the request its
	// history records, both of which it has only for sessions seen since it started.
	if code != 0 || out != "New opencode sessions → ete.\nSessions already running stay where they are, except one that has "+
		"sent nothing since the proxy last started, which is treated as a new one.\n" {
		t.Fatalf("exit %d, stdout:\n%s%s", code, out, errOut)
	}
	if !strings.Contains(readConfig(t, path), "            claude-code: glm\n            opencode: ete\n") {
		t.Errorf("config:\n%s", readConfig(t, path))
	}
}

func TestServerUse_AnAgentAlreadyThereIsNotRewritten(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, out, _ := runServerCmd(t, "", "use", "glm", "--agent", "claude-code", "--config", path)
	if code != 0 || !strings.Contains(out, "New claude-code sessions already go to glm.") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

func TestServerUse_RefusesAServerThatIsNotConfigured(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, _, errOut := runServerCmd(t, "", "use", "east", "--agent", "claude-code", "--config", path)
	if code != 1 || !strings.Contains(errOut, "no server named east; configured: ete, glm. Add it first:\n  agentop server add east <url>") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServerUse_RefusesABadAgentName(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	for _, agent := range []string{"Claude Code", "unknown", ""} {
		if code, _, _ := runServerCmd(t, "", "use", "ete", "--agent", agent, "--config", path); code != 2 {
			t.Errorf("--agent %q: exit %d, want 2", agent, code)
		}
	}
}

// use and remove check the server name first, as add does: a name no server can
// have is a usage error (exit 2) before any config is read, and the refusal quotes
// it, so a control sequence typed or pasted into the argument never reaches the
// terminal raw. Every later message names a server that passed the check.
func TestServerUseAndRemove_RefuseABadServerNameFirst(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	before := readConfig(t, path)
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")
	for _, args := range [][]string{
		{"use", "ETE", "--agent", "claude-code", "--config", path},
		{"use", "a\x1b[2Jb", "--agent", "claude-code", "--config", path},
		{"use", "a\x1b[2Jb", "--agent", "claude-code", "--config", missing},
		{"remove", "ETE", "--config", path},
		{"remove", "a\x1b[2Jb", "--config", path},
		{"remove", "a\x1b[2Jb", "--config", missing},
	} {
		code, out, errOut := runServerCmd(t, "", args...)
		if code != 2 || !strings.Contains(errOut, "is not a server name") {
			t.Errorf("%q: exit %d, want 2 and the name refused; stderr:\n%s", args, code, errOut)
		}
		if strings.ContainsRune(out+errOut, '\x1b') {
			t.Errorf("%q: a raw ESC reached the output:\n%q", args, out+errOut)
		}
	}
	if readConfig(t, path) != before {
		t.Error("the config was written")
	}
}

func TestServerReset_StopsRoutingTheAgent(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	code, out, errOut := runServerCmd(t, "", "reset", "--agent", "claude-code", "--config", path)
	if code != 0 || !strings.Contains(out, "New claude-code sessions are no longer routed.") {
		t.Fatalf("exit %d, stdout:\n%s%s", code, out, errOut)
	}
	if got := readConfig(t, path); strings.Contains(got, "agents:") {
		t.Errorf("want the emptied agents map gone:\n%s", got)
	}
}

func TestServerReset_AnAgentThatIsNotRoutedChangesNothing(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	before := readConfig(t, path)
	code, out, _ := runServerCmd(t, "", "reset", "--agent", "opencode", "--config", path)
	if code != 0 || !strings.Contains(out, "opencode is not routed; nothing to change.") || readConfig(t, path) != before {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

// Every write is checked against the plugin's own rules before it lands, so a
// config the proxy would refuse is never written — here, one a hand edit already
// broke with a model the router does not accept yet.
func TestServerWrites_NeverWriteAConfigThePluginRefuses(t *testing.T) {
	broken := strings.Replace(routerBlock, "              key: sk-ete\n", "              key: sk-ete\n              opus: glm-5.3\n", 1)
	path := serverEnv(t, newFakeStats(t, 0).addr(), broken)
	before := readConfig(t, path)
	code, _, errOut := runServerCmd(t, "", "use", "ete", "--agent", "opencode", "--config", path)
	if code != 1 || !strings.Contains(errOut, "model mapping needs chained body writers") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
	if readConfig(t, path) != before {
		t.Error("the config was written")
	}
}

// Replacing a server whose stored URL took the key for its host (see
// TestServer_AKeyTakenForTheHostIsNeverShown) names it without that URL.
func TestServerAdd_ReplacingShowsNothingOfARefusedStoredURL(t *testing.T) {
	stubConfirm(t, true)
	for _, raw := range []string{
		"https://sk-SECRET/x@h.example.com",
		"https://sk-SECRET?x@h.example.com",
		"https://sk-SECRET#x@h.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			path := serverEnv(t, newFakeStats(t, 0).addr(), strings.Replace(routerBlock, "https://ete.example.com", raw, 1))
			code, out, errOut := runServerCmd(t, "sk-new", "add", "ete", "https://ete.example.com", "--key-stdin", "--yes", "--config", path)
			if code != 0 {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			if all := strings.ToLower(out + errOut); strings.Contains(all, "secret") {
				t.Errorf("prints the stored key:\n%s%s", out, errOut)
			}
			if !strings.Contains(out, "ete is already configured (not a valid URL).") {
				t.Errorf("stdout:\n%s", out)
			}
		})
	}
}

// stubWrite stands in for edit.WritePluginConfig with a fixed answer.
func stubWrite(t *testing.T, res edit.WriteResult, err error) {
	t.Helper()
	prev := writePluginConfig
	writePluginConfig = func(context.Context, edit.ConfigWrite) (edit.WriteResult, error) { return res, err }
	t.Cleanup(func() { writePluginConfig = prev })
}

// A proxy that stopped answering refused nothing. The command says what happened —
// it stopped answering, and the file was put back — and exits 1, rather than
// reporting a refusal the proxy never made.
func TestServerWrites_TellAProxyThatStoppedAnsweringFromARefusal(t *testing.T) {
	path := serverEnv(t, newFakeStats(t, 0).addr(), routerBlock)
	stubWrite(t, edit.WriteResult{Outcome: edit.WriteStatusUnreachable, RolledBack: true,
		ReloadError: "reload status endpoint unreachable (is the local proxy still running?)"}, nil)
	code, out, errOut := runServerCmd(t, "", "use", "ete", "--agent", "opencode", "--config", path)
	if code != 1 {
		t.Errorf("exit %d, want 1; stdout:\n%s", code, out)
	}
	if strings.Contains(errOut, "refused") || !strings.Contains(errOut, "stopped answering") ||
		!strings.Contains(errOut, "was put back as it was") || !strings.Contains(errOut, "is the local proxy still running?") {
		t.Errorf("stderr:\n%s", errOut)
	}
}

// --key-stdin reads the key from stdin as typed, so on a terminal every character
// of the key would echo. The command refuses before asking anything, and reads and
// writes nothing.
func TestServerAdd_RefusesKeyStdinFromATerminal(t *testing.T) {
	prev := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = prev })
	path := serverEnv(t, newFakeStats(t, 0).addr(), "")
	before := readConfig(t, path)

	code, _, errOut := runServerCmd(t, "sk-typed\n", "add", "ete", "https://ete.example.com", "--key-stdin", "--config", path)
	if code != 1 || !strings.Contains(errOut, "stdin is a terminal") || !strings.Contains(errOut, "Pipe the key in, or drop --key-stdin") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
	if readConfig(t, path) != before {
		t.Error("the config was written")
	}
}

// observeRouter is routerBlock with the router's entry under on_error: policy.
func observeRouter(policy string) string {
	return strings.Replace(routerBlock, "      - name: inference-router\n",
		"      - name: inference-router\n        on_error: "+policy+"\n", 1)
}

// Under on_error: observe the router moves nothing and only records would_route,
// and under off it does not run: either way nothing is routed, which use, add and
// the listing must say rather than report a route that will not happen.
func TestServer_SaysWhenTheRouterCannotRoute(t *testing.T) {
	for policy, want := range map[string]string{
		"observe": "on_error: observe, so nothing is routed",
		"off":     "on_error: off, so it does not run and nothing is routed",
	} {
		t.Run(policy, func(t *testing.T) {
			path := serverEnv(t, newFakeStats(t, 0).addr(), observeRouter(policy))
			_, out, _ := runServerCmd(t, "", "--config", path)
			if !strings.Contains(flat(out), "✗ The "+routerName+" entry runs under "+want) {
				t.Errorf("listing:\n%s", out)
			}
			_, out, errOut := runServerCmd(t, "", "use", "ete", "--agent", "opencode", "--config", path)
			if !strings.Contains(flat(out), want) {
				t.Errorf("use:\n%s%s", out, errOut)
			}
			_, out, errOut = runServerCmd(t, "sk-lan", "add", "lan", "http://127.0.0.1:4000", "--key-stdin", "--config", path)
			if !strings.Contains(flat(out), want) {
				t.Errorf("add:\n%s%s", out, errOut)
			}
		})
	}
	path := serverEnv(t, newFakeStats(t, 0).addr(), observeRouter("enforce"))
	if _, out, _ := runServerCmd(t, "", "use", "ete", "--agent", "opencode", "--config", path); strings.Contains(out, "on_error") {
		t.Errorf("enforce named on_error:\n%s", out)
	}
}

// The only server, with an agent routed to it, needs two steps to remove, and the
// refusal names both at once rather than one and then, after it, the other.
func TestServerRemove_TheOnlyServerWithAnAgentNamesBothSteps(t *testing.T) {
	only := strings.Replace(routerBlock, `            ete:
              url: https://ete.example.com
              key: sk-ete
`, "", 1)
	path := serverEnv(t, newFakeStats(t, 0).addr(), only)
	code, _, errOut := runServerCmd(t, "", "remove", "glm", "--config", path)
	for _, want := range []string{"glm is the only server", "agentop server reset --agent claude-code", "delete its entry"} {
		if !strings.Contains(flat(errOut), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// A proxy that is not running holds no pins and, once started, no history, so it
// routes every session it then sees as a new one. Written for the next start, the
// change must not promise that running sessions stay where they are.
func TestServerWrites_ForTheNextStartPromiseNoRunningSessionStays(t *testing.T) {
	for _, args := range [][]string{
		{"use", "ete", "--agent", "opencode"},
		{"reset", "--agent", "claude-code"},
		{"remove", "ete"},
	} {
		t.Run(args[0], func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), routerBlock)
			code, out, errOut := runServerCmd(t, "", append(args, "--config", path)...)
			if code != 0 || !strings.Contains(out, "applies when the proxy next starts") {
				t.Fatalf("exit %d, stdout:\n%s%s", code, out, errOut)
			}
			if strings.Contains(out, "stay where they are") || strings.Contains(out, "now gets an error") {
				t.Errorf("promises what a restart does not keep:\n%s", out)
			}
			if !strings.Contains(flat(out), "treats every session it sees as a new one, the ones running now included") {
				t.Errorf("does not say what the next start does:\n%s", out)
			}
		})
	}
}
