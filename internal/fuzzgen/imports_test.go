package fuzzgen

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoProductionImport keeps fuzzgen out of the shipped binary. It generates
// hostile Kubernetes objects and imports "testing"; if any non-test file
// imported it, every kubeagent binary would carry the testing package's flag
// registrations and its init-time cost.
//
// go/parser with ImportsOnly is the whole implementation — no new dependency,
// and it reads import lists without type-checking the tree.
func TestNoProductionImport(t *testing.T) {
	const self = "github.com/imantaba/kubeagent/internal/fuzzgen"
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".github", "website", "chaos", "docs", "deploy":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if p == self {
				t.Errorf("%s imports %s from a non-test file — fuzzgen is test-only", path, self)
			}
			if strings.HasSuffix(p, "/internal/remediate") || strings.HasSuffix(p, "/internal/explain") {
				if strings.Contains(filepath.ToSlash(path), "/internal/fuzzgen/") {
					t.Errorf("%s imports %s — fuzzgen must never reach a write or a model call", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}
