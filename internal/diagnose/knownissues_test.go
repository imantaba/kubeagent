package diagnose

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
		// ContainerStartError — the zero CreationTimestamp is older than the
		// dwell measured against rlNow, so this fires without a restart count.
		{Pod: podWaiting("example-ns", "web-15", "app", "RunContainerError",
			"failed to start container")},
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
		// Init:CreateContainerConfigError
		{Pod: podWithInit("example-ns", "web-13", corev1.ContainerStatus{
			Name: "setup",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: `configmap "app-config" not found`}},
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
		// VolumeMountError
		{Pod: podCreating("example-ns", "web-14"), Events: []corev1.Event{
			mountEvent("example-ns", "web-14",
				`MountVolume.SetUp failed for volume "config" : configmap "app-config" not found`),
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
// The same rule governs where the field is set, not only what it is set to.
// A key in a composite literal and an assignment to the field are both read
// (issueValues); a compound or multi-value assignment is refused; and a Finding
// written positionally, with no field names at all, is refused too
// (unkeyedFindingLiteral) because it names no field for either walk to match.
// Each of those was invisible rather than refused at some point in this test's
// history — a category the walk never saw, which the other three tests cannot
// signal because they stay green. That is the failure this design exists to
// prevent, and it is why a new shape is refused until it is taught.
//
// One boundary is not a shape at all: this walk reads syntax, so a package that
// writes a field without the writer naming it — reflect, unsafe, encoding/json,
// any decoder — would leave nothing to read. That is why the import set is
// pinned rather than filtered (unpinnedImports), and it is what lets the
// paragraph above speak for the whole package rather than for the part of it
// written plainly.
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

	for _, use := range unpinnedImports(t) {
		t.Errorf("%s, which the detectors' pinned import set does not list; an import "+
			"can set a field with no syntax for this walk to read, as encoding/json "+
			"does, or hand back a Finding built where this walk cannot see it, so the "+
			"set is widened deliberately or not at all", use)
	}

	for _, use := range unclassifiedIssueIdents(t) {
		t.Errorf("%s, and the walk below accounts for neither; it reads an Issue field "+
			"named as a composite-literal key or assigned to, inside a function "+
			"declaration, and refuses anything else rather than passing it over", use)
	}

	for _, decl := range findingAliasDecls(t) {
		t.Errorf("%s is a second name for a %s, so a positional literal of it names "+
			"neither the type nor the field for any check here to match; the package "+
			"needs no second name for it", decl, findingType)
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
		if reason := unkeyedFindingLiteral(n); reason != "" {
			out = append(out, issueSite{unreadable: reason})
		}
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

// findingType is the struct whose Issue field carries a kind into a report.
const findingType = "Finding"

// issueField is the field name every check in this file is about.
const issueField = "Issue"

// unclassifiedIssueIdents returns "file:line: <what it is>" for every place the
// package's non-test sources write the identifier Issue in a position none of
// the shapes below accounts for.
//
// This is the check that closes the class rather than another shape. The walks
// below enumerate the ways a value can be put into the field, and Go keeps
// offering more of them than an enumeration holds: a range assignment,
// for f.Issue = range c, and the address of the field handed to a helper,
// setIssue(&f.Issue, w.Reason), are both writes that no case matched — skipped
// rather than refused, which is the one outcome this design does not tolerate.
//
// So the question is asked from the other side. A value can reach the field in
// exactly three ways: the field is named, the field is not named (a positional
// literal, unkeyedFindingLiteral), or syntax is bypassed altogether
// (unpinnedImports). This function owns the first: wherever the field is
// named, the occurrence must sit in a position the walk reads — a key in a
// composite literal, or the left-hand side of an assignment — inside a function
// declaration, where reasonLiterals can see the guards. Every other position is
// named and refused, whether it is a write this walk has never met or an
// ordinary read someone added.
//
// The one exception is the field's own declaration in the Finding struct. A
// second type declaring an Issue field is not exempt: see
// unreadableIssuePosition, and findingAliasDecls for the other half of that
// argument.
func unclassifiedIssueIdents(t *testing.T) []string {
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
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if id, ok := n.(*ast.Ident); ok && id.Name == issueField {
				if why := unreadableIssuePosition(id, stack); why != "" {
					out = append(out, fmt.Sprintf("%s: %s", fset.Position(id.Pos()), why))
				}
			}
			stack = append(stack, n)
			return true
		})
	}
	return out
}

// unreadableIssuePosition names why the identifier id, whose ancestors are
// stack outermost-first, sits somewhere the walk does not read — or returns ""
// when it sits somewhere the walk does.
func unreadableIssuePosition(id *ast.Ident, stack []ast.Node) string {
	if len(stack) == 0 {
		return "the name Issue stands alone"
	}
	parent := stack[len(stack)-1]

	// The field's own declaration, in the Finding struct and nowhere else. A
	// second type that declares an Issue field is refused rather than exempted:
	// Go converts between struct types whose field names and types match, so
	// Finding(other{pod, w.Reason, …}) would carry a kind into the field with no
	// Issue token at the site that sets it for anything here to match.
	if field, ok := parent.(*ast.Field); ok {
		for _, name := range field.Names {
			if name != id {
				continue
			}
			if declaresFinding(stack) {
				return ""
			}
			return "the name Issue declared somewhere other than the " + findingType + " struct"
		}
	}

	inFuncDecl := false
	for _, n := range stack {
		if _, ok := n.(*ast.FuncDecl); ok {
			inFuncDecl = true
			break
		}
	}
	outside := " outside a function declaration, where this walk cannot read the guards"

	switch p := parent.(type) {
	case *ast.KeyValueExpr:
		if p.Key != id {
			return "an Issue field used as a composite-literal value rather than a key"
		}
		if !inFuncDecl {
			return "an Issue field set" + outside
		}
		return ""
	case *ast.SelectorExpr:
		if p.Sel != id {
			return "an Issue field used to qualify another name"
		}
		if len(stack) < 2 {
			return "an Issue field selected with nothing enclosing it"
		}
		assign, ok := stack[len(stack)-2].(*ast.AssignStmt)
		if !ok {
			return "an Issue field reached by something other than an assignment to it"
		}
		for _, lhs := range assign.Lhs {
			if lhs == p {
				if !inFuncDecl {
					return "an Issue field assigned" + outside
				}
				return ""
			}
		}
		return "an Issue field read rather than assigned"
	}
	return "the name Issue in a position this walk does not read"
}

// declaresFinding reports whether stack, an ancestor chain outermost-first,
// passes through the package-level declaration of Finding itself.
//
// A type declared inside a function body can be spelled Finding too, and its
// own Issue field would then be exempt for no better reason than the spelling.
// So a function boundary crossed on the way down ends the search: only a
// TypeSpec reached from the file is the type this file is about.
func declaresFinding(stack []ast.Node) bool {
	for _, n := range stack {
		if _, ok := n.(*ast.FuncDecl); ok {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if spec, ok := n.(*ast.TypeSpec); ok && spec.Name != nil && spec.Name.Name == findingType {
			return true
		}
	}
	return false
}

// findingAliasDecls returns "file:line: name" for every type this package's
// non-test sources declare as a second name for Finding — an alias,
// type f = Finding, or a defined type, type f Finding.
//
// Either one gives a composite literal a type name that is not the word
// Finding, so a positional f{pod, w.Reason, …} sets the field with nothing for
// unkeyedFindingLiteral to recognise and no Issue token for
// unclassifiedIssueIdents to see. The package needs neither name, so their
// absence is asserted rather than assumed.
//
// That leaves one way to reach the field without naming either the type or the
// field: a conversion from a second struct of identical layout,
// Finding(other{…}). Go requires the field names to match, so that struct must
// declare an Issue field of its own, which unreadableIssuePosition refuses —
// and it must be declared here, because unpinnedImports refuses the import that
// would bring one in from elsewhere.
//
// Elsewhere is where this file's claim stops, and deliberately. internal/scan
// builds diagnose.Findings of its own in rollouthealth, createhealth and
// batchhealth, carrying RolloutStuck, FailedCreate and JobFailed — kinds
// internal/knownissues does not document and is not meant to. The vocabulary
// closed here is the vocabulary of internal/diagnose's own detectors, which is
// what the reference is about; see the honest boundary in
// website/docs/features/known-issues.md.
func findingAliasDecls(t *testing.T) []string {
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
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name == nil || spec.Name.Name == findingType {
				return true
			}
			if id, ok := spec.Type.(*ast.Ident); ok && id.Name == findingType {
				out = append(out, fmt.Sprintf("%s: %s", fset.Position(spec.Pos()), spec.Name.Name))
			}
			return true
		})
	}
	return out
}

