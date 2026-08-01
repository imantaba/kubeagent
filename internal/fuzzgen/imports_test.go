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

// constrained lists packages whose import graph is part of kubeagent's
// security contract, and what each may never reach. The rule is about
// capability: a package that cannot import internal/remediate cannot write to
// a cluster, and one that cannot import internal/explain cannot make a model
// call, no matter what a future edit does inside it.
//
// internal/schemadoc is deliberately the inverse case and is absent here: it
// imports the surface packages to name the document roots, so it transitively
// reaches remediate and explain. The invariants constrain what these packages
// import, not who imports them.
var constrained = map[string][]string{
	"internal/fuzzgen":     {"internal/remediate", "internal/explain"},
	"internal/safetext":    {"internal/remediate", "internal/explain"},
	"internal/parallel":    {"internal/remediate", "internal/explain"},
	"internal/mcp":         {"internal/remediate", "internal/explain"},
	"internal/gate":        {"internal/remediate", "internal/explain"},
	"internal/rbacprofile": {"internal/remediate", "internal/explain"},
	"internal/tui": {
		"internal/remediate", "internal/explain",
		"internal/investigate", "internal/report",
	},
	// internal/policy is pure: it is handed bytes and objects and returns
	// values. It may not reach report/scan/findings either — findings imports
	// scan and scan imports policy, so a policy import of findings would close
	// a cycle, which is why policy declares its own Level type.
	"internal/policy": {
		"internal/remediate", "internal/explain", "internal/investigate",
		"internal/report", "internal/scan", "internal/findings",
	},
}

// TestNoProductionImport keeps fuzzgen out of the shipped binary. It generates
// hostile Kubernetes objects and imports "testing"; if any non-test file
// imported it, every kubeagent binary would carry the testing package's flag
// registrations and its init-time cost.
//
// It also enforces the wider set of import invariants in constrained above:
// a package listed there may never reach one of the packages named for it,
// anywhere in the tree, in a non-test file.
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
			slash := filepath.ToSlash(path)
			for dir, forbidden := range constrained {
				if !strings.Contains(slash, "/"+dir+"/") {
					continue
				}
				for _, bad := range forbidden {
					if strings.HasSuffix(p, "/"+bad) {
						t.Errorf("%s imports %s — %s may never reach it", path, p, dir)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}
