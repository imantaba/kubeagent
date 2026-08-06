package diagnose

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// TestEveryIssueLiteralIsDocumented is the static half: it parses this
// package's non-test sources and checks every string literal that reaches a
// Finding's Issue field against the registry.
//
// Two shapes appear in the detectors. A bare literal — Issue: "OOMKilled" — must
// be a documented kind. A literal ending in ':' is a prefix composed with a
// runtime value — Issue: "Init:" + w.Reason — and must have at least one
// documented kind beneath it; the exact set it can produce is not knowable
// statically, which is what the behavioural test below covers.
//
// Both ways of setting the field are read — a key in a composite literal and an
// assignment to the field afterwards — so a refactor that builds the common
// fields first and sets Issue on the next line cannot slip past. See issueValues.
//
// The honest limit, written down rather than implied: this test enumerates
// literals, not kinds. An Issue field assigned a bare variable (imagepull.go's
// Issue: w.Reason) contributes nothing here. It is a guard against a new
// literal slipping in undocumented, not a proof of completeness — that proof is
// TestDetectorsProduceOnlyDocumentedKinds.
func TestEveryIssueLiteralIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, k := range knownissues.Kinds() {
		documented[k] = true
	}

	literals := issueLiterals(t)
	if len(literals) == 0 {
		t.Fatal("found no Issue: literals; the walk is broken and would pass vacuously")
	}
	for _, lit := range literals {
		if strings.HasSuffix(lit, ":") {
			if !anyKindHasPrefix(documented, lit) {
				t.Errorf("Issue prefix %q composes kinds none of which are documented", lit)
			}
			continue
		}
		if !documented[lit] {
			t.Errorf("Issue %q is emitted by a detector and is not in internal/knownissues", lit)
		}
	}
}

// anyKindHasPrefix reports whether at least one documented kind starts with p.
func anyKindHasPrefix(documented map[string]bool, p string) bool {
	for k := range documented {
		if strings.HasPrefix(k, p) && k != p {
			return true
		}
	}
	return false
}

// issueLiterals collects every string literal that reaches an Issue field in
// this package's non-test files, including the left operand of a concatenation.
func issueLiterals(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)
	var out []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			values, _ := issueValues(n)
			for _, v := range values {
				if lit := leadingStringLit(v); lit != "" {
					out = append(out, lit)
				}
			}
			return true
		})
	}
	return out
}

// issueValues returns the expressions n gives to a Finding's Issue field, and a
// reason when n reaches the field in a way this walk will not read.
//
// Go offers two ways to set a field and both are read here: a key in a composite
// literal, Issue: x, and an assignment to the field, f.Issue = x. Reading only
// the first is how a real gap survived two rounds of review — a natural refactor
// that builds the common fields and sets Issue afterwards was not refused,
// it was never seen.
//
// The left-hand side is matched on the field name alone, so f.Issue,
// found[i].Issue and p.q.Issue all count. A compound assignment or a
// multi-value assignment whose sides do not line up one to one is reported as
// unreadable rather than guessed at.
func issueValues(n ast.Node) (values []ast.Expr, unreadable string) {
	switch v := n.(type) {
	case *ast.KeyValueExpr:
		if key, ok := v.Key.(*ast.Ident); ok && key.Name == "Issue" {
			return []ast.Expr{v.Value}, ""
		}
	case *ast.AssignStmt:
		for i, lhs := range v.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Issue" {
				continue
			}
			switch {
			case v.Tok != token.ASSIGN:
				unreadable = "a compound assignment to an Issue field"
			case len(v.Rhs) != len(v.Lhs):
				unreadable = "a multi-value assignment to an Issue field"
			default:
				values = append(values, v.Rhs[i])
			}
		}
	}
	return values, unreadable
}

// leadingStringLit unwraps a string literal, or the leftmost operand of a
// concatenation when that operand is one; "" when the expression begins with
// something else.
func leadingStringLit(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return ""
		}
		return leadingStringLit(v.X)
	}
	return ""
}

