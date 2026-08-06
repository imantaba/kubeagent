package schemadoc

import (
	"bytes"
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/jsonschema"
)

var update = flag.Bool("update", false, "rewrite the committed schema files")

// repoPath resolves a repository-relative path from this package's directory.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "..", rel)
}

func TestDocumentsTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Documents {
		if d.Name == "" || d.Surface == "" || d.Version == "" || d.Title == "" || d.Description == "" {
			t.Errorf("%+v has an empty field", d)
		}
		if d.Root == nil || d.Root.Kind() != reflect.Struct {
			t.Errorf("%s: root is not a struct type", d.Name)
		}
		if seen[d.Name] {
			t.Errorf("%s: duplicate document name", d.Name)
		}
		seen[d.Name] = true
		if _, err := jsonschema.Major(d.Version); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
	}
	if len(Documents) != 7 {
		t.Errorf("Documents has %d entries, want the seven documented surfaces", len(Documents))
	}
}

// TestBaselineSchemaVersionMatches pins internal/baseline's own SchemaVersion
// constant to the one internal/jsonschema publishes. internal/baseline imports
// nothing from kubeagent, so it cannot reference jsonschema.BaselineVersion
// directly; this is where the two spellings are held together. Every other
// surface sets its schemaVersion from jsonschema and needs no such test.
func TestBaselineSchemaVersionMatches(t *testing.T) {
	if baseline.SchemaVersion != jsonschema.BaselineVersion {
		t.Errorf("baseline.SchemaVersion = %q, jsonschema.BaselineVersion = %q — they must agree",
			baseline.SchemaVersion, jsonschema.BaselineVersion)
	}
}

func TestEveryDocumentDeclaresASchemaVersion(t *testing.T) {
	for _, d := range Documents {
		f, ok := d.Root.FieldByName("SchemaVersion")
		if !ok {
			t.Errorf("%s: root %s has no SchemaVersion field", d.Name, d.Root)
			continue
		}
		if got := f.Tag.Get("json"); got != "schemaVersion" {
			t.Errorf("%s: SchemaVersion json tag = %q, want schemaVersion", d.Name, got)
		}
	}
}

func TestGenerateUnknownNameNamesTheValidOnes(t *testing.T) {
	_, err := Generate("nope")
	if err == nil {
		t.Fatal("want an error for an unknown document name")
	}
	for _, n := range Names() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("error %q does not mention %q", err, n)
		}
	}
}

// TestSchemaDrift is the contract's enforcement: the committed documents are
// exactly what the current types generate, and a mismatch says which kind of
// change happened in the terms the version contract uses.
//
// Regenerate deliberately:
//
//	go test ./internal/schemadoc -run TestSchemaDrift -update
func TestSchemaDrift(t *testing.T) {
	for _, d := range Documents {
		rel, err := Path(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		path := repoPath(t, rel)
		got, err := Generate(d.Name)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		if *update {
			if problem := updateSchema(t, path, got); problem != "" {
				t.Errorf("%s (%s v%s): refusing to overwrite %s in place:\n%s", d.Name, d.Surface, d.Version, rel, problem)
				continue
			}
			t.Logf("wrote %s", rel)
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v\nregenerate with: go test ./internal/schemadoc -run TestSchemaDrift -update", d.Name, err)
			continue
		}
		if bytes.Equal(got, want) {
			continue
		}
		t.Errorf("%s (%s v%s) has drifted from %s:\n%s", d.Name, d.Surface, d.Version, rel, classify(t, want, got))
	}
}

// updateSchema applies -update semantics to one document at path: got
// overwrites whatever is committed there. Returns "" once the write lands, or
// classify()'s report — unwritten — when it refuses.
//
// This is deliberately the only place -update touches disk, so the refusal
// added for finding 2 (a breaking change must not silently become an
// in-place edit) lives in one function that both TestSchemaDrift and the
// TestUpdate* tests below exercise, rather than being duplicated between the
// real driver and a parallel test-only copy of the same logic.
func updateSchema(t *testing.T, path string, got []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("%v", err)
	}
	want, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Something is already committed at path: refuse to let a breaking
		// change land as if it were the in-place MINOR edit the same URL
		// promises. A MAJOR change moves to a new path instead (Path(d)
		// changes once the surface's version constant is bumped), which
		// falls into the case below because nothing is committed there yet.
		if problem := classify(t, want, got); strings.Contains(problem, "BREAKING") {
			return problem
		}
	case os.IsNotExist(err):
		// Nothing committed at path yet — a MAJOR bump publishing at a new
		// path, or the first time this document is generated. There is
		// nothing to compare against, so nothing here can be a breaking
		// in-place edit.
	default:
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(path, got, 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	return ""
}

// TestUpdateRefusesABreakingChangeInPlace is finding 2: -update must not
// silently turn a breaking change into an in-place edit of a published file.
// It operates on a file in t.TempDir(), not on anything under
// website/docs/schemas/, so it never touches a real committed schema.
func TestUpdateRefusesABreakingChangeInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-v1.json")
	committed := []byte(`{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]}` + "\n")
	if err := os.WriteFile(path, committed, 0o644); err != nil {
		t.Fatal(err)
	}
	// The mutation finding 2 reproduced live: ,omitempty added to
	// Verdict.Verdict drops it from required. Modeled here as the generator
	// output that same mutation would produce.
	breaking := []byte(`{"type":"object","properties":{"verdict":{"type":"string"}},"required":[]}` + "\n")

	problem := updateSchema(t, path, breaking)
	if problem == "" {
		t.Fatal("updateSchema() = \"\", want a refusal for a breaking change")
	}
	if !strings.Contains(problem, "BREAKING") {
		t.Errorf("problem = %q, want a BREAKING line", problem)
	}
	if !strings.Contains(problem, "MAJOR") {
		t.Errorf("problem = %q, want MAJOR-bump instructions", problem)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, committed) {
		t.Errorf("file at %s changed despite the refusal:\ngot  %s\nwant %s (unchanged)", path, got, committed)
	}
}

