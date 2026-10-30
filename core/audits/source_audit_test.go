// Package audits holds source-scanning tests: they parse the tree's non-test
// Go files and fail on patterns that have produced bugs more than once, so the
// next copy cannot arrive silently. Parser-only — no type information — which
// is the honest limit of each check and the reason every rule is textual.
package audits

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// scannedRoots are the subtrees these audits walk. The whole of core and cmd,
// modules and all, because the bug these guard against arrives in new code and
// does not respect module boundaries — parsing is path-based and needs none of
// them. Skipped: _test.go files, and anything under a directory starting with
// "." (worktrees).
var scannedRoots = []string{"../../core", "../../cmd"}

// sourceFiles walks the roots and returns every non-test .go file, relative to
// this package. An empty walk fails loudly: it is how a path change turns an
// audit into a no-op that reports success.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range scannedRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no source files under %v: these audits would pass over nothing", scannedRoots)
	}
	sort.Strings(out)
	return out
}

// parseAll parses every file from sourceFiles, once, for the tests that share
// it, and returns the fileset their positions refer to.
func parseAll(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, path := range sourceFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files[path] = file
	}
	return fset, files
}

// relPath renders a path relative to the repo root, the form the allowlists key
// on — stable no matter which module the test runs from.
func relPath(path string) string {
	trimmed := strings.TrimPrefix(path, "../../")
	if trimmed == path {
		return path
	}
	return filepath.ToSlash(trimmed)
}

// ---------------------------------------------------------------- transport

// Go's http.Transport keeps 2 idle connections per host unless told otherwise,
// so a concurrent caller over HTTP/1.1 pays a fresh TCP+TLS handshake for every
// request beyond the second — 10 concurrent callers to one gateway opened 36–45
// connections (#1546). Every transport that serves ongoing traffic must raise
// the pool; the exceptions are one-shot fetches, and each is listed here with
// its reason so an exemption is a decision someone wrote down.
var transportsWithoutIdlePool = map[string]string{
	// A single model-list fetch per refresh; no request ever reuses the pool.
	"core/plugins/inferencerouter/routerconfig/models.go:NewModelsClient": "one-shot fetch; the pool is never reused",
}

func TestTransportsSetIdleConnectionPool(t *testing.T) {
	seen := map[string]bool{}
	fset, files := parseAll(t)
	for path, parsed := range files {
		// The whole file, not just function bodies: a transport built in a
		// package-level var is as live as one built in a function, and looking
		// only at FuncDecls would not see it.
		for _, scope := range scopes(parsed) {
			ast.Inspect(scope.node, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isTransport(lit.Type) {
					return true
				}
				if compositeKeySet(lit, "MaxIdleConnsPerHost") {
					return true
				}
				key := fmt.Sprintf("%s:%s", relPath(path), scope.name)
				if _, ok := transportsWithoutIdlePool[key]; ok {
					seen[key] = true
					return true
				}
				t.Errorf(`%s builds an http.Transport without MaxIdleConnsPerHost

  Go keeps 2 idle connections per host by default, so concurrent requests over
  HTTP/1.1 pay a new TCP+TLS handshake each — the pool churn #1546 measured.
  Set MaxIdleConnsPerHost on this transport, or add it to
  transportsWithoutIdlePool with the reason it is exempt.`,
					fset.Position(lit.Pos()))
				return true
			})
		}
	}
	assertNoStaleEntries(t, "transportsWithoutIdlePool", transportsWithoutIdlePool, seen)
}

// isTransport reports whether e is an http.Transport composite literal type.
func isTransport(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Transport"
}

// ------------------------------------------------------------------ rename

