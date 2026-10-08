package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// routerBlock is the inference-router entry agentop writes, with two servers and
// claude-code routed to glm.
const routerBlock = `      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete
            glm:
              url: https://glm.example.com:8443
              key: sk-glm
          agents:
            claude-code: glm
`

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// serverEnv gives a test a scratch HOME, so nothing reads or writes the real
// ~/.claude or ~/.cortex, and a config in it whose stats address is statsAddr and
// whose outbound chain ends with router (none when ""). It returns the config's
// path, which sits directly in that HOME: filepath.Dir of it is the scratch home.
func serverEnv(t *testing.T, statsAddr, router string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := fmt.Sprintf(`mode: proxy-sidecar
stats:
  address: %s
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: []
%s`, statsAddr, router)
	path := filepath.Join(home, "cortex.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeClaudeSettings puts env in home's .claude/settings.json. home is a
// parameter, not $HOME, so a test that calls this before serverEnv writes into a
// directory it named rather than over the developer's real settings.
func writeClaudeSettings(t *testing.T, home string, env map[string]string) {
	t.Helper()
	writeClaudeSettingsDoc(t, home, map[string]any{"env": env})
}

// writeClaudeSettingsDoc puts doc, a whole settings document, in home's
// .claude/settings.json.
func writeClaudeSettingsDoc(t *testing.T, home string, doc map[string]any) {
	t.Helper()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runServerCmd(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runServer(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// flat joins s's words with single spaces, so a phrase can be found across the
// line wrapping printCheck applies.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestServer_SaysWhenThereAreNoServers(t *testing.T) {
	path := serverEnv(t, closedAddr(t), "")
	code, out, _ := runServerCmd(t, "", "--config", path)
	if code != 0 || !strings.Contains(out, "No inference servers yet") || !strings.Contains(out, "agentop server add <name> <url>") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

func TestServer_ListsEachServerWithItsHostMappingAndAgents(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(out, "\n")
	if got := flat(lines[0]); got != "ete ete.example.com uses Claude Code's names" {
		t.Errorf("line 1 = %q", got)
	}
	if got := flat(lines[1]); got != "glm glm.example.com:8443 uses Claude Code's names claude-code" {
		t.Errorf("line 2 = %q", got)
	}
	if strings.Index(lines[0], "uses") != strings.Index(lines[1], "uses") {
		t.Errorf("columns do not line up:\n%s\n%s", lines[0], lines[1])
	}
}

func TestServer_ListsPlainHTTPServersByTheirWholeURL(t *testing.T) {
	path := serverEnv(t, closedAddr(t), strings.Replace(routerBlock, "https://ete.example.com", "http://localhost:4000", 1))
	_, out, _ := runServerCmd(t, "", "--config", path)
	if !strings.Contains(out, "http://localhost:4000") {
		t.Errorf("want the plain-http server listed with its scheme:\n%s", out)
	}
}

func TestServer_ChecksWhereClaudeCodePoints(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, want string
	}{
		{"a server", "https://ete.example.com", "✓ Claude Code points at ete (~/.claude/settings.json)"},
		{"a server, other port", "https://GLM.example.com:9999/", "✓ Claude Code points at glm (~/.claude/settings.json)"},
		{"elsewhere", "https://api.anthropic.com", "✗ Claude Code points at https://api.anthropic.com, which is not one of these servers, so nothing is routed (ANTHROPIC_BASE_URL in ~/.claude/settings.json). Point it at one of the servers above."},
		{"nowhere", "", "✗ ~/.claude/settings.json sets no ANTHROPIC_BASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), routerBlock)
			env := map[string]string{}
			if tc.baseURL != "" {
				env["ANTHROPIC_BASE_URL"] = tc.baseURL
			}
			writeClaudeSettings(t, filepath.Dir(path), env)
			_, out, _ := runServerCmd(t, "", "--config", path)
			if !strings.Contains(flat(out), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out)
			}
		})
	}
}

func TestServer_FlagsAModelVariableThatNamesAnotherModel(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	writeClaudeSettings(t, filepath.Dir(path), map[string]string{
		"ANTHROPIC_BASE_URL":           "https://ete.example.com",
		"ANTHROPIC_MODEL":              "glm-5.3",
		"ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5-5",
		"ANTHROPIC_SMALL_FAST_MODEL":   "haiku",
		"CLAUDE_CODE_SUBAGENT_MODEL":   "opus[1m]",
	})
	_, out, _ := runServerCmd(t, "", "--config", path)
	got := flat(out)
	if want := "✗ Claude Code asks for glm-5.3 instead of Claude's models (ANTHROPIC_MODEL in ~/.claude/settings.json). " +
		"Cortex can't map that back: remove the line, and give the server its models with agentop server add --opus, --sonnet and --haiku instead."; !strings.Contains(got, want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
	if strings.Count(got, "✗") != 1 {
		t.Errorf("want exactly one failed check — Claude's own names and aliases pass:\n%s", out)
	}
}

func TestServer_PassesSettingsThatNameOnlyClaudesModels(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	writeClaudeSettings(t, filepath.Dir(path), map[string]string{"ANTHROPIC_BASE_URL": "https://ete.example.com"})
	_, out, _ := runServerCmd(t, "", "--config", path)
	if want := "✓ Claude Code asks for Claude's own model names (~/.claude/settings.json)"; !strings.Contains(flat(out), want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
}

func TestIsClaudeModel(t *testing.T) {
	for m, want := range map[string]bool{
		"opus": true, "sonnet": true, "haiku": true, "opusplan": true, "default": true, "sonnet[1m]": true,
		"best": true, "fable": true, "Fable[1m]": true,
		"claude-opus-5-5": true, "us.anthropic.claude-sonnet-5": true, "Claude-Haiku-4-5": true,
		"glm-5.3": false, "gpt-5": false, "premium-ide": false,
	} {
		if got := isClaudeModel(m); got != want {
			t.Errorf("isClaudeModel(%q) = %v, want %v", m, got, want)
		}
	}
}

func TestMappingText(t *testing.T) {
	for _, tc := range []struct {
		s    routerconfig.Server
		want string
	}{
		{routerconfig.Server{}, "uses Claude Code's names"},
		{routerconfig.Server{Opus: "glm-5.3", Sonnet: "glm-5.3", Haiku: "glm-5.3"}, "all → glm-5.3"},
		{routerconfig.Server{Opus: "big", Sonnet: "mid", Haiku: "small"}, "opus → big · sonnet → mid · haiku → small"},
	} {
		if got := mappingText(tc.s); got != tc.want {
			t.Errorf("mappingText(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestServer_HelpGoesToStdoutAndAWrongActionToStderr(t *testing.T) {
	if code, out, _ := runServerCmd(t, "", "--help"); code != 0 || !strings.Contains(out, "agentop server add <name> <url>") {
		t.Errorf("--help: exit %d, stdout:\n%s", code, out)
	}
	if code, _, errOut := runServerCmd(t, "", "rename"); code != 2 || !strings.Contains(errOut, `unknown action "rename"`) {
		t.Errorf("rename: exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServer_AnActionAfterAFlagIsRefusedWithTheFix(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	code, _, errOut := runServerCmd(t, "", "--config", path, "remove", "ete")
	if code != 2 || !strings.Contains(errOut, "the action comes first") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

// The router maps Claude's names, so every place Claude Code takes a model from is
// checked: the settings' top-level "model" setting, and each model variable,
// ANTHROPIC_DEFAULT_FABLE_MODEL among them. Claude's aliases pass wherever they sit.
func TestServer_ChecksEveryWayClaudeCodePicksAModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   map[string]any
		wantX string // the one ✗ expected; "" for none
	}{
		{"model key, another model", map[string]any{"model": "glm-5.3"},
			`✗ Claude Code asks for glm-5.3 instead of Claude's models ("model" in ~/.claude/settings.json). Cortex can't map that back: remove the line`},
		{"fable variable, another model", map[string]any{"env": map[string]any{"ANTHROPIC_DEFAULT_FABLE_MODEL": "glm-5.3"}},
			`✗ Claude Code asks for glm-5.3 instead of Claude's models (ANTHROPIC_DEFAULT_FABLE_MODEL in ~/.claude/settings.json)`},
		{"model key fable", map[string]any{"model": "fable"}, ""},
		{"model key opus", map[string]any{"model": "opus"}, ""},
		{"model key, a claude id", map[string]any{"model": "claude-opus-5-5[1m]"}, ""},
		{"fable and best variables", map[string]any{"env": map[string]any{"ANTHROPIC_MODEL": "fable", "CLAUDE_CODE_SUBAGENT_MODEL": "best"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), routerBlock)
			env, _ := tc.doc["env"].(map[string]any)
			if env == nil {
				env = map[string]any{}
			}
			env["ANTHROPIC_BASE_URL"] = "https://ete.example.com"
			tc.doc["env"] = env
			writeClaudeSettingsDoc(t, filepath.Dir(path), tc.doc)
			_, out, _ := runServerCmd(t, "", "--config", path)
			got := flat(out)
			if tc.wantX == "" {
				if want := "✓ Claude Code asks for Claude's own model names"; strings.Contains(got, "✗") || !strings.Contains(got, want) {
					t.Errorf("want no ✗ and %q in:\n%s", want, out)
				}
				return
			}
			if strings.Count(got, "✗") != 1 || !strings.Contains(got, tc.wantX) {
				t.Errorf("want exactly one ✗, %q, in:\n%s", tc.wantX, out)
			}
		})
	}
}

// A key pasted into a server URL as user info must not reach the terminal, and
// url.URL.Redacted would not stop it: it masks a password but not a username.
func TestServer_ListsNoURLCredentials(t *testing.T) {
	router := strings.NewReplacer(
		"https://ete.example.com", "https://sk-SECRET-ETE@ete.example.com/sk-SECRET-PATH",
		"https://glm.example.com:8443", "https://user:sk-SECRET-GLM@glm.example.com:8443",
	).Replace(routerBlock)
	path := serverEnv(t, closedAddr(t), router)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if all := out + errOut; strings.Contains(all, "sk-SECRET") || strings.Contains(all, "user:") {
		t.Errorf("the listing prints URL user info:\n%s%s", out, errOut)
	}
	lines := strings.Split(out, "\n")
	if got := flat(lines[0]); !strings.HasPrefix(got, "ete not a valid URL uses") {
		t.Errorf("line 1 = %q, want not a valid URL and nothing of the URL", got)
	}
	if got := flat(lines[1]); !strings.HasPrefix(got, "glm not a valid URL uses") {
		t.Errorf("line 2 = %q, want not a valid URL and nothing of the URL", got)
	}
}

func TestServer_ChecksPrintNoURLCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, want string
	}{
		{"elsewhere, key as username", "https://sk-SECRET@api.anthropic.com/v1",
			"✗ Claude Code points at a URL that is not one of these servers, so nothing is routed"},
		{"elsewhere, key as password", "https://user:sk-SECRET@api.anthropic.com",
			"✗ Claude Code points at a URL that is not one of these servers, so nothing is routed"},
		{"a server, with user info", "https://user:sk-SECRET@ete.example.com",
			"✓ Claude Code points at ete"},
		{"does not parse", "https://user:sk-SECRET@api.anthropic.com:port",
			"✗ ANTHROPIC_BASE_URL in ~/.claude/settings.json is not a URL with a host, so nothing is routed. Point it at one of the servers above."},
		{"no slashes", "https:user:sk-SECRET@api.anthropic.com",
			"✗ ANTHROPIC_BASE_URL in ~/.claude/settings.json is not a URL with a host"},
		{"elsewhere, key in the query", "https://other.example.com/?key=sk-SECRET",
			"✗ Claude Code points at https://other.example.com, which is not one of these servers"},
		{"elsewhere, key in the fragment", "https://other.example.com/v1#sk-SECRET",
			"✗ Claude Code points at https://other.example.com, which is not one of these servers"},
		{"elsewhere, key as a path segment", "https://other.example.com/v1/sk-SECRET",
			"✗ Claude Code points at https://other.example.com, which is not one of these servers"},
		{"elsewhere, with a port", "https://other.example.com:8443/v1/sk-SECRET",
			"✗ Claude Code points at https://other.example.com:8443, which is not one of these servers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), routerBlock)
			writeClaudeSettings(t, filepath.Dir(path), map[string]string{"ANTHROPIC_BASE_URL": tc.baseURL})
			_, out, errOut := runServerCmd(t, "", "--config", path)
			if all := out + errOut; strings.Contains(all, "sk-SECRET") || strings.Contains(all, "user:") {
				t.Errorf("the checks print a key from the URL:\n%s%s", out, errOut)
			}
			if !strings.Contains(flat(out), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out)
			}
		})
	}
}

// A server no agent is routed to must not end the column block: tabwriter aligns a
// column only across consecutive rows that all have it.
func TestServer_AgentsLineUpAroundAServerWithNone(t *testing.T) {
	path := serverEnv(t, closedAddr(t), `      - name: inference-router
        config:
          servers:
            aaa:
              url: https://a.example.com
              key: sk-a
              opus: big
              sonnet: mid
              haiku: small
            bbb:
              url: https://b.example.com
              key: sk-b
            ccc:
              url: https://c.example.com
              key: sk-c
          agents:
            claude-code: aaa
            opencode: ccc
`)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(out, "\n")
	// Columns, not bytes: the mapping's arrows are multi-byte.
	col := func(line, word string) int {
		i := strings.Index(line, word)
		if i < 0 {
			t.Fatalf("no %q in %q", word, line)
		}
		return utf8.RuneCountInString(line[:i])
	}
	if a, c := col(lines[0], "claude-code"), col(lines[2], "opencode"); a != c {
		t.Errorf("agents start in columns %d and %d:\n%s", a, c, out)
	}
	for i, line := range lines[:3] {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line %d ends in spaces: %q", i+1, line)
		}
	}
}

// With no home directory there is no ~/.claude/settings.json to check. The
// listing still prints, and the checks say why they did not run instead of
// reading .claude/settings.json from wherever agentop was started.
func TestServer_AnUnknownHomeIsReportedNotReadFromTheWorkingDirectory(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	cwd := t.TempDir()
	writeClaudeSettings(t, cwd, map[string]string{"ANTHROPIC_BASE_URL": "https://ete.example.com"})
	t.Chdir(cwd)
	t.Setenv("HOME", "")
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := flat(out)
	if !strings.Contains(got, "ete ete.example.com") {
		t.Errorf("want the listing still printed:\n%s", out)
	}
	if strings.Contains(got, "points at") {
		t.Errorf("read the working directory's .claude/settings.json:\n%s", out)
	}
	if want := "✗ Claude Code's settings are not checked: cannot determine your home directory"; !strings.Contains(got, want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
}

// A key given as a username that contains '/', '?' or '#' ends the authority
// early, so url.Parse takes the KEY for the host: https://sk-SECRET/x@h.example.com
// parses with Host "sk-SECRET". Nothing of a refused server URL may be shown, and
// no host of an ANTHROPIC_BASE_URL containing '@'.
func TestServer_AKeyTakenForTheHostIsNeverShown(t *testing.T) {
	for _, raw := range []string{
		"https://sk-SECRET/x@h.example.com",
		"https://sk-SECRET?x@h.example.com",
		"https://sk-SECRET#x@h.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), strings.Replace(routerBlock, "https://ete.example.com", raw, 1))
			writeClaudeSettings(t, filepath.Dir(path), map[string]string{"ANTHROPIC_BASE_URL": raw})
			code, out, errOut := runServerCmd(t, "", "--config", path)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if all := strings.ToLower(out + errOut); strings.Contains(all, "secret") {
				t.Errorf("prints the key:\n%s%s", out, errOut)
			}
			if got := flat(strings.Split(out, "\n")[0]); !strings.HasPrefix(got, "ete not a valid URL uses") {
				t.Errorf("line 1 = %q, want the server listed as not a valid URL, with nothing of the URL", got)
			}
			if want := "✗ Claude Code points at a URL that is not one of these servers, so nothing is routed (ANTHROPIC_BASE_URL in ~/.claude/settings.json). Point it at one of the servers above."; !strings.Contains(flat(out), want) {
				t.Errorf("want %q in:\n%s", want, out)
			}
		})
	}
}