// TestUpdateWritesAnAdditiveChangeInPlace guards the other half of finding 2:
// the refusal must not also trap an additive change. -update still has to
// work for the ordinary case.
func TestUpdateWritesAnAdditiveChangeInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-v1.json")
	committed := []byte(`{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]}` + "\n")
	if err := os.WriteFile(path, committed, 0o644); err != nil {
		t.Fatal(err)
	}
	additive := []byte(`{"type":"object","properties":{"scope":{"type":"string"},"verdict":{"type":"string"}},"required":["verdict"]}` + "\n")

	if problem := updateSchema(t, path, additive); problem != "" {
		t.Fatalf("updateSchema() = %q, want no refusal for an additive change", problem)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, additive) {
		t.Errorf("file at %s:\ngot  %s\nwant %s", path, got, additive)
	}
}

// TestUpdateWritesANewFileWhenNoneIsCommitted is the escape hatch a MAJOR
// bump depends on: Path(d) names a file that does not exist yet once the
// surface's version constant is bumped, so there is nothing to compare
// against and nothing that can be "broken" — -update must still write it.
func TestUpdateWritesANewFileWhenNoneIsCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-v2.json")
	content := []byte(`{"type":"object","properties":{"verdict":{"type":"string"}}}` + "\n")

	if problem := updateSchema(t, path, content); problem != "" {
		t.Fatalf("updateSchema() = %q, want no refusal when nothing is committed at path yet", problem)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file at %s:\ngot  %s\nwant %s", path, got, content)
	}
}

// classify reports what moved between two schema documents, and whether that is
// a breaking or an additive change under the version contract.
func classify(t *testing.T, want, got []byte) string {
	t.Helper()
	a, b := flatten(t, want), flatten(t, got)
	var breaking, additive []string
	for k, av := range a {
		bv, present := b[k]
		switch {
		case !present && isMember(k):
			// A required entry or enum value disappeared.
			if strings.Contains(k, "/required#") {
				breaking = append(breaking, "no longer required: "+k)
			} else {
				breaking = append(breaking, "enum value removed: "+k)
			}
		case !present:
			breaking = append(breaking, "removed: "+k)
		case av != bv:
			breaking = append(breaking, fmt.Sprintf("changed: %s (%s → %s)", k, av, bv))
		}
	}
	for k := range b {
		if _, present := a[k]; present {
			continue
		}
		if inNewSubtree(k, a) {
			// The $defs entry containing k does not appear anywhere in the
			// committed document — no document captured against that
			// document could reach it, so nothing about its shape, however
			// it looks, can break an existing consumer. Applies uniformly to
			// every branch below, not just "newly required": the other two
			// are already additive, so this only changes their wording, not
			// their verdict.
			additive = append(additive, "added (new type): "+k)
			continue
		}
		switch {
		case strings.Contains(k, "/required#"):
			breaking = append(breaking, "newly required: "+k)
		case strings.Contains(k, "/enum#"):
			additive = append(additive, "enum value added: "+k)
		default:
			additive = append(additive, "added: "+k)
		}
	}
	sort.Strings(breaking)
	sort.Strings(additive)
	var b2 strings.Builder
	for _, line := range breaking {
		fmt.Fprintf(&b2, "  BREAKING  %s\n", line)
	}
	for _, line := range additive {
		fmt.Fprintf(&b2, "  additive  %s\n", line)
	}
	if len(breaking) > 0 {
		b2.WriteString("\nThis is a MAJOR change: bump the surface's version constant in internal/jsonschema, publish the new file beside the old one, and record it in the CHANGELOG under Changed.\n")
	} else {
		b2.WriteString("\nThis is a MINOR change: bump the surface's minor in internal/jsonschema, then regenerate:\n  go test ./internal/schemadoc -run TestSchemaDrift -update\n")
	}
	return b2.String()
}

