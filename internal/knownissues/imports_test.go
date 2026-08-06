package knownissues

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport pins the first half of this package's wall: it imports
// nothing from kubeagent at all. That puts internal/remediate and
// internal/explain out of reach by construction rather than by rule, which is
// the same guarantee internal/jsonschema, internal/dashboard, internal/baseline
// and internal/glob carry.
func TestNoKubeagentImport(t *testing.T) {
	for _, f := range packageFiles(t) {
		for _, imp := range importsOf(t, f) {
			if strings.HasPrefix(imp, modulePath) {
				t.Errorf("%s imports %q; this package must import nothing from kubeagent", f, imp)
			}
		}
	}
}

// TestStdlibOnly pins the second half: nothing outside the standard library
// either. A standard-library path's first segment never contains a dot; a
// module path's always does.
func TestStdlibOnly(t *testing.T) {
	for _, f := range packageFiles(t) {
		for _, imp := range importsOf(t, f) {
			first, _, _ := strings.Cut(imp, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q; this package is standard-library only", f, imp)
			}
		}
	}
}

// packageFiles lists the package's non-test .go files. It is fatal on an empty
// result so the guards above can never pass vacuously.
func packageFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var files []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files found; the import guards would pass vacuously")
	}
	return files
}

// importsOf returns the import paths of one file.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote %s in %s: %v", spec.Path.Value, path, err)
		}
		out = append(out, p)
	}
	return out
}
