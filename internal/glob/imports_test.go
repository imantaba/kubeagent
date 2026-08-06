package glob

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this repository's module path. Any import that begins with it
// is a kubeagent package.
const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport pins the wall: internal/glob imports nothing from
// kubeagent. It is a leaf both internal/policy and internal/cli depend on, and
// a leaf that reached back into kubeagent could grow a cycle or — worse — reach
// internal/remediate. Keeping the wall at "nothing from kubeagent" makes that
// impossible by construction rather than by rule.
func TestNoKubeagentImport(t *testing.T) {
	for _, file := range packageFiles(t) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, imp := range importsOf(t, file) {
			if strings.HasPrefix(imp, modulePath) {
				t.Errorf("%s imports %s; internal/glob must import nothing from kubeagent", file, imp)
			}
		}
	}
}

// TestStdlibOnly pins the second half: no third-party dependency either. A
// standard-library import path has no dot in its first segment; every module
// path does, because it starts with a hostname.
func TestStdlibOnly(t *testing.T) {
	for _, file := range packageFiles(t) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, imp := range importsOf(t, file) {
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s; internal/glob must import nothing outside the standard library", file, imp)
			}
		}
	}
}

// importsOf returns the import paths one file declares.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquoting %s in %s: %v", spec.Path.Value, path, err)
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists this package's Go files. It fatals on an empty result so a
// guard above can never pass vacuously.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; the guards would pass vacuously")
	}
	return files
}