func isMember(path string) bool {
	return strings.Contains(path, "/required#") || strings.Contains(path, "/enum#")
}

// inNewSubtree reports whether path's containing $defs type is absent from a
// (the committed/old document) entirely. $defs is flat — every named struct
// type in a generated schema is exactly one $defs entry, however deep the
// type is reached through refs — so a $defs entry with no presence at all in
// a is a subtree no old document could ever reach: nothing in the old
// document set holds a value shaped by a type its schema never mentioned.
// That makes any difference found only inside it additive, whatever its
// shape (finding 1: a new optional field whose type has its own required
// subfields must not read as a MAJOR change). A path that is not under
// $defs — a root-level property, say — is never "new" this way: both sides
// of a diff describe the same root type, so the root itself always exists.
func inNewSubtree(path string, a map[string]string) bool {
	c := defsContainer(path)
	if c == "" {
		return false
	}
	prefix := c + "/"
	for k := range a {
		if strings.HasPrefix(k, prefix) {
			return false
		}
	}
	return true
}

// defsContainer returns the "/$defs/<Type>" prefix a path lives under, or ""
// if path is not under $defs at all.
func defsContainer(path string) string {
	const prefix = "/$defs/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := path[len(prefix):]
	if i := strings.Index(rest, "/"); i >= 0 {
		return prefix + rest[:i]
	}
	return path
}

// TestClassify is a direct unit test over classify() itself, isolated from the
// six real documents: each case flips exactly one thing between two minimal
// schema bodies and checks which side of the version contract classify() puts
// it on. TestSchemaDrift only ever exercises classify() through whatever the
// current types happen to drift by, so a polarity bug in one branch can hide
// for a long time if nobody happens to make that exact kind of change. This
// test makes every branch reachable on demand.
func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		want    string // the committed ("old") document
		got     string // the freshly generated ("new") document
		verdict string // "breaking" or "additive"
	}{
		{
			name:    "required field removed",
			want:    `{"type":"object","properties":{"foo":{"type":"string"}},"required":["foo"]}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string"}},"required":[]}`,
			verdict: "breaking",
		},
		{
			name:    "required field added",
			want:    `{"type":"object","properties":{"foo":{"type":"string"}},"required":[]}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string"}},"required":["foo"]}`,
			verdict: "breaking",
		},
		{
			name:    "optional field added",
			want:    `{"type":"object","properties":{"foo":{"type":"string"}}}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string"},"bar":{"type":"string"}}}`,
			verdict: "additive",
		},
		{
			name:    "field type changed",
			want:    `{"type":"object","properties":{"foo":{"type":"string"}}}`,
			got:     `{"type":"object","properties":{"foo":{"type":"integer"}}}`,
			verdict: "breaking",
		},
		{
			name:    "field removed entirely",
			want:    `{"type":"object","properties":{"foo":{"type":"string"},"bar":{"type":"string"}}}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string"}}}`,
			verdict: "breaking",
		},
		{
			name:    "enum value removed",
			want:    `{"type":"object","properties":{"foo":{"type":"string","enum":["a","b"]}}}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string","enum":["a"]}}}`,
			verdict: "breaking",
		},
		{
			name:    "enum value added",
			want:    `{"type":"object","properties":{"foo":{"type":"string","enum":["a"]}}}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string","enum":["a","b"]}}}`,
			verdict: "additive",
		},
		{
			// A brand-new optional field whose type is a brand-new struct with
			// its own required subfield. No old document reaches
			// $defs/gate.ProbeInfo at all — it doesn't exist in want — so
			// nothing about its shape, however it looks, can break an old
			// consumer. This is the false MAJOR from finding 1.
			name:    "required field added inside a brand-new $defs type reached only by a new optional field",
			want:    `{"type":"object","properties":{"foo":{"type":"string"}}}`,
			got:     `{"type":"object","properties":{"foo":{"type":"string"},"probe":{"$ref":"#/$defs/gate.ProbeInfo"}},"$defs":{"gate.ProbeInfo":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}}}`,
			verdict: "additive",
		},
		{
			// Same shape of diff, but $defs/gate.ProbeInfo already existed in
			// want: a document captured before this edit could hold ProbeInfo
			// without "x" and would now fail to validate. Must stay breaking.
			name:    "required field added inside an existing $defs type",
			want:    `{"type":"object","properties":{"foo":{"$ref":"#/$defs/gate.ProbeInfo"}},"$defs":{"gate.ProbeInfo":{"type":"object","properties":{"x":{"type":"string"}}}}}`,
			got:     `{"type":"object","properties":{"foo":{"$ref":"#/$defs/gate.ProbeInfo"}},"$defs":{"gate.ProbeInfo":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}}}`,
			verdict: "breaking",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := classify(t, []byte(tt.want), []byte(tt.got))
			hasBreaking := strings.Contains(out, "BREAKING")
			switch tt.verdict {
			case "breaking":
				if !hasBreaking {
					t.Errorf("classify() = %q, want a BREAKING line", out)
				}
				if !strings.Contains(out, "This is a MAJOR change") {
					t.Errorf("classify() = %q, want MAJOR-bump instructions", out)
				}
			case "additive":
				if hasBreaking {
					t.Errorf("classify() = %q, want no BREAKING line", out)
				}
				if !strings.Contains(out, "This is a MINOR change") {
					t.Errorf("classify() = %q, want MINOR-bump instructions", out)
				}
			default:
				t.Fatalf("bad test case: verdict %q is neither breaking nor additive", tt.verdict)
			}
		})
	}
}