// pinnedImports is every package internal/diagnose's non-test sources may
// import. Nine pure functions over API objects need this much and no more.
var pinnedImports = map[string]bool{
	"fmt":                true,
	"sort":               true,
	"strings":            true,
	"time":               true,
	"k8s.io/api/core/v1": true,

	"github.com/imantaba/kubeagent/internal/safetext": true,
}

// unpinnedImports returns one "file imports pkg" string per non-test file in
// this package that imports something pinnedImports does not list, sorted.
//
// Every check in this file reads syntax, so any package that can write a field
// without the writer naming it defeats all of them at once — not as a refused
// shape, but as nothing at all. reflect and unsafe are the obvious two, and
// banning those two is what this check used to do; it was wrong, and a review
// proved it with four lines that no test noticed:
//
//	var f Finding
//	json.Unmarshal([]byte(`{"issue":"Undocumented"}`), &f)
//
// encoding/json reflects on kubeagent's behalf. So does encoding/gob, and so
// does every decoder anyone might reach for next. Naming the packages that must
// not appear is the same open enumeration this file gave up on elsewhere, for
// the same reason: the language keeps offering more of them.
//
// So the set is pinned from the other side. These six are what the detectors
// use; a seventh fails the test until someone looks at it and widens the list on
// purpose. That closes the decoder class, and with it the second way an import
// could undermine this file — a package handing back a Finding it built
// somewhere this walk cannot see.
func unpinnedImports(t *testing.T) []string {
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
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", path, spec.Path.Value, err)
			}
			if !pinnedImports[p] {
				out = append(out, path+" imports "+p)
			}
		}
	}
	return out
}