// TestDetectorsProduceOnlyDocumentedKinds is the behavioural half: it drives
// the shipped detector set over a fixture per kind and checks that every Issue
// it actually produces is documented. This is the test that covers the kinds
// composed at runtime, which the static walk cannot see.
func TestDetectorsProduceOnlyDocumentedKinds(t *testing.T) {
	for _, issue := range producedKinds(t) {
		if _, ok := knownissues.Lookup(issue); !ok {
			t.Errorf("a detector produced Issue %q, which is not in internal/knownissues", issue)
		}
	}
}

// TestEveryDocumentedKindIsProduced is the reverse: nothing in the registry
// documents a kind no detector can emit. Without this the registry could grow
// entries for kinds that were removed, or invented, and every other test would
// still pass.
func TestEveryDocumentedKindIsProduced(t *testing.T) {
	produced := map[string]bool{}
	for _, k := range producedKinds(t) {
		produced[k] = true
	}
	for _, k := range knownissues.Kinds() {
		if !produced[k] {
			t.Errorf("internal/knownissues documents %q, which no detector in this fixture set produces", k)
		}
	}
}

// producedKinds runs DefaultDetectors over one fixture per kind and returns the
// sorted, deduplicated set of Issue values that came out.
//
// The fixtures deliberately go through DefaultDetectors and Run rather than
// calling each detector directly: what must stay in step with the registry is
// the set kubeagent ships, not the set of types that happen to exist.
func producedKinds(t *testing.T) []string {
	t.Helper()

	facts := []PodFacts{
		// CrashLoopBackOff
		{Pod: podWaiting("example-ns", "web-1", "app", "CrashLoopBackOff", "")},
		// CreateContainerConfigError
		{Pod: podWaiting("example-ns", "web-2", "app", "CreateContainerConfigError",
			`configmap "app-config" not found`)},
		// ErrImagePull
		{Pod: podWaiting("example-ns", "web-3", "app", "ErrImagePull", "manifest unknown")},
		// ImagePullBackOff
		{Pod: podWaiting("example-ns", "web-4", "app", "ImagePullBackOff", "back-off pulling image")},
		// Init:CrashLoopBackOff
		{Pod: podWithInit("example-ns", "web-5", corev1.ContainerStatus{
			Name: "setup", RestartCount: 4,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		})},
		// Init:ErrImagePull
		{Pod: podWithInit("example-ns", "web-6", corev1.ContainerStatus{
			Name:  "setup",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
		})},
		// Init:ImagePullBackOff
		{Pod: podWithInit("example-ns", "web-7", corev1.ContainerStatus{
			Name:  "setup",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		})},
		// Init:OOMKilled
		{Pod: podWithInit("example-ns", "web-8", corev1.ContainerStatus{
			Name:  "setup",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		})},
		// OOMKilled
		{Pod: podOOMKilled("example-ns", "web-9", "app", 137, false)},
		// ProbeFailure
		{Pod: pfPod("example-ns", "web-10", "app"), Events: []corev1.Event{
			pfEvent("example-ns", "web-10", "app", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
		}},
		// RestartLoop
		{Pod: flapPod(3, 20*time.Second, 1, "Error", 25*time.Second)},
		// Unschedulable
		{Pod: podUnschedulable("example-ns", "web-11", "0/3 nodes are available: 3 Insufficient memory.")},
		// VolumeAttachError
		{Pod: podCreating("example-ns", "web-12"), Events: []corev1.Event{
			attachEvent("example-ns", "web-12",
				`Multi-Attach error for volume "pvc-example" Volume is already exclusively attached to one node`),
		}},
	}

	seen := map[string]bool{}
	for _, f := range Run(DefaultDetectors(rlNow), facts) {
		seen[f.Issue] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("the fixture set produced no findings at all")
	}
	return out
}

// TestDynamicIssueSitesProduceOnlyDocumentedKinds closes the one hole the other
// three tests share.
//
// Two detectors build an Issue kind from a runtime value rather than a literal:
// imagepull.go's Issue: w.Reason and initcontainer.go's Issue: "Init:" + w.Reason.
// The static walk cannot see through either, and the behavioural tests only
// exercise the fixtures someone remembered to write. So widening a guard —
// adding || w.Reason == "InvalidImageName" to imagepull.go — would let a
// fourteenth kind reach a report while all three tests stayed green.
//
// This test reads the guards instead of the output. For every function that
// builds an Issue from a runtime value, it collects every string literal that
// function tests a .Reason field against, composes each with that site's
// constant prefix, and requires the result to be documented. Widen a guard and
// the new kind is computed here, found undocumented, and the test fails.
//
// The design rule that makes that claim hold is that the walk recognises a
// closed set of shapes and REFUSES everything else rather than ignoring it.
// A guard it cannot read is a test failure naming the function, not a silent
// pass — so rewriting a guard into a shape this walk does not understand
// (extracting the comparison into a helper, inverting it to !=, testing the
// reason some other way, comparing it against a named constant) fails the suite
// just as widening it does. The two readable guards are an == comparison
// against a .Reason field and a switch whose tag is one, in both cases against a
// string literal; the two readable Issue values are a bare .Reason selector and
// a string literal added to one. Teaching the walk a new shape is a deliberate
// act, which is the point.
//
// The same rule governs where the field is set, not only what it is set to: a
// key in a composite literal and an assignment to the field are both read, and a
// compound or multi-value assignment to it is refused. Reading only the composite
// literal would have left f.Issue = w.Reason invisible rather than refused —
// a category the walk never saw, which is the failure this design exists to
// prevent. See issueValues.
//
// Within a recognised shape it over-approximates on purpose: a .Reason literal
// tested for some unrelated purpose in the same function still counts as a
// producible kind. That direction can only raise a false alarm, and the fix for
// one — document the kind, or split the function — leaves the vocabulary honest
// either way.
func TestDynamicIssueSitesProduceOnlyDocumentedKinds(t *testing.T) {
	documented := map[string]bool{}
	for _, k := range knownissues.Kinds() {
		documented[k] = true
	}

	sites := dynamicIssueSites(t)
	if len(sites) == 0 {
		t.Fatal("found no dynamic Issue: site; the walk is broken and would pass vacuously")
	}
	for _, s := range sites {
		if s.unreadable != "" {
			t.Errorf("%s: %s builds an Issue kind from %s, a shape this walk cannot read, "+
				"so it can no longer tell whether the kinds reaching a report are documented; "+
				"restore a readable shape or teach the walk this one", s.file, s.fn, s.unreadable)
			continue
		}
		if len(s.reasons) == 0 {
			t.Errorf("%s: %s builds an Issue kind from a runtime reason but tests no .Reason "+
				"literal this walk can read, so any reason at all could reach a report; "+
				"restore a readable guard or teach the walk this one", s.file, s.fn)
			continue
		}
		for _, r := range s.reasons {
			if kind := s.prefix + r; !documented[kind] {
				t.Errorf("%s: %s admits %q, which its Issue: field would emit, "+
					"and it is not in internal/knownissues", s.file, s.fn, kind)
			}
		}
	}
}

// issueSite is one Issue field whose value is built from a runtime reason.
// prefix is the constant it composes with — "" for a bare w.Reason, "Init:" for
// "Init:" + w.Reason — and reasons is every .Reason literal the enclosing
// function tests, sorted. unreadable is set instead when the value is a shape
// the walk does not understand, and names that shape for the failure message.
type issueSite struct {
	file       string
	fn         string
	prefix     string
	reasons    []string
	unreadable string
}

// dynamicIssueSites returns one entry per non-literal Issue field in the
// package's non-test sources. A site with a fully static kind is left to
// TestEveryIssueLiteralIsDocumented and is not returned here.
func dynamicIssueSites(t *testing.T) []issueSite {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)
	var out []issueSite
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			sites := dynamicIssueValues(fn)
			if len(sites) == 0 {
				continue
			}
			reasons, opaque := reasonLiterals(fn)
			for _, s := range sites {
				s.file, s.fn, s.reasons = path, fn.Name.Name, reasons
				if s.unreadable == "" && opaque != "" {
					s.unreadable = "a reason guarded by " + opaque + " rather than a string literal"
				}
				out = append(out, s)
			}
		}
	}
	return out
}