// flatten reduces a document to "path = value" lines so a diff names what moved
// instead of printing two JSON blobs. required and enum arrays flatten to one
// membership line per element, because their order carries no meaning and an
// index-based diff would report every element after an insertion as changed.
func flatten(t *testing.T, doc []byte) map[string]string {
	t.Helper()
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatalf("flatten: %v", err)
	}
	out := map[string]string{}
	walkJSON("", v, out)
	return out
}

func walkJSON(path string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkJSON(path+"/"+k, x[k], out)
		}
	case []any:
		if base := path[strings.LastIndex(path, "/")+1:]; base == "required" || base == "enum" {
			for _, e := range x {
				out[fmt.Sprintf("%s#%v", path, e)] = "present"
			}
			return
		}
		for i, e := range x {
			walkJSON(fmt.Sprintf("%s[%d]", path, i), e, out)
		}
	default:
		out[path] = fmt.Sprintf("%v", v)
	}
}

// walkTypes visits every type reachable from root through the fields
// encoding/json emits — the same reachability the generator walks.
func walkTypes(t reflect.Type, seen map[reflect.Type]bool, visit func(reflect.Type)) {
	if seen[t] {
		return
	}
	seen[t] = true
	visit(t)
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		walkTypes(t.Elem(), seen, visit)
	case reflect.Map:
		walkTypes(t.Key(), seen, visit)
		walkTypes(t.Elem(), seen, visit)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" || f.Tag.Get("json") == "-" {
				continue
			}
			walkTypes(f.Type, seen, visit)
		}
	}
}

// eachDocumentType visits every type in every document, once per document so a
// failure can name which one.
func eachDocumentType(visit func(d Document, t reflect.Type)) {
	for _, d := range Documents {
		seen := map[reflect.Type]bool{}
		walkTypes(d.Root, seen, func(t reflect.Type) { visit(d, t) })
	}
}

func named(t reflect.Type) bool { return t.Name() != "" && t.PkgPath() != "" }

// A type with a custom marshaler emits JSON its Go kind does not describe.
// Reflection cannot see that, so every such type needs an override — and the
// next one added must fail here rather than ship a schema that is simply wrong.
func TestEveryCustomMarshalerHasAnOverride(t *testing.T) {
	jm := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	tm := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	eachDocumentType(func(d Document, ty reflect.Type) {
		if !named(ty) {
			return
		}
		custom := ty.Implements(jm) || reflect.PointerTo(ty).Implements(jm) ||
			ty.Implements(tm) || reflect.PointerTo(ty).Implements(tm)
		if !custom {
			return
		}
		if _, ok := overrides[jsonschema.TypeKey(ty)]; !ok {
			t.Errorf("%s: %s has a custom marshaler but no entry in overrides — reflection would document its Go kind, not its JSON", d.Name, jsonschema.TypeKey(ty))
		}
	})
}

