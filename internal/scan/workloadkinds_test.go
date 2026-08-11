package scan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// TestWorkloadPassesEmitOnlyTheListedKinds closes the vocabulary of the kinds
// scan's workload passes build, the way internal/diagnose closes the detector
// set's.
//
// internal/knownissues documents the pod detectors, and three kinds kubeagent
// reports are deliberately not among them: RolloutStuck, FailedCreate and
// JobFailed come from the passes scan runs over workloads rather than from a
// Detector. knownissues.WorkloadKinds names them so `known-issues RolloutStuck`
// can answer honestly instead of calling an emitted kind unknown.
//
// That list has to stay true, and no existing test can keep it true. The four
// closure tests in internal/diagnose are scoped to internal/diagnose on
// purpose — they say nothing about a Finding built anywhere else — and
// internal/knownissues imports nothing, so it cannot see a workload pass at all.
// Each of those three kinds was added after a shape slipped through with every
// test green; a fourth would do the same. This is the test that fails instead.
//
// The package list is derived, not typed here: it comes from the <pkg>.Annotate
// call sites in scan.go, resolved through that file's own import block. A
// workload pass added later is walked because it is called, which is the
// property a hand-written list of three package names would not have.
//
// The honest boundary, written down rather than implied: this walk reads the
// two ways a Finding's Issue field is written — a key in a composite literal and
// an assignment to the field — and refuses every other shape by name rather than
// passing over it. It does not refuse a read: rootcause.go compares f.Issue
// against a kind, which is not a write and not this test's business. And unlike
// internal/diagnose's, it pins no import set: these packages read the Kubernetes
// API and legitimately import a great deal, so a decoder writing the field
// without naming it stays outside what this test can see.
func TestWorkloadPassesEmitOnlyTheListedKinds(t *testing.T) {
	dirs := annotatedPackageDirs(t)
	if len(dirs) == 0 {
		t.Fatal("found no <pkg>.Annotate call site in scan.go; the walk is broken and would pass vacuously")
	}

	seen := map[string]bool{}
	for _, dir := range dirs {
		for _, w := range issueWrites(t, dir) {
			if w.unreadable != "" {
				t.Errorf("%s: an Issue field is set from %s, a shape this walk cannot read, "+
					"so it can no longer tell which kinds a workload pass emits; "+
					"restore a readable shape or teach the walk this one", w.pos, w.unreadable)
				continue
			}
			seen[w.kind] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("the annotated packages set no Issue field at all; the walk is broken")
	}

	got := make([]string, 0, len(seen))
	for k := range seen {
		got = append(got, k)
	}
	sort.Strings(got)

	want := knownissues.WorkloadKinds()
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("scan's workload passes emit %v; knownissues.WorkloadKinds() lists %v.\n"+
			"A kind on one side and not the other means `known-issues <kind>` answers wrongly: "+
			"an emitted kind missing from the list is called unknown, and a listed kind nothing "+
			"emits is described as a finding kubeagent can produce.", got, want)
	}
}

// modulePath is this repository's module path, the prefix that tells a
// kubeagent import from a vendored one.
const modulePath = "github.com/imantaba/kubeagent/"

// annotatedPackageDirs returns the directory of every kubeagent package scan.go
// calls Annotate on, relative to this one, sorted and deduplicated.
//
// The identifier in pkg.Annotate(...) is resolved through scan.go's import
// block rather than assumed to equal the last path element, so a renamed import
// resolves correctly. A non-kubeagent package is dropped: the question is which
// of kubeagent's own packages build a Finding.
func annotatedPackageDirs(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "scan.go", nil, 0)
	if err != nil {
		t.Fatalf("parse scan.go: %v", err)
	}

	byName := map[string]string{}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s: %v", spec.Path.Value, err)
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		byName[name] = path
	}

	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Annotate" {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path, ok := byName[id.Name]
		if !ok || !strings.HasPrefix(path, modulePath) {
			return true
		}
		// internal/scan is one level below the module root, so a sibling
		// package's directory is the module-relative path rewritten from here.
		seen[filepath.Join("..", "..", strings.TrimPrefix(path, modulePath))] = true
		return true
	})

	out := make([]string, 0, len(seen))
	for dir := range seen {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// issueWrite is one place a package's non-test sources set a Finding's Issue
// field: the kind when the value is a plain string literal, or a named reason
// when it is a shape this walk does not read.
type issueWrite struct {
	pos        string
	kind       string
	unreadable string
}

// issueWrites returns every Issue field written in dir's non-test sources.
//
// Both ways Go offers to set a field are read — a key in a composite literal,
// Issue: "JobFailed", and an assignment to the field, f.Issue = x — because
// reading only the first is how a gap survived review in internal/diagnose: a
// refactor that builds the common fields and sets Issue afterwards was not
// refused, it was never seen.
func issueWrites(t *testing.T, dir string) []issueWrite {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(files)

	var out []issueWrite
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			for _, v := range issueFieldValues(n) {
				w := issueWrite{pos: fmt.Sprint(fset.Position(v.Pos()))}
				lit, ok := v.(*ast.BasicLit)
				switch {
				case !ok || lit.Kind != token.STRING:
					w.unreadable = "an expression other than a string literal"
				default:
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						w.unreadable = "a string literal this walk cannot unquote"
						break
					}
					w.kind = s
				}
				out = append(out, w)
			}
			return true
		})
	}
	return out
}

// issueFieldValues returns the expressions n gives to a Finding's Issue field.
//
// The left-hand side of an assignment is matched on the field name alone, so
// f.Issue, found[i].Issue and p.q.Issue all count. A compound assignment or a
// multi-value assignment whose sides do not line up one to one yields the
// left-hand side itself, which is not a string literal and so is refused by
// name rather than skipped.
func issueFieldValues(n ast.Node) []ast.Expr {
	switch v := n.(type) {
	case *ast.KeyValueExpr:
		if key, ok := v.Key.(*ast.Ident); ok && key.Name == "Issue" {
			return []ast.Expr{v.Value}
		}
	case *ast.AssignStmt:
		var out []ast.Expr
		for i, lhs := range v.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Issue" {
				continue
			}
			if v.Tok != token.ASSIGN || len(v.Rhs) != len(v.Lhs) {
				out = append(out, lhs)
				continue
			}
			out = append(out, v.Rhs[i])
		}
		return out
	}
	return nil
}
