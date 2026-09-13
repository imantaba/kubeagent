package hypothesis

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/imantaba/kubeagent"

// allowedKubeagent is every kubeagent package a non-test file may import.
var allowedKubeagent = []string{modulePath + "/internal/inventory"}

// banned is every import path a non-test file may never name, alone or as
// a prefix. The three stdlib entries keep the package free of I/O, time and
// cancellation: it is handed values and returns values.
var banned = []string{
	"k8s.io/client-go",
	"k8s.io/apimachinery",
	modulePath + "/internal/cluster",
	modulePath + "/internal/investigate",
	modulePath + "/internal/remediate",
	modulePath + "/internal/explain",
	modulePath + "/internal/report",
	modulePath + "/internal/scan",
	"context",
	"io",
	"time",
}

// TestImportWalls pins the package's imports: stdlib, k8s.io/api and
// internal/inventory only, and none of the banned paths.
func TestImportWalls(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, imp := range importsOf(t, path) {
			for _, b := range banned {
				if imp == b || strings.HasPrefix(imp, b+"/") {
					t.Errorf("%s imports banned %q", path, imp)
				}
			}
			if strings.HasPrefix(imp, modulePath+"/") {
				ok := false
				for _, a := range allowedKubeagent {
					if imp == a {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s imports kubeagent package %q; only %v are allowed", path, imp, allowedKubeagent)
				}
				continue
			}
			first := strings.SplitN(imp, "/", 2)[0]
			if strings.Contains(first, ".") && !strings.HasPrefix(imp, "k8s.io/api/") {
				t.Errorf("%s imports %q; only stdlib, k8s.io/api and internal/inventory are allowed", path, imp)
			}
		}
	}
}

// importsOf parses one file's import block and returns the unquoted paths.
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
			continue
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists every Go file in this package's directory. An empty
// list would let the guard pass vacuously, so it is fatal.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found — the guard tests would pass vacuously")
	}
	return files
}