// encoding/json ignores omitempty on a plain (non-pointer) struct-kind field:
// such a field marshals every time, zero value or not. internal/jsonschema's
// generator has no way to know that — it takes the tag at its word — so a
// plain struct field tagged omitempty would publish as optional in the schema
// while the real output always has it. That understates what kubeagent
// promises: a consumer reading the schema would not know it can rely on the
// field always being there. The fix belongs here, not in the generator: this
// package already carries the other cases reflection cannot see for itself.
func TestNoOmitemptyOnAPlainStructField(t *testing.T) {
	eachDocumentType(func(d Document, ty reflect.Type) {
		if ty.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			if f.PkgPath != "" || f.Tag.Get("json") == "-" {
				continue
			}
			if f.Type.Kind() != reflect.Struct {
				continue
			}
			if !hasJSONOmitempty(f.Tag.Get("json")) {
				continue
			}
			t.Errorf("%s: %s.%s is a plain (non-pointer) struct field tagged omitempty. encoding/json ignores omitempty there, so the field always marshals — remove the tag, or make the field a pointer so omitempty means what it says.", d.Name, jsonschema.TypeKey(ty), f.Name)
		}
	})
}

// hasJSONOmitempty walks a json tag's comma-separated options looking for
// omitempty, the same way internal/jsonschema's own (unexported) check does,
// so an option that merely contains the substring cannot false-positive.
func hasJSONOmitempty(tag string) bool {
	_, opts, _ := strings.Cut(tag, ",")
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == "omitempty" {
			return true
		}
	}
	return false
}

// kubeagent must not freeze a type it does not own: a field added upstream in a
// future Kubernetes release would trip the drift test as a breaking change
// nobody could fix, and would silently widen what kubeagent has promised.
func TestNoForeignTypesInAnyDocument(t *testing.T) {
	eachDocumentType(func(d Document, ty reflect.Type) {
		p := ty.PkgPath()
		if p == "" || strings.HasPrefix(p, "github.com/imantaba/kubeagent/") || stdlib(p) {
			return
		}
		t.Errorf("%s: %s.%s is not kubeagent's type. Either project it into a type this repo owns, or describe it and accept that its shape is not kubeagent's to promise.", d.Name, p, ty.Name())
	})
}

// stdlib reports whether an import path is in the standard library: its first
// segment has no dot, so it is not a domain.
func stdlib(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".")
}

// An un-enumerated enum is the failure mode that matters: a consumer switching
// on a value it was never told about.
func TestEveryNamedStringTypeIsEnumeratedOrFreeForm(t *testing.T) {
	eachDocumentType(func(d Document, ty reflect.Type) {
		if ty.Kind() != reflect.String || !named(ty) {
			return
		}
		key := jsonschema.TypeKey(ty)
		if _, ok := enums[key]; ok {
			return
		}
		if freeFormStrings[key] {
			return
		}
		t.Errorf("%s: %s is a named string type with no enum entry. Add its constants to enums, or add it to freeFormStrings to say a consumer must not switch on the value.", d.Name, key)
	})
}

// Every override must describe a type that is actually in a document, or the
// table is carrying a stale entry that hides a real drift.
func TestNoStaleTableEntries(t *testing.T) {
	present := map[string]bool{}
	eachDocumentType(func(_ Document, ty reflect.Type) {
		if named(ty) {
			present[jsonschema.TypeKey(ty)] = true
		}
	})
	for key := range overrides {
		if !present[key] {
			t.Errorf("overrides has %q, which appears in no document", key)
		}
	}
	for key := range enums {
		if !present[key] {
			t.Errorf("enums has %q, which appears in no document", key)
		}
	}
	for key := range freeFormStrings {
		if !present[key] {
			t.Errorf("freeFormStrings has %q, which appears in no document", key)
		}
	}
}

// The published $id must resolve to the file the document is committed at, or a
// consumer following the $id gets a 404.
func TestPublishedIDMatchesTheCommittedPath(t *testing.T) {
	for _, d := range Documents {
		raw, err := Generate(d.Name)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		var doc struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		rel, err := Path(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		want := "https://k8sproject.top/" + strings.TrimPrefix(rel, "website/docs/")
		if doc.ID != want {
			t.Errorf("%s: $id = %q, want %q", d.Name, doc.ID, want)
		}
	}
}
