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

// issueLiterals collects every string literal assigned to an Issue field in a
// composite literal in this package's non-test files, including the left
// operand of a concatenation.
func issueLiterals(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
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
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Issue" {
				return true
			}
			if lit := leadingStringLit(kv.Value); lit != "" {
				out = append(out, lit)
			}
			return true
		})
	}
	return out
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