// dynamicIssueValues returns one entry per Issue field in fn whose value is not
// a plain string literal, reading the two shapes the detectors use and marking
// every other shape unreadable rather than guessing at it. Both ways of setting
// the field are read, via issueValues.
func dynamicIssueValues(fn *ast.FuncDecl) []issueSite {
	var out []issueSite
	ast.Inspect(fn, func(n ast.Node) bool {
		values, unreadable := issueValues(n)
		if unreadable != "" {
			out = append(out, issueSite{unreadable: unreadable})
		}
		for _, value := range values {
			switch v := value.(type) {
			case *ast.BasicLit:
				// A fully static kind; TestEveryIssueLiteralIsDocumented owns it.
			case *ast.SelectorExpr:
				if v.Sel.Name != "Reason" {
					out = append(out, issueSite{unreadable: "a ." + v.Sel.Name + " field"})
					break
				}
				out = append(out, issueSite{})
			case *ast.BinaryExpr:
				prefix := leadingStringLit(v.X)
				sel, ok := v.Y.(*ast.SelectorExpr)
				if v.Op != token.ADD || prefix == "" || !ok || sel.Sel.Name != "Reason" {
					out = append(out, issueSite{unreadable: "an expression other than a literal prefix and a .Reason field"})
					break
				}
				out = append(out, issueSite{prefix: prefix})
			default:
				out = append(out, issueSite{unreadable: "neither a .Reason field nor a literal prefix added to one"})
			}
		}
		return true
	})
	return out
}

