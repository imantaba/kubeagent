package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/policy"
)

const validPolicy = `- id: registry-allowlist
  level: critical
  message: image is not from an allowed registry
  match:
    kind: Pod
  assert:
    path: spec.containers[*].image
    op: matches
    values: ["registry.example.com/*"]
`

const secondPolicy = `- id: zone-label
  level: info
  message: no topology label
  match:
    kind: Node
  assert:
    path: metadata.labels["topology.kubernetes.io/zone"]
    op: exists
`

// duplicateRegistryAllowlist reuses validPolicy's rule id from a different
// file, on purpose, so a test can drive a cross-file duplicate through the
// CLI wiring rather than through internal/policy directly.
const duplicateRegistryAllowlist = `- id: registry-allowlist
  level: info
  message: a second, unrelated rule that happens to reuse the id
  match:
    kind: Node
  assert:
    path: metadata.labels["topology.kubernetes.io/zone"]
    op: exists
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyValidateReportsRulesAndKinds(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.yaml", validPolicy)
	b := writeFile(t, dir, "b.yaml", secondPolicy)

	var out bytes.Buffer
	if err := runPolicyValidate([]string{a, b}, &out); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "2 rules") || !strings.Contains(got, "2 kinds") {
		t.Errorf("output = %q, want a rule and kind count", got)
	}
}

// A count is all stdout gets. The path stays on stderr, where Main puts a
// returned error, because a filesystem path names the machine kubeagent ran on.
func TestPolicyValidateStdoutCarriesNoPath(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "rules.yaml", validPolicy)

	var out bytes.Buffer
	if err := runPolicyValidate([]string{p}, &out); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{dir, "rules.yaml", ".yaml", "/"} {
		if strings.Contains(out.String(), needle) {
			t.Errorf("stdout contains %q:\n%s", needle, out.String())
		}
	}
}

func TestPolicyValidateRejectsABadFileAndNamesIt(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "broken.yaml", "- id: no-level\n  match:\n    kind: Pod\n")

	var out bytes.Buffer
	err := runPolicyValidate([]string{p}, &out)
	if err == nil {
		t.Fatal("a policy with no level validated")
	}
	// The error is stderr-bound, so it may name the file — and must, or the
	// operator cannot tell which of five files is wrong.
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("error = %v, want the offending file named", err)
	}
	if out.Len() != 0 {
		t.Errorf("a failed validation printed to stdout: %q", out.String())
	}
}

func TestPolicyValidateWithNoArgumentsIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	err := runPolicyValidate(nil, &out)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

func TestPolicyDocumentsReadsADirectoryInNameOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.yaml", secondPolicy)
	writeFile(t, dir, "a.yml", validPolicy)
	writeFile(t, dir, "notes.txt", "not a policy")

	docs, err := policyDocuments([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2 (the .txt must be skipped)", len(docs))
	}
	if filepath.Base(docs[0].Source) != "a.yml" || filepath.Base(docs[1].Source) != "b.yaml" {
		t.Errorf("documents out of name order: %q, %q", docs[0].Source, docs[1].Source)
	}
}

// A directory with nothing in it is far more likely a wrong path than an
// operator asking for zero rules, and silently evaluating nothing is the
// failure mode this whole sub-project exists to prevent.
func TestPolicyDocumentsRejectsADirectoryWithNoPolicyFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "notes.txt", "not a policy")

	if _, err := policyDocuments([]string{dir}); err == nil {
		t.Fatal("an empty policy directory loaded without error")
	}
}

// A named file is taken at its word whatever it is called: the operator typed
// the name, so kubeagent does not second-guess the extension.
func TestPolicyDocumentsAcceptsANamedFileWithAnyExtension(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "rules.policy", validPolicy)

	docs, err := policyDocuments([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
}

func TestPolicyDocumentsNamesAMissingPath(t *testing.T) {
	_, err := policyDocuments([]string{filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the missing path named", err)
	}
}

// policy validate's only entry point is a positional file argument — there is
// no --policy flag until Task 15 — so its error must name the file the
// operator typed and never a flag they did not.
func TestPolicyValidateErrorNamesTheFileNotAFlag(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")

	var out bytes.Buffer
	err := runPolicyValidate([]string{missing}, &out)
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the missing file named", err)
	}
	if strings.Contains(err.Error(), "--policy") {
		t.Errorf("err = %v, names a flag that does not exist in this binary yet", err)
	}
}

// policyDocuments is the surface Task 15's --policy flag calls, so its errors
// must keep the --policy wording even though runPolicyValidate's must not.
func TestPolicyDocumentsErrorNamesTheFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := policyDocuments([]string{missing})
	if err == nil || !strings.Contains(err.Error(), "--policy "+missing) {
		t.Fatalf("err = %v, want %q named alongside the flag", err, missing)
	}
}

// policy.Load takes every document in one call so a duplicate rule id is
// caught across files, not just within one. That property depends on this
// CLI's wiring passing every document through together, and nothing above
// drove it through runPolicyValidate directly.
func TestPolicyValidateRejectsADuplicateRuleIDAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.yaml", validPolicy)
	b := writeFile(t, dir, "b.yaml", duplicateRegistryAllowlist)

	var out bytes.Buffer
	err := runPolicyValidate([]string{a, b}, &out)
	if err == nil || !strings.Contains(err.Error(), "registry-allowlist") {
		t.Fatalf("err = %v, want the duplicate rule id named", err)
	}
	if out.Len() != 0 {
		t.Errorf("a failed validation printed to stdout: %q", out.String())
	}
}

func TestEvaluatePolicyWithNoPathsReturnsNil(t *testing.T) {
	got, err := evaluatePolicy(context.Background(), nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %#v, want nil so no section renders and no JSON key appears", got)
	}
}

// A file that will not load must stop the command. A scan that printed a
// report with no policy section would read as "your rules are satisfied".
func TestEvaluatePolicyFailsOnABadFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "broken.yaml", "- id: no-level\n  match:\n    kind: Pod\n")

	if _, err := evaluatePolicy(context.Background(), []string{p}, "", "", ""); err == nil {
		t.Fatal("a bad policy file did not stop the command")
	}
}

func TestPolicyPacksListsWhatShips(t *testing.T) {
	var buf bytes.Buffer
	if err := runPolicyPacks(nil, "", &buf); err != nil {
		t.Fatalf("runPolicyPacks: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "reliability") {
		t.Errorf("the listing does not name the reliability pack:\n%s", out)
	}
	// The count comes from loading, so it cannot drift from the file.
	if !strings.Contains(out, "14 rules") {
		t.Errorf("the listing does not carry a rule count:\n%s", out)
	}
	if !strings.Contains(out, "kubeagent policy packs --print") {
		t.Errorf("the listing does not say how to print one:\n%s", out)
	}
}

func TestPolicyPacksPrintEmitsLoadableYAML(t *testing.T) {
	var buf bytes.Buffer
	if err := runPolicyPacks(nil, "reliability", &buf); err != nil {
		t.Fatalf("runPolicyPacks --print: %v", err)
	}
	// What is printed must be what the flag would run: load it back.
	rules, err := policy.Load([]policy.Document{{Source: "stdin", Data: buf.Bytes()}})
	if err != nil {
		t.Fatalf("the printed pack does not load: %v", err)
	}
	if len(rules) != 14 {
		t.Errorf("printed pack has %d rules, want 14", len(rules))
	}
}

func TestPolicyPacksPrintUnknownNameIsRefused(t *testing.T) {
	var buf bytes.Buffer
	err := runPolicyPacks(nil, "no-such-pack", &buf)
	if err == nil {
		t.Fatal("an unknown pack name was accepted")
	}
	if !strings.Contains(err.Error(), `"no-such-pack"`) {
		t.Errorf("the error does not quote the name given: %v", err)
	}
	if !strings.Contains(err.Error(), "reliability") {
		t.Errorf("the error does not name the packs that do exist: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused name still wrote to stdout: %q", buf.String())
	}
}

func TestPolicyPacksRefusesPositionalArguments(t *testing.T) {
	var buf bytes.Buffer
	err := runPolicyPacks([]string{"reliability"}, "", &buf)
	if err == nil {
		t.Fatal("a positional argument was accepted; the pack name goes to --print")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("the error is not a usage error: %v", err)
	}
}

func TestPackDocumentsCarryNoFilesystemPath(t *testing.T) {
	docs, err := packDocuments([]string{"reliability"})
	if err != nil {
		t.Fatalf("packDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("packDocuments returned %d documents, want 1", len(docs))
	}
	if docs[0].Source != "pack:reliability" {
		t.Errorf("Source = %q, want %q — a pack has no path, so none may leak", docs[0].Source, "pack:reliability")
	}
}

func TestPackDocumentsRefusesAnUnknownName(t *testing.T) {
	if _, err := packDocuments([]string{"no-such-pack"}); err == nil {
		t.Fatal("an unknown pack name was accepted")
	}
}

// policy.Load treats empty or nil YAML as a valid, empty document rather
// than an error, so nothing downstream of requirePackBytes would ever catch
// a broken embed on its own. This drives the guard directly with empty
// bytes, since no shipped pack is ever actually empty: there is no way to
// reach this branch through packDocuments or runPolicyPacks against the
// real embedded registry.
func TestRequirePackBytesRefusesEmptyBytes(t *testing.T) {
	_, err := requirePackBytes("reliability", nil)
	if err == nil {
		t.Fatal("empty pack bytes were accepted")
	}
	if !strings.Contains(err.Error(), `"reliability"`) {
		t.Errorf("the error does not name the pack: %v", err)
	}
}

func TestRequirePackBytesAcceptsNonEmptyBytes(t *testing.T) {
	data, err := requirePackBytes("reliability", []byte("- id: x\n"))
	if err != nil {
		t.Fatalf("requirePackBytes: %v", err)
	}
	if string(data) != "- id: x\n" {
		t.Errorf("requirePackBytes changed the bytes: got %q", data)
	}
}

// The two surfaces that can miss a pack name — packDocuments and
// runPolicyPacks --print — must describe the same miss the same way, so a
// future edit to one cannot quietly drift from the other.
func TestUnknownPackErrorIsIdenticalAcrossBothSurfaces(t *testing.T) {
	_, docErr := packDocuments([]string{"no-such-pack"})
	var buf bytes.Buffer
	printErr := runPolicyPacks(nil, "no-such-pack", &buf)
	if docErr == nil || printErr == nil {
		t.Fatal("expected both surfaces to refuse the unknown name")
	}
	if docErr.Error() != printErr.Error() {
		t.Errorf("packDocuments error = %q, runPolicyPacks --print error = %q, want identical",
			docErr.Error(), printErr.Error())
	}
}
