package edit

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func routeClaudeToGLM() ConfigChange { return change([]string{"agents", "claude-code"}, "glm") }

func TestWritePluginConfig_WritesAndReportsTheReload(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	srv := makeStatusServer(t, func() ReloadStatus { return ReloadStatus{LastSuccess: time.Now()} })

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloaded {
		t.Errorf("outcome = %v, want WriteReloaded", res.Outcome)
	}
	if got, want := readFile(t, path), strings.Replace(withRouter, "claude-code: ete", "claude-code: glm", 1); got != want {
		t.Errorf("file differs:\n%s", Diff([]byte(want), []byte(got)))
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's own 0600 kept", st.Mode().Perm())
	}
}

func TestWritePluginConfig_ReportsAReloadTheProxyRefused(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		if calls.Add(1) == 1 {
			return ReloadStatus{ReloadsFailed: 2}
		}
		return ReloadStatus{ReloadsFailed: 3, LastError: `configure "inference-router": servers: at least one server is required`}
	})

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteReloadFailed || !strings.Contains(res.ReloadError, "at least one server") {
		t.Errorf("result = %+v, want WriteReloadFailed with the proxy's error", res)
	}
}

func TestWritePluginConfig_WithNoProxyWritesAndSaysSo(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteNotRunning {
		t.Errorf("outcome = %v, want WriteNotRunning", res.Outcome)
	}
	if !strings.Contains(readFile(t, path), "claude-code: glm") {
		t.Error("the file was not written")
	}
}

// The reloader ignores a byte-identical file, so a poll after a no-op write would
// wait out its whole deadline for a reload that never comes.
func TestWritePluginConfig_AnUnchangedValueIsNeitherWrittenNorPolled(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	before, _ := os.Stat(path)
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus { calls.Add(1); return ReloadStatus{LastSuccess: time.Now()} })

	res, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, StatsURL: srv.URL,
		Changes: []ConfigChange{change([]string{"agents", "claude-code"}, "ete")}})
	if err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if res.Outcome != WriteUnchanged {
		t.Errorf("outcome = %v, want WriteUnchanged", res.Outcome)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("polled /reload/status %d times for a write that did not happen", n)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("the file was rewritten")
	}
}

func TestWritePluginConfig_NeverWritesAResultThatWillNotLoad(t *testing.T) {
	src := withRouter + "mtls:\n  mode: sideways\n"
	path := writeFixtureContent(t, src, 0o600)
	_, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err == nil || !strings.Contains(err.Error(), "would not load") {
		t.Fatalf("err = %v, want a load refusal", err)
	}
	if readFile(t, path) != src {
		t.Error("a result that does not load was written")
	}
}

func TestWritePluginConfig_NeverWritesAResultVerifyRefuses(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	refuse := func(*config.Config) error { return errors.New("glm is not a server") }
	_, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}, Verify: refuse})
	if err == nil || !strings.Contains(err.Error(), "glm is not a server") {
		t.Fatalf("err = %v, want Verify's refusal", err)
	}
	if readFile(t, path) != withRouter {
		t.Error("a result Verify refused was written")
	}
}

// Verify sees the config the proxy would load, so it can check the plugin's own block.
func TestWritePluginConfig_VerifySeesTheResult(t *testing.T) {
	path := writeFixtureContent(t, withRouter, 0o600)
	var saw string
	look := func(c *config.Config) error {
		for _, e := range c.Pipeline.Outbound.Plugins {
			if e.Name == "inference-router" {
				saw = string(e.Config)
			}
		}
		return nil
	}
	if _, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}, Verify: look}); err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	if !strings.Contains(saw, `"claude-code":"glm"`) {
		t.Errorf("Verify saw %s, want the changed agents block", saw)
	}
}

func TestWritePluginConfig_AChangeThatCannotBeMadeWritesNothing(t *testing.T) {
	path := writeFixtureContent(t, localConfig, 0o600)
	_, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{routeClaudeToGLM()}})
	if err == nil || !strings.Contains(err.Error(), "has no inference-router entry") {
		t.Fatalf("err = %v, want the missing-entry error", err)
	}
	if readFile(t, path) != localConfig {
		t.Error("the file changed")
	}
}

// Changes apply in order, each to the result of the one before: add a server, then
// route an agent to it, in one write and one reload.
func TestWritePluginConfig_AppliesChangesInOrder(t *testing.T) {
	path := writeFixtureContent(t, localConfig, 0o600)
	add := change([]string{"servers", "ete"}, "url", "https://ete.example.com", "key", "sk-ete")
	add.CreatePlugin = true
	use := change([]string{"agents", "claude-code"}, "ete")
	if _, err := WritePluginConfig(context.Background(), ConfigWrite{Path: path, Changes: []ConfigChange{add, use}}); err != nil {
		t.Fatalf("WritePluginConfig: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "              key: sk-ete\n          agents:\n            claude-code: ete\n") {
		t.Errorf("want the server then the agent:\n%s", got)
	}
}
