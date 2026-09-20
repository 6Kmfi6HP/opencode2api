package bridge

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImports lists the import path prefixes that the bridge purity
// contract bans. Any import whose path equals or lives under one of these is
// a contract violation.
var forbiddenImports = []string{
	"net/http",
	"github.com/6Kmfi6HP/opencode2api/internal/stats",
	"github.com/6Kmfi6HP/opencode2api/internal/logging",
	"github.com/6Kmfi6HP/opencode2api/internal/config",
}

// internalImportPrefix scopes the internal/* bans above to this module, so a
// hypothetical stdlib package with a colliding name would not be flagged.
const internalImportPrefix = "github.com/6Kmfi6HP/opencode2api/internal/"

// TestNoForbiddenImports mechanically enforces the bridge purity contract: it
// parses every non-test .go source file in this package's directory and fails
// if any file imports net/http, internal/stats, internal/logging, or
// internal/config.
//
// Test files are intentionally exempt so the test itself (and any future
// white-box test helpers) may reference whatever they need; the contract
// governs shipped package source only.
func TestNoForbiddenImports(t *testing.T) {
	dir := "." // this test runs in the package directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read bridge dir: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			raw, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			if banned := forbiddenImport(raw); banned != "" {
				t.Errorf("%s imports forbidden package %q (purity contract bans %q)", name, raw, banned)
			}
		}
	}
}

// forbiddenImport returns the forbidden prefix that path matches, or "" if the
// import is allowed. A path matches when it equals a forbidden prefix exactly
// (e.g. "net/http") or, for the internal packages, lives under an exact banned
// package path. forbiddenImports entries are exact package roots, so we match
// either equal or as a path-prefix boundary.
func forbiddenImport(path string) string {
	// net/http is banned as an exact root and in every subpackage form
	// (net/http/httptest, net/http/cookiejar, ...).
	if path == "net/http" || strings.HasPrefix(path, "net/http/") {
		return "net/http"
	}
	for _, banned := range forbiddenImports {
		if !strings.HasPrefix(banned, internalImportPrefix) {
			continue
		}
		if path == banned || strings.HasPrefix(path, banned+"/") {
			return banned
		}
	}
	return ""
}
