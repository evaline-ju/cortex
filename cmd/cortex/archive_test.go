package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// localConfig returns a config path inside a fresh $HOME's ~/.cortex — what a local install
// runs from — and one outside it.
func localConfig(t *testing.T) (inside, outside string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	inside = filepath.Join(home, ".cortex", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(inside), 0o700); err != nil {
		t.Fatal(err)
	}
	return inside, filepath.Join(t.TempDir(), "config.yaml")
}

func archiveCfg(enabled *bool) *config.Config {
	return &config.Config{Session: config.SessionConfig{Archive: &config.SessionArchiveConfig{Enabled: enabled}}}
}

// loopbackOnly is cfg bound to loopback only, as the config `cortex --local` writes is.
func loopbackOnly(cfg *config.Config) *config.Config {
	cfg.Listener.BindLoopbackOnly = true
	return cfg
}

// On by default only where DELETE /v1/sessions can clear what it writes: a local install bound to
// loopback only. An explicit true opts in on any local install; nothing outside one runs it.
func TestSessionArchiveRuns_OnForALocalInstall(t *testing.T) {
	inside, outside := localConfig(t)
	on, off := true, false
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		path string
		want bool
	}{
		{"local, unset, loopback only", loopbackOnly(&config.Config{}), inside, true},
		{"local, unset, not loopback only", &config.Config{}, inside, false},
		{"local, enabled, not loopback only", archiveCfg(&on), inside, true},
		{"local, disabled, loopback only", loopbackOnly(archiveCfg(&off)), inside, false},
		{"not local, enabled", archiveCfg(&on), outside, false},
		{"not local, unset", &config.Config{}, outside, false},
	} {
		got, why := sessionArchiveRuns(tc.cfg, tc.path)
		if got != tc.want || why == "" {
			t.Errorf("%s: sessionArchiveRuns = %v (%q), want %v and a reason", tc.name, got, why, tc.want)
		}
	}
}

func TestSessionArchiveDir_IsUnderCortexDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := sessionArchiveDir()
	if err != nil || dir != filepath.Join(home, ".cortex", "sessions") {
		t.Fatalf("sessionArchiveDir = %q, %v", dir, err)
	}
}

// The wiring end to end: on a local install with the archive enabled, events the store appends
// reach disk, and the store seeds numbering from the archive.
func TestOpenSessionArchive_RecordsWhatTheStoreAppends(t *testing.T) {
	inside, _ := localConfig(t)
	on := true
	st := session.New(0, 0, 0)
	defer st.Close()
	arch := openSessionArchive(archiveCfg(&on), inside, st)
	if arch == nil {
		t.Fatal("the archive did not open on a local install with enabled: true")
	}
	st.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if err := arch.Close(); err != nil {
		t.Fatal(err)
	}
	if arch.LastSeq("s1") != 1 {
		t.Fatalf("the archive did not see the store's event: LastSeq = %d", arch.LastSeq("s1"))
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(inside), "sessions", "data", "*", "session.json"))
	if len(matches) != 1 {
		t.Fatalf("%d session.json files under ~/.cortex/sessions, want 1", len(matches))
	}
}

// The archive runs by default on a local install, so it must open from a config that never
// mentions it, as well as from one that sets its bounds without saying enabled.
func TestOpenSessionArchive_OpensWithEnabledUnset(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cfg           *config.Config
		wantRetention int
	}{
		{"no session.archive block", loopbackOnly(&config.Config{}), 30},
		{"bounds without enabled", loopbackOnly(&config.Config{Session: config.SessionConfig{
			Archive: &config.SessionArchiveConfig{RetentionDays: 7}}}), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inside, _ := localConfig(t)
			st := session.New(0, 0, 0)
			defer st.Close()
			arch := openSessionArchive(tc.cfg, inside, st)
			if arch == nil {
				t.Fatal("the archive did not open on a local install")
			}
			if got := arch.RetentionDays(); got != tc.wantRetention {
				t.Errorf("RetentionDays = %d, want %d", got, tc.wantRetention)
			}
			st.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
			if err := arch.Close(); err != nil {
				t.Fatal(err)
			}
			if arch.LastSeq("s1") != 1 {
				t.Fatalf("the archive did not see the store's event: LastSeq = %d", arch.LastSeq("s1"))
			}
		})
	}
}

func TestOpenSessionArchive_NotOnALocalInstallIsNil(t *testing.T) {
	_, outside := localConfig(t)
	on := true
	st := session.New(0, 0, 0)
	defer st.Close()
	if arch := openSessionArchive(archiveCfg(&on), outside, st); arch != nil {
		arch.Close()
		t.Fatal("the archive opened outside a local install")
	}
}

// fatalf ends in os.Exit, which runs no defers, so the archive's open segments are only finished
// on a fatal startup error if fatalf closes them itself — the same reason it closes the ledger.
func TestFatalf_ClosesTheArchive(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "fatalf" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "closeArchiveOnFatal" {
					found = true
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatal("fatalf never calls closeArchiveOnFatal: a fatal startup error would leave open segments unfinished")
	}
}
