package dashboard

import (
	"go/ast"
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

// TestNoKubeagentImport is the structural half of this package's security
// contract. internal/dashboard renders a page for a browser out of daemon
// state; the design makes reaching internal/remediate or internal/explain
// impossible by construction rather than by a rule someone has to remember,
// by forbidding every kubeagent import. That is strictly stronger than the
// two-entry rule internal/fuzzgen's `constrained` map applies to the other
// surface packages, which is why this package is absent from that map: the
// weaker rule there would add nothing to the stronger one here.
//
// Only non-test files are walked. A test file importing internal/watch would
// not compile anyway — watch imports this package, so the edge would close a
// cycle and the compiler refuses it.
func TestNoKubeagentImport(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if strings.HasPrefix(p, modulePath) {
				t.Errorf("%s imports %s — internal/dashboard must import nothing from kubeagent", path, p)
			}
		}
	}
}

// unsafeConversions are the html/template types that carry a string into the
// document without escaping it. The design names HTML, JS and URL; the other
// four defeat the same boundary in the same way, so all seven are refused.
var unsafeConversions = map[string]bool{
	"HTML": true, "JS": true, "URL": true,
	"HTMLAttr": true, "CSS": true, "JSStr": true, "Srcset": true,
}

// TestNoUnsafeTemplateConversion asserts that contextual auto-escaping is this
// package's single escape boundary. Converting a string to one of the types
// above is the only way to defeat it, so their absence is what makes the
// escaping guarantee a property of the package rather than of its reviewers.
//
// Test files are walked too: a test that constructed template.HTML would be
// asserting on a value the renderer can never produce.
func TestNoUnsafeTemplateConversion(t *testing.T) {
	for _, path := range packageFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "template" {
				return true
			}
			if unsafeConversions[sel.Sel.Name] {
				t.Errorf("%s: references template.%s at %s — it defeats contextual escaping",
					path, sel.Sel.Name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
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