// reasonLiterals returns every string literal fn tests a .Reason field against,
// sorted and deduplicated, and separately the first opaque operand it saw one
// tested against. Two shapes are read: an == comparison, whichever side the
// literal is written on, and a switch whose tag is a .Reason field.
//
// The opaque return is what stops a partial read from passing as a whole one.
// A guard like w.Reason == "ImagePullBackOff" || w.Reason == reasonBadImage
// yields one readable literal, and reporting only that would silently bless
// whatever the named constant holds. Any operand this walk cannot resolve to a
// string literal — a constant, a variable, a range value, a call — makes the
// whole site unreadable instead. Any other way of testing a reason yields no
// literals at all, which the caller also treats as a failure.
func reasonLiterals(fn *ast.FuncDecl) (lits []string, opaque string) {
	seen := map[string]bool{}
	add := func(e ast.Expr) {
		if lit := leadingStringLit(e); lit != "" {
			seen[lit] = true
			return
		}
		if opaque == "" {
			opaque = describeExpr(e)
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if v.Op != token.EQL {
				return true
			}
			for _, pair := range [][2]ast.Expr{{v.X, v.Y}, {v.Y, v.X}} {
				if sel, ok := pair[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Reason" {
					add(pair[1])
				}
			}
		case *ast.SwitchStmt:
			sel, ok := v.Tag.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Reason" {
				return true
			}
			for _, stmt := range v.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, e := range cc.List {
					add(e)
				}
			}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for lit := range seen {
		out = append(out, lit)
	}
	sort.Strings(out)
	return out, opaque
}

// describeExpr names an expression shape for a failure message, without
// pretending to know what it evaluates to.
func describeExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return "the name " + v.Name
	case *ast.SelectorExpr:
		return "the field or name ." + v.Sel.Name
	case *ast.CallExpr:
		return "a call"
	default:
		return "a computed expression"
	}
}