// Renaming a temp file over its destination without f.Sync() first can leave
// the destination EMPTY after a power loss — the very outcome the rename was
// meant to rule out (#1547). The rule covers a function that creates or writes
// the file it renames; pure renames (rotations, trash moves) are outside it,
// because the bytes they move were synced where they were written. The
// exemptions are files that are rebuilt from another source on next use, where
// a lost write costs a rebuild and nothing else.
var renamesWithoutSync = map[string]string{
	// A restart re-downloads the list when the cache is unreadable.
	"core/cost/pricing/pricelist/pricelist.go:save": "cache; re-downloaded on a bad read",
	// A corrupt install is what `agentop script install` exists to redo.
	"cmd/agentop/cmd_script.go:writeAtomic": "installed files; reinstall restores them",
	// Rewritten from the embedded source on the next start that needs it.
	"core/plugins/script/sdk.go:installSDK": "embedded source; rewritten on next start",
}

func TestRenamesSyncFirst(t *testing.T) {
	seen := map[string]bool{}
	fset, files := parseAll(t)
	for path, parsed := range files {
		for _, fn := range functions(parsed) {
			if !callsSelector(fn, "os", "Rename") || !writesTempFile(fn) || synced(fn) {
				continue
			}
			key := fmt.Sprintf("%s:%s", relPath(path), fn.Name.Name)
			if _, ok := renamesWithoutSync[key]; ok {
				seen[key] = true
				continue
			}
			t.Errorf(`%s renames a file it did not Sync

  Without f.Sync() between the write and os.Rename, a power loss can leave the
  renamed file empty on disk (#1547). Sync the temp file before the rename, or
  route the write through a helper that does (writeFileAtomic), or add the
  function to renamesWithoutSync with the reason a lost write costs nothing.`,
				fmt.Sprintf("%s:%s", fset.Position(fn.Pos()), fn.Name.Name))
		}
	}
	assertNoStaleEntries(t, "renamesWithoutSync", renamesWithoutSync, seen)
}

// ---------------------------------------------------------------- helpers

// functions returns every function declaration in file, methods included.
func functions(file *ast.File) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			out = append(out, fn)
		}
	}
	return out
}

// scope is one naming unit for an allowlist key: a function, or the package
// scope for declarations outside any function.
type scope struct {
	name string
	node ast.Node
}

// scopes returns every place a declaration can live: each function under its
// own name, and each non-function declaration (a package-level var, const or
// type) under "package-scope". Walking the declarations rather than the file
// keeps the two from overlapping — Inspect on the file would descend into
// function bodies and key their literals as package-scope too.
func scopes(file *ast.File) []scope {
	var out []scope
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			out = append(out, scope{name: fn.Name.Name, node: fn})
			continue
		}
		out = append(out, scope{name: "package-scope", node: decl})
	}
	return out
}

// callsSelector reports whether fn contains a <pkg>.<name> call; "*" matches
// any package.
func callsSelector(fn *ast.FuncDecl, pkg, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && (pkg == "*" || id.Name == pkg) {
			found = true
		}
		return true
	})
	return found
}

// writesTempFile reports whether fn creates or writes the file it goes on to
// rename — the shape the rule is about: every spelling the tree uses for
// making the temp file.
func writesTempFile(fn *ast.FuncDecl) bool {
	for _, name := range []string{"CreateTemp", "WriteFile", "Create", "OpenFile"} {
		if callsSelector(fn, "os", name) {
			return true
		}
	}
	return false
}

// synced reports whether fn syncs what it wrote: a .Sync method call, or a
// call to a local seam like syncFile (an identifier containing "sync"). Loose
// on purpose — what matters is that a sync happened, not its spelling.
func synced(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name == "Sync" {
				found = true
			}
		case *ast.Ident:
			if strings.Contains(strings.ToLower(fun.Name), "sync") {
				found = true
			}
		}
		return true
	})
	return found
}

// compositeKeySet reports whether lit carries a keyed field named key.
func compositeKeySet(lit *ast.CompositeLit, key string) bool {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return true
			}
		}
	}
	return false
}

// assertNoStaleEntries fails for allowlist entries that matched nothing: an
// entry for code that no longer exists reads as coverage nothing checks.
func assertNoStaleEntries(t *testing.T, table string, allowlist map[string]string, seen map[string]bool) {
	t.Helper()
	var stale []string
	for key := range allowlist {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("%s has %q, which matches nothing in the source: remove it, or fix the key", table, key)
	}
}
