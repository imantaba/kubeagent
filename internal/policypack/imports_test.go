package policypack

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is kubeagent's module path. Any import beginning with it is an
// import of kubeagent.
const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport is the structural half of this package's contract.
// internal/policypack holds curated rule data that reaches a report and a
// gate; the design makes reaching internal/remediate or internal/explain
// impossible by construction rather than by a rule someone has to remember,
// by forbidding every kubeagent import. It is the same class as
// internal/baseline, internal/glob, internal/knownissues, internal/dashboard
// and internal/jsonschema.
//
// Only non-test files are walked: a test may import internal/policy to prove
// the pack loads without weakening what the shipped package can reach.
func TestNoKubeagentImport(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, p := range importsOf(t, path) {
			if strings.HasPrefix(p, modulePath) {
				t.Errorf("%s imports %s — internal/policypack must import nothing from kubeagent", path, p)
			}
		}
	}
}

// TestStdlibOnly is the second half: internal/policypack may import nothing
// outside the standard library either, so go.mod can never move because of
// this package. The convention Go itself uses is that a module path's first
// segment contains a dot; a standard-library import path never does.
func TestStdlibOnly(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, p := range importsOf(t, path) {
			first, _, _ := strings.Cut(p, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %s — internal/policypack must import only the standard library", path, p)
			}
		}
	}
}

func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists this package's Go files. The test binary runs with the
// package directory as its working directory, so a glob is enough — no walk,
// and no dependency on where the repository is checked out.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found — the guard tests would pass vacuously")
	}
	return files
}