// unkeyedFindingLiteral names the reason n is a Finding written positionally,
// or returns "" when n is anything else.
//
// Go lets a struct literal give its fields in declaration order with no names —
// &Finding{pod, kind, reason, evidence, nil, "", "", "", ""} — and `go vet` does
// not flag that for a type declared in the same package. Issue is the second
// field, so such a literal sets a kind with no Issue token anywhere for a parser
// to find: a third invisible category, alongside the assignment form, rather
// than a refused shape. It is refused here by name.
//
// Two forms are read: the literal itself, and a slice, array or map of Findings
// whose elements elide the type ([]Finding{{...}}), which is checked from the
// container because the elements alone no longer say what they are.
func unkeyedFindingLiteral(n ast.Node) string {
	lit, ok := n.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	const reason = "a Finding written positionally, without field names"
	if namesFinding(lit.Type) {
		if hasUnkeyedElement(lit) {
			return reason
		}
		return ""
	}
	var elem ast.Expr
	switch t := lit.Type.(type) {
	case *ast.ArrayType:
		elem = t.Elt
	case *ast.MapType:
		elem = t.Value
	}
	if !namesFinding(elem) {
		return ""
	}
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			e = kv.Value
		}
		if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil && hasUnkeyedElement(inner) {
			return reason
		}
	}
	return ""
}

// namesFinding reports whether e writes the Finding type, with or without a
// pointer star.
func namesFinding(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Name == findingType
}

// hasUnkeyedElement reports whether lit gives any field without naming it. An
// empty literal has none and is fine.
func hasUnkeyedElement(lit *ast.CompositeLit) bool {
	for _, e := range lit.Elts {
		if _, ok := e.(*ast.KeyValueExpr); !ok {
			return true
		}
	}
	return false
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
