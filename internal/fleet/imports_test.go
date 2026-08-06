package fleet

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// banned is the wall internal/fleet inherits from internal/gate, whose pipeline
// it runs. internal/remediate is the only package that writes to a cluster and
// internal/explain is the only one that calls a model — so keeping both out is
// what makes "read-only toward the cluster" and "makes no LLM call" two
// separate, checkable promises rather than one slogan.
var banned = []string{
	"github.com/imantaba/kubeagent/internal/remediate",
	"github.com/imantaba/kubeagent/internal/explain",
}

func TestNoRemediateOrExplainImport(t *testing.T) {
	for _, file := range packageFiles(t) {
		for _, imp := range importsOf(t, file) {
			for _, b := range banned {
				// The prefix arm covers a subpackage neither banned package has
				// today. Both are flat, so the arm is dead — and it is here so
				// that adding internal/remediate/foo tomorrow does not silently
				// open the wall.
				if imp == b || strings.HasPrefix(imp, b+"/") {
					t.Errorf("%s imports %s; internal/fleet must never import it", file, b)
				}
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

// packageFiles lists this package's Go files, test files included — a test that
// reached into internal/remediate would be just as much a hole in the wall. It
// fatals on an empty result so the guard can never pass vacuously.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; the guard would pass vacuously")
	}
	return files
}
