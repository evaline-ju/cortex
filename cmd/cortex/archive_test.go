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

// Until the archive can be read back and cleared, it runs only where someone asked for it on a
// laptop. Outside a local install it never runs: raw prompts on a cluster's volume are their own
// decision, and the archive has no dir to point at one.
func TestSessionArchiveRuns_OffByDefaultUntilLaunch(t *testing.T) {
	inside, outside := localConfig(t)
	on, off := true, false
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		path string
		want bool
	}{
		{"local, unset", &config.Config{}, inside, false},
		{"local, enabled", archiveCfg(&on), inside, true},
		{"local, disabled", archiveCfg(&off), inside, false},
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
