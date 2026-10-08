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
// whose outbound chain ends with router (none when ""). It returns the config's path.
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

// writeClaudeSettings puts env in the scratch HOME's ~/.claude/settings.json.
func writeClaudeSettings(t *testing.T, env map[string]string) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"env": env})
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
		{"elsewhere", "https://api.anthropic.com", "✗ Claude Code points at https://api.anthropic.com, which is not one of these servers, so nothing is routed"},
		{"nowhere", "", "✗ ~/.claude/settings.json sets no ANTHROPIC_BASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), routerBlock)
			env := map[string]string{}
			if tc.baseURL != "" {
				env["ANTHROPIC_BASE_URL"] = tc.baseURL
			}
			writeClaudeSettings(t, env)
			_, out, _ := runServerCmd(t, "", "--config", path)
			if !strings.Contains(flat(out), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out)
			}
		})
	}
}

func TestServer_FlagsAModelVariableThatNamesAnotherModel(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	writeClaudeSettings(t, map[string]string{
		"ANTHROPIC_BASE_URL":           "https://ete.example.com",
		"ANTHROPIC_MODEL":              "glm-5.3",
		"ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5-5",
		"ANTHROPIC_SMALL_FAST_MODEL":   "haiku",
		"CLAUDE_CODE_SUBAGENT_MODEL":   "opus[1m]",
	})
	_, out, _ := runServerCmd(t, "", "--config", path)
	got := flat(out)
	if want := "✗ Claude Code asks for glm-5.3 instead of Claude's models (ANTHROPIC_MODEL in ~/.claude/settings.json). Cortex can't map that back: remove the line"; !strings.Contains(got, want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
	if strings.Count(got, "✗") != 1 {
		t.Errorf("want exactly one failed check — Claude's own names and aliases pass:\n%s", out)
	}
}

func TestServer_PassesSettingsThatNameOnlyClaudesModels(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	writeClaudeSettings(t, map[string]string{"ANTHROPIC_BASE_URL": "https://ete.example.com"})
	_, out, _ := runServerCmd(t, "", "--config", path)
	if want := "✓ Claude Code asks for Claude's own model names (~/.claude/settings.json)"; !strings.Contains(flat(out), want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
}

func TestIsClaudeModel(t *testing.T) {
	for m, want := range map[string]bool{
		"opus": true, "sonnet": true, "haiku": true, "opusplan": true, "default": true, "sonnet[1m]": true,
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
