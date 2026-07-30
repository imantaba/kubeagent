# Stable versioned JSON Schema Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every machine-readable JSON document kubeagent emits declares a
`schemaVersion`, publishes a generated JSON Schema, and fails a test when its
shape moves without a version bump.

**Architecture:** A dependency-free `internal/jsonschema` walks a `reflect.Type`
and renders JSON Schema draft 2020-12; the caller supplies the document's
identity plus the two tables reflection cannot derive (enums, custom
marshalers). A second package `internal/schemadoc` names the six document roots
in one table, which drives the committed schema files, the drift test and the
new `kubeagent schema` command. Each surface package stamps its own version
constant into its own output.

**Tech Stack:** Go 1.26, standard library only — `reflect`, `encoding/json`,
`go/ast`/`go/parser` (for the enum-coverage guard). No new dependency.

**Source spec:** [docs/superpowers/specs/2026-07-30-versioned-json-schema-design.md](../specs/2026-07-30-versioned-json-schema-design.md)
(commit `99679ec`). Branch `json-schema`, cut off `main` at `2c50f2a`.

## Global Constraints

Every task's requirements implicitly include all of these.

- Every commit carries a `Signed-off-by` trailer matching its author — use
  `git commit -s`. `main` enforces DCO; a commit without it fails CI.
- **No `Co-Authored-By` trailer and no AI attribution anywhere** — commits, PR
  bodies, code comments, docs, CHANGELOG. Every commit is authored solely by the
  human.
- No new third-party dependency. The generator is `reflect` + `encoding/json`.
- Standard-library `flag` only — Cobra is a later sub-project.
- Detectors stay pure functions; the scan stays sequential (no goroutines).
- `internal/report/testdata/golden-scan.txt` must stay **byte-identical**. Prove
  it with `go test ./internal/report -run TestGoldenScanOutput`, never with
  `-update`.
- `go test` runs with **`-p 2`** (full parallelism trips a known Go linker panic
  on `internal/advisory`). Never `-short`.
- No secrets, credentials, private IPs or internal hostnames anywhere, including
  schema titles, descriptions and examples. Documentation IPs are RFC 5737
  (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains are
  RFC 2606 (`.example`, `example.com`).
- Untrusted API text is sanitized at ingress, not at each renderer. A schema
  describes shape and adds no new ingress, so this work introduces **no new
  `safetext` call site**.
- `internal/jsonschema` must never import `internal/remediate` or
  `internal/explain` — it imports nothing from kubeagent at all.
- Usage and error text use the `invokedAs` variable, never a hardcoded
  `"kubeagent"`.
- TDD: write the failing test first, run it and watch it fail, then implement.
- Go lives at `/usr/local/go/bin`: `export PATH=$PATH:/usr/local/go/bin`.
- `docs/go-concepts.md` is **gitignored** — edit it, but do not `git add` it.

---

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `internal/jsonschema/jsonschema.go` | `Schema`, `Meta`, `Generate`, `TypeKey`, `Major`, the version constants, the reflection walker. Knows nothing about kubeagent. |
| `internal/jsonschema/jsonschema_test.go` | Unit tests over hand-built types in the test file. |
| `internal/schemadoc/schemadoc.go` | The `Documents` table, kubeagent's enum and override tables, `Generate(name)`, `Names()`. |
| `internal/schemadoc/schemadoc_test.go` | Drift test with `-update`, plus the marshaler, foreign-type and named-string guards and the table-validity test. |
| `internal/schemadoc/enums_test.go` | The go/ast test that the enum tables list exactly the packages' own constants. |
| `website/docs/schemas/{scan,gate,rbac-print,rbac-check,watch-issues,watch-explanations}-v1.json` | The six committed, published documents. |
| `website/docs/features/json-schema.md` | The compatibility contract. |

**Modified:** `internal/report/report.go`, `internal/watch/metrics.go`,
`internal/rbacprofile/profile.go`, `internal/rbacprofile/check.go`,
`internal/gate/gate.go`, `main.go`, `main_test.go`, `website/mkdocs.yml`,
`CHANGELOG.md`, `website/docs/roadmap.md`, `CLAUDE.md`, and the cross-linked
docs pages.

---

## Task 1: `internal/jsonschema` — the generator

**Files:**

- Create: `internal/jsonschema/jsonschema.go`
- Test: `internal/jsonschema/jsonschema_test.go`

**Interfaces:**

- Consumes: nothing. This package imports only `encoding/json`, `fmt`,
  `reflect`, `sort`, `strings`.
- Produces, relied on by Tasks 3, 4 and 5:
  - `type Schema = map[string]any`
  - `type Meta struct { Name, Version, Title, Description string; Enums map[string][]string; Overrides map[string]Schema }`
  - `func Generate(root reflect.Type, m Meta) ([]byte, error)`
  - `func TypeKey(t reflect.Type) string`
  - `func Major(version string) (string, error)`
  - `const ScanVersion, GateVersion, RBACVersion, WatchVersion = "1.0"`

- [ ] **Step 1: Write the failing tests**

Create `internal/jsonschema/jsonschema_test.go`. The types under test are
declared in the test file itself, so their `TypeKey` prefix is `jsonschema.`:

```go
package jsonschema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The shapes under test. Declared here rather than borrowed from a real report
// package: the generator must be provable without a cluster type in sight.
type leaf struct {
	Name string `json:"name"`
}

type colour string

type flat struct {
	SchemaVersion string            `json:"schemaVersion"`
	Title         string            `json:"title"`
	Count         int               `json:"count"`
	Ratio         float64           `json:"ratio"`
	OK            bool              `json:"ok"`
	Shade         colour            `json:"shade"`
	Child         leaf              `json:"child"`
	MaybeChild    *leaf             `json:"maybeChild,omitempty"`
	AlwaysChild   *leaf             `json:"alwaysChild"`
	Items         []leaf            `json:"items"`
	MaybeItems    []leaf            `json:"maybeItems,omitempty"`
	Counts        map[colour]int    `json:"counts"`
	Labels        map[string]string `json:"labels,omitempty"`
	Skipped       string            `json:"-"`
	unexported    string
	Renamed       string `json:"renamed,omitempty"`
}

// selfRef proves the $defs recursion guard terminates.
type selfRef struct {
	Name  string     `json:"name"`
	Child *selfRef   `json:"child,omitempty"`
	Peers []*selfRef `json:"peers,omitempty"`
}

// coded is an int with a custom marshaler, standing in for findings.Level: the
// JSON is a string and reflection would say integer.
type coded int

func (c coded) MarshalJSON() ([]byte, error) { return json.Marshal("critical") }

type withCoded struct {
	Level coded `json:"level"`
}

func testMeta() Meta {
	return Meta{
		Name:        "example",
		Version:     "1.0",
		Title:       "Example document",
		Description: "A document used only by tests.",
		Enums:       map[string][]string{"jsonschema.colour": {"red", "green"}},
		Overrides:   map[string]Schema{"jsonschema.coded": {"type": "string"}},
	}
}

// generate renders and decodes, so tests assert on the document a consumer sees.
func generate(t *testing.T, root reflect.Type, m Meta) map[string]any {
	t.Helper()
	raw, err := Generate(root, m)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("output does not end in a newline")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return doc
}

// props walks into a nested object, failing loudly rather than panicking.
func props(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	p, ok := obj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("no properties object in %v", obj)
	}
	return p
}

func field(t *testing.T, obj map[string]any, name string) map[string]any {
	t.Helper()
	f, ok := props(t, obj)[name].(map[string]any)
	if !ok {
		t.Fatalf("no %q property in %v", name, props(t, obj))
	}
	return f
}

func TestTypeKey(t *testing.T) {
	if got, want := TypeKey(reflect.TypeOf(leaf{})), "jsonschema.leaf"; got != want {
		t.Errorf("TypeKey = %q, want %q", got, want)
	}
	if got, want := TypeKey(reflect.TypeOf("")), "string"; got != want {
		t.Errorf("TypeKey(string) = %q, want %q", got, want)
	}
}

func TestMajor(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "1.0", want: "1"},
		{in: "2.17", want: "2"},
		{in: "1", wantErr: true},
		{in: "1.x", wantErr: true},
		{in: "", wantErr: true},
	} {
		got, err := Major(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Major(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Major(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

func TestGenerateDocumentEnvelope(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	for k, want := range map[string]string{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         "https://k8sproject.top/schemas/example-v1.json",
		"title":       "Example document",
		"description": "A document used only by tests.",
		"type":        "object",
	} {
		if got, _ := doc[k].(string); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestGenerateScalarKinds(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	for name, want := range map[string]string{
		"title": "string", "count": "integer", "ratio": "number", "ok": "boolean",
	} {
		if got, _ := field(t, doc, name)["type"].(string); got != want {
			t.Errorf("%s type = %q, want %q", name, got, want)
		}
	}
}

// The root's schemaVersion is a pattern over the MAJOR, not a const: a 1.1
// document must still validate against the published 1.0 schema, or the
// minor-bump promise is void.
func TestGenerateRootSchemaVersionIsAMajorPattern(t *testing.T) {
	f := field(t, generate(t, reflect.TypeOf(flat{}), testMeta()), "schemaVersion")
	if got, _ := f["type"].(string); got != "string" {
		t.Errorf("schemaVersion type = %q, want string", got)
	}
	if got, want := f["pattern"], `^1\.[0-9]+$`; got != want {
		t.Errorf("schemaVersion pattern = %v, want %q", got, want)
	}
	if _, ok := f["const"]; ok {
		t.Error("schemaVersion must not be a const — a 1.1 document would fail validation")
	}
}

func TestGenerateEnum(t *testing.T) {
	f := field(t, generate(t, reflect.TypeOf(flat{}), testMeta()), "shade")
	if got, _ := f["type"].(string); got != "string" {
		t.Errorf("shade type = %q, want string", got)
	}
	got, _ := f["enum"].([]any)
	if len(got) != 2 || got[0] != "red" || got[1] != "green" {
		t.Errorf("shade enum = %v, want [red green]", got)
	}
}

func TestGenerateStructBecomesARef(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	if got, _ := field(t, doc, "child")["$ref"].(string); got != "#/$defs/jsonschema.leaf" {
		t.Errorf("child $ref = %q, want #/$defs/jsonschema.leaf", got)
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatal("no $defs object")
	}
	if _, ok := defs["jsonschema.leaf"]; !ok {
		t.Errorf("$defs has no jsonschema.leaf: %v", defs)
	}
}

// A nil slice marshals to null, not []. A field that cannot be absent must
// therefore admit null, or the schema lies about kubeagent's own output.
func TestGenerateSliceNullability(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	if got, _ := field(t, doc, "maybeItems")["type"].(string); got != "array" {
		t.Errorf("maybeItems type = %q, want array", got)
	}
	got, _ := field(t, doc, "items")["type"].([]any)
	if len(got) != 2 || got[0] != "array" || got[1] != "null" {
		t.Errorf("items type = %v, want [array null]", got)
	}
	items, _ := field(t, doc, "items")["items"].(map[string]any)
	if got, _ := items["$ref"].(string); got != "#/$defs/jsonschema.leaf" {
		t.Errorf("items.items $ref = %q, want the leaf ref", got)
	}
}

func TestGeneratePointerNullability(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	if got, _ := field(t, doc, "maybeChild")["$ref"].(string); got != "#/$defs/jsonschema.leaf" {
		t.Errorf("maybeChild = %v, want a bare $ref", field(t, doc, "maybeChild"))
	}
	// A $ref carries no type, so null is admitted by wrapping instead.
	any_, ok := field(t, doc, "alwaysChild")["anyOf"].([]any)
	if !ok || len(any_) != 2 {
		t.Fatalf("alwaysChild = %v, want anyOf[ref, null]", field(t, doc, "alwaysChild"))
	}
}

func TestGenerateMap(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	labels := field(t, doc, "labels")
	if got, _ := labels["type"].(string); got != "object" {
		t.Errorf("labels type = %q, want object", got)
	}
	vals, _ := labels["additionalProperties"].(map[string]any)
	if got, _ := vals["type"].(string); got != "string" {
		t.Errorf("labels values = %v, want string", vals)
	}
	// A named string key gets propertyNames from the enum table, and no
	// omitempty means the nil map's null must be admitted.
	counts := field(t, doc, "counts")
	got, _ := counts["type"].([]any)
	if len(got) != 2 || got[0] != "object" || got[1] != "null" {
		t.Errorf("counts type = %v, want [object null]", got)
	}
	names, _ := counts["propertyNames"].(map[string]any)
	enum, _ := names["enum"].([]any)
	if len(enum) != 2 {
		t.Errorf("counts propertyNames = %v, want the colour enum", names)
	}
}

func TestGenerateSkipsHiddenFields(t *testing.T) {
	p := props(t, generate(t, reflect.TypeOf(flat{}), testMeta()))
	for _, name := range []string{"Skipped", "-", "unexported"} {
		if _, ok := p[name]; ok {
			t.Errorf("%q must not appear in properties", name)
		}
	}
	if _, ok := p["renamed"]; !ok {
		t.Error("the json tag name must win over the Go field name")
	}
}

func TestGenerateRequiredIsTheNonOmitemptyFieldsSorted(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	req, _ := doc["required"].([]any)
	var got []string
	for _, r := range req {
		s, _ := r.(string)
		got = append(got, s)
	}
	want := []string{"alwaysChild", "child", "count", "counts", "items", "ok", "ratio", "schemaVersion", "shade", "title"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("required = %v, want %v", got, want)
	}
}

func TestGenerateLeavesAdditionalPropertiesOpen(t *testing.T) {
	doc := generate(t, reflect.TypeOf(flat{}), testMeta())
	if _, ok := doc["additionalProperties"]; ok {
		t.Error("additionalProperties must stay unset so a MINOR bump cannot break validation")
	}
	if got, _ := doc["$comment"].(string); !strings.Contains(got, "additionalProperties") {
		t.Errorf("$comment = %q, want it to explain the open object", got)
	}
}

func TestGenerateOverrideBeatsTheGoKind(t *testing.T) {
	f := field(t, generate(t, reflect.TypeOf(withCoded{}), testMeta()), "level")
	if got, _ := f["type"].(string); got != "string" {
		t.Errorf("level type = %q, want string from the override, not integer", got)
	}
}

// A generation run must not mutate the caller's Overrides table.
func TestGenerateDoesNotMutateMeta(t *testing.T) {
	m := testMeta()
	m.Enums["jsonschema.coded"] = []string{"critical"}
	if _, err := Generate(reflect.TypeOf(withCoded{}), m); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := m.Overrides["jsonschema.coded"]; len(got) != 1 {
		t.Errorf("override table was mutated: %v", got)
	}
}

func TestGenerateHandlesRecursiveTypes(t *testing.T) {
	doc := generate(t, reflect.TypeOf(selfRef{}), testMeta())
	defs, _ := doc["$defs"].(map[string]any)
	if _, ok := defs["jsonschema.selfRef"]; !ok {
		t.Errorf("$defs has no jsonschema.selfRef: %v", defs)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate(reflect.TypeOf(flat{}), testMeta())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate(reflect.TypeOf(flat{}), testMeta())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(a) != string(b) {
		t.Error("two runs produced different bytes")
	}
}

func TestGenerateRejectsWhatItCannotDescribe(t *testing.T) {
	type withInterface struct {
		Anything any `json:"anything"`
	}
	type withEmbedded struct {
		leaf
		Extra string `json:"extra"`
	}
	type withBadMap struct {
		Counts map[int]string `json:"counts"`
	}
	for name, root := range map[string]reflect.Type{
		"interface":  reflect.TypeOf(withInterface{}),
		"embedded":   reflect.TypeOf(withEmbedded{}),
		"int-keyed":  reflect.TypeOf(withBadMap{}),
		"non-struct": reflect.TypeOf(""),
	} {
		if _, err := Generate(root, testMeta()); err == nil {
			t.Errorf("%s: want an error, got a document", name)
		}
	}
	m := testMeta()
	m.Version = "one"
	if _, err := Generate(reflect.TypeOf(flat{}), m); err == nil {
		t.Error("a malformed version must be an error")
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/jsonschema -p 2
```

Expected: build failure — `undefined: Generate`, `undefined: Meta`,
`undefined: TypeKey`, `undefined: Major`.

- [ ] **Step 3: Write the generator**

Create `internal/jsonschema/jsonschema.go`:

```go
// Package jsonschema renders a Go type as a JSON Schema draft 2020-12 document.
//
// It knows nothing about kubeagent: the caller supplies the document's identity
// and the two things reflection cannot derive — which named types are enums, and
// which types have a custom MarshalJSON whose JSON differs from their Go kind.
// That keeps this package importable by every surface package, including the
// ones that may not import internal/remediate or internal/explain: it imports
// nothing from kubeagent at all.
package jsonschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Schema is one JSON Schema object. A map, so encoding/json sorts its keys and
// generated output is byte-deterministic without an ordered container. Property
// order is therefore alphabetical, which JSON does not give meaning to anyway.
type Schema = map[string]any

// Per-surface schema versions, MAJOR.MINOR. Deliberately not the kubeagent
// release version: a surface's version moves only when its own shape does, so a
// new scan field does not disturb a CI pipeline reading the gate document.
const (
	ScanVersion  = "1.0"
	GateVersion  = "1.0"
	RBACVersion  = "1.0"
	WatchVersion = "1.0"
)

// baseID is where the generated files are published. The $id carries the MAJOR
// only, so a minor bump does not move a URL a consumer pinned.
const baseID = "https://k8sproject.top/schemas/"

// openComment explains the one thing a validator user must know about these
// documents: they do not forbid unknown properties, on purpose.
const openComment = "additionalProperties is deliberately unset (JSON Schema's default: permitted). A MINOR version bump may add properties, and a document must still validate against an older schema of the same MAJOR."

// Meta is what the caller knows and reflection cannot.
type Meta struct {
	Name        string // "scan" — the file stem and the schema command's argument
	Version     string // "1.0"
	Title       string
	Description string
	// Enums maps a TypeKey to the complete set of values that type may hold.
	// Reflection cannot see a const block.
	Enums map[string][]string
	// Overrides maps a TypeKey to the schema its custom MarshalJSON implies,
	// for types whose JSON is not their Go kind.
	Overrides map[string]Schema
}

// TypeKey is the "<pkgbase>.<TypeName>" key used by $defs, Enums and Overrides.
// Exported so a caller's guard tests key the same way the generator does.
func TypeKey(t reflect.Type) string {
	if t.Name() == "" || t.PkgPath() == "" {
		return t.String()
	}
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	return pkg + "." + t.Name()
}

// Major returns the MAJOR component of a MAJOR.MINOR version.
func Major(version string) (string, error) {
	major, minor, ok := strings.Cut(version, ".")
	if !ok || !digits(major) || !digits(minor) {
		return "", fmt.Errorf("version %q is not MAJOR.MINOR", version)
	}
	return major, nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Generate renders root as a JSON Schema document, indented and newline
// terminated. The same type and Meta always produce identical bytes.
func Generate(root reflect.Type, m Meta) ([]byte, error) {
	if root.Kind() != reflect.Struct {
		return nil, fmt.Errorf("root type %s is not a struct", root)
	}
	major, err := Major(m.Version)
	if err != nil {
		return nil, err
	}
	g := &generator{meta: m, defs: Schema{}, open: map[string]bool{}}
	obj, err := g.object(root)
	if err != nil {
		return nil, err
	}
	// The root's own version property is the one thing reflection gets
	// uselessly right: it is a string, but which string matters. A const of
	// m.Version would reject a compatible 1.1 document, so it becomes a
	// pattern over the major instead.
	if p, ok := obj["properties"].(Schema); ok {
		if _, ok := p["schemaVersion"]; ok {
			p["schemaVersion"] = Schema{
				"type":        "string",
				"pattern":     `^` + major + `\.[0-9]+$`,
				"description": "This document's schema version, MAJOR.MINOR. Treat an unknown MINOR as compatible.",
			}
		}
	}
	doc := Schema{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         baseID + m.Name + "-v" + major + ".json",
		"title":       m.Title,
		"description": m.Description,
	}
	for k, v := range obj {
		doc[k] = v
	}
	if len(g.defs) > 0 {
		doc["$defs"] = g.defs
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type generator struct {
	meta Meta
	defs Schema          // TypeKey → the inline object schema
	open map[string]bool // TypeKeys mid-build, so a recursive type terminates
}

// object renders a struct type inline: type, properties, required, $comment.
func (g *generator) object(t reflect.Type) (Schema, error) {
	p := Schema{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported: encoding/json never emits it
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			return nil, fmt.Errorf("%s.%s: an embedded field with no json name would be flattened; give it a name or project it into a named field", TypeKey(t), f.Name)
		}
		if name == "" {
			name = f.Name
		}
		omitempty := hasOpt(opts, "omitempty")
		s, err := g.field(f.Type, omitempty)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", TypeKey(t), name, err)
		}
		p[name] = s
		if !omitempty {
			required = append(required, name)
		}
	}
	out := Schema{"type": "object", "properties": p, "$comment": openComment}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out, nil
}

func hasOpt(opts, want string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == want {
			return true
		}
	}
	return false
}

// field renders one field's type. omitempty decides nullability: a nil slice,
// map or pointer marshals to null rather than to an empty value, so a field
// that cannot be absent has to admit null or the schema lies about the output.
func (g *generator) field(t reflect.Type, omitempty bool) (Schema, error) {
	key := TypeKey(t)
	if o, ok := g.meta.Overrides[key]; ok {
		s := clone(o)
		g.applyEnum(s, key)
		return s, nil
	}
	switch t.Kind() {
	case reflect.String:
		s := Schema{"type": "string"}
		g.applyEnum(s, key)
		return s, nil
	case reflect.Bool:
		return Schema{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Schema{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return Schema{"type": "number"}, nil
	case reflect.Pointer:
		s, err := g.field(t.Elem(), true)
		if err != nil {
			return nil, err
		}
		if omitempty {
			return s, nil
		}
		return nullable(s), nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return Schema{"type": "string", "contentEncoding": "base64"}, nil
		}
		items, err := g.field(t.Elem(), true)
		if err != nil {
			return nil, err
		}
		s := Schema{"type": "array", "items": items}
		if !omitempty {
			s["type"] = []string{"array", "null"}
		}
		return s, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key type %s is not a string", t.Key())
		}
		vals, err := g.field(t.Elem(), true)
		if err != nil {
			return nil, err
		}
		s := Schema{"type": "object", "additionalProperties": vals}
		if vs, ok := g.meta.Enums[TypeKey(t.Key())]; ok {
			s["propertyNames"] = Schema{"enum": append([]string(nil), vs...)}
		}
		if !omitempty {
			s["type"] = []string{"object", "null"}
		}
		return s, nil
	case reflect.Struct:
		return g.ref(t)
	case reflect.Interface:
		return nil, fmt.Errorf("an interface has no describable shape: %s", t)
	}
	return nil, fmt.Errorf("unsupported kind %s (%s)", t.Kind(), t)
}

// ref registers t in $defs once and returns a reference to it. The name is
// marked open before the body is built, so a self-referential type terminates.
func (g *generator) ref(t reflect.Type) (Schema, error) {
	if t.Name() == "" || t.PkgPath() == "" {
		return nil, fmt.Errorf("an anonymous struct cannot be a $defs entry: %s", t)
	}
	key := TypeKey(t)
	if _, done := g.defs[key]; !done && !g.open[key] {
		g.open[key] = true
		obj, err := g.object(t)
		delete(g.open, key)
		if err != nil {
			return nil, err
		}
		g.defs[key] = obj
	}
	return Schema{"$ref": "#/$defs/" + key}, nil
}

// nullable admits null alongside a value's own type. A $ref carries no type, so
// it is wrapped in anyOf instead of edited.
func nullable(s Schema) Schema {
	switch tv := s["type"].(type) {
	case string:
		out := clone(s)
		out["type"] = []string{tv, "null"}
		return out
	case []string:
		out := clone(s)
		out["type"] = append(append([]string{}, tv...), "null")
		return out
	}
	return Schema{"anyOf": []any{s, Schema{"type": "null"}}}
}

func (g *generator) applyEnum(s Schema, key string) {
	if vs, ok := g.meta.Enums[key]; ok {
		s["enum"] = append([]string(nil), vs...)
	}
}

// clone copies one level, enough that a generation run cannot mutate the
// caller's Overrides table.
func clone(s Schema) Schema {
	out := make(Schema, len(s)+1)
	for k, v := range s {
		out[k] = v
	}
	return out
}
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
go test ./internal/jsonschema -p 2 -v
go vet ./internal/jsonschema
```

Expected: every test PASS. If `TestGenerateRequiredIsTheNonOmitemptyFieldsSorted`
fails, compare the got list against the `flat` struct's tags — the expected list
is exactly its non-`omitempty` exported fields, sorted.

- [ ] **Step 5: Prove the package imports nothing from kubeagent**

```bash
go list -deps ./internal/jsonschema | grep imantaba || echo "no kubeagent imports — correct"
```

Expected: the `echo` line. If anything prints, an import crept in; remove it.

- [ ] **Step 6: Commit**

```bash
git add internal/jsonschema
git commit -s -m "feat(jsonschema): reflection-based JSON Schema generator"
```

---

## Task 2: Export the document root types

**Files:**

- Modify: `internal/report/report.go` (rename `inventoryReport`,
  `investigationView`, `remediationActionView`)
- Modify: `internal/watch/metrics.go:587-745` (rename the seven view types)
- Modify: `internal/rbacprofile/profile.go` (add `RulesDocument`)
- Modify: `internal/rbacprofile/check.go` (add `CheckDocument`)
- Test: `internal/rbacprofile/document_test.go` (create)

**Interfaces:**

- Consumes: nothing from Task 1.
- Produces, relied on by Tasks 3 and 4:
  - `report.ScanReport`, `report.InvestigationView`, `report.RemediationActionView`
  - `watch.IssuesReport`, `watch.ExplanationsReport`, `watch.IssueView`,
    `watch.ClusterView`, `watch.StatsView`, `watch.ExplanationView`,
    `watch.ExplainStatsView`
  - `rbacprofile.RulesDocument{SchemaVersion string; RoleName string; Rules []Rule}`
  - `rbacprofile.CheckDocument{SchemaVersion string; Features []FeatureStatus}`

Nothing emits the two rbac documents yet — Task 3 wires them. This task is
renames plus two new types, and **no output changes**.

- [ ] **Step 1: Write the failing test**

Create `internal/rbacprofile/document_test.go`:

```go
package rbacprofile

import (
	"encoding/json"
	"strings"
	"testing"
)

// The rbac outputs were bare JSON arrays, which can never carry a version
// field. Both are wrapped, and these are the wrappers.
func TestRulesDocumentShape(t *testing.T) {
	raw, err := json.Marshal(RulesDocument{
		SchemaVersion: "1.0",
		RoleName:      "kubeagent",
		Rules:         []Rule{{APIGroup: "", Resources: []string{"pods"}, Verbs: []string{"get", "list"}}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"schemaVersion":"1.0"`, `"roleName":"kubeagent"`, `"rules":[`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("document is missing %s:\n%s", want, raw)
		}
	}
}

func TestCheckDocumentShape(t *testing.T) {
	raw, err := json.Marshal(CheckDocument{
		SchemaVersion: "1.0",
		Features:      []FeatureStatus{{Name: "core", Summary: "list pods", Allowed: true}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"schemaVersion":"1.0"`, `"features":[`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("document is missing %s:\n%s", want, raw)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/rbacprofile -p 2
```

Expected: `undefined: RulesDocument`, `undefined: CheckDocument`.

- [ ] **Step 3: Add the two document types**

In `internal/rbacprofile/profile.go`, after the `Rule` type:

```go
// RulesDocument is the --output json shape of `rbac print`. The command used to
// emit a bare array of rules; an array root can carry no version field, so the
// rules moved under a key. RoleName is here because it is what the YAML form
// names the ClusterRole, and a consumer generating one needs both halves.
type RulesDocument struct {
	SchemaVersion string `json:"schemaVersion"`
	RoleName      string `json:"roleName"`
	Rules         []Rule `json:"rules"`
}
```

In `internal/rbacprofile/check.go`, after `FeatureStatus`:

```go
// CheckDocument is the --output json shape of `rbac check`, wrapping what was a
// bare array so the output can declare its version.
type CheckDocument struct {
	SchemaVersion string          `json:"schemaVersion"`
	Features      []FeatureStatus `json:"features"`
}
```

- [ ] **Step 4: Rename the report types**

In `internal/report/report.go`, rename with the comments updated to match:

- `inventoryReport` → `ScanReport`, and its doc comment becomes:

```go
// ScanReport is the JSON document written by `kubeagent scan --output json`. It
// is exported because internal/schemadoc has to name it to generate its
// published schema; nothing outside this package constructs one.
type ScanReport struct {
```

- `investigationView` → `InvestigationView`, `remediationActionView` →
  `RemediationActionView`, and the two builder functions' signatures follow
  (`investigationOf` returns `*InvestigationView`, `remediationPlanOf` returns
  `[]RemediationActionView`).
- The `inventoryReport{` literal in `PrintInventory` becomes `ScanReport{`, and
  the line-141 comment referencing `inventoryReport` becomes `ScanReport`.

```bash
grep -rn "inventoryReport\|investigationView\|remediationActionView" --include="*.go" .
```

Expected after the edit: no matches.

- [ ] **Step 5: Rename the watch view types**

In `internal/watch/metrics.go`: `issueView` → `IssueView`, `clusterView` →
`ClusterView`, `statsView` → `StatsView`, `issuesView` → `IssuesReport`,
`explanationView` → `ExplanationView`, `explainStatsView` → `ExplainStatsView`,
`explanationsView` → `ExplanationsReport`. The helper `issueViews` keeps its
name (it is a function, not a type) but its signature returns `[]IssueView`.

Add to the two report types' doc comments why they are exported:

```go
// IssuesReport is the document served by GET /issues. Exported, like the view
// types it reaches, because these names become $defs keys in a published
// schema: watch.IssueView reads like a contract where watch.issueView reads
// like a leaked internal.
type IssuesReport struct {
```

```bash
grep -rn "issuesView\|explanationsView\|issueView{\|clusterView\|statsView\|explanationView\|explainStatsView" --include="*.go" .
```

Expected after the edit: no matches (`issueViews(` as a function call is fine —
check the match is the function, not a type).

- [ ] **Step 6: Prove nothing changed but names**

```bash
go build ./... && go test ./internal/report ./internal/watch ./internal/rbacprofile -p 2
go test ./internal/report -run TestGoldenScanOutput -p 2 -v
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: all pass; the last command prints nothing (the golden file is
untouched). Do **not** pass `-update`.

- [ ] **Step 7: Run the whole suite**

```bash
go test ./... -p 2 2>&1 | tail -40
```

Expected: no failures.

- [ ] **Step 8: Commit**

```bash
git add internal/report internal/watch internal/rbacprofile
git commit -s -m "refactor: export the JSON document root types"
```

---

## Task 3: Stamp `schemaVersion` into all six outputs

**Files:**

- Modify: `internal/report/report.go` (field + stamp in `PrintInventory`)
- Modify: `internal/gate/gate.go` (field + stamp in `Decide`)
- Modify: `internal/watch/metrics.go` (field on both reports + stamp in
  `issuesJSON`, `explanationsJSON`)
- Modify: `main.go:~817` and `main.go:~862` (encode the two rbac documents)
- Test: `internal/report/report_test.go`, `internal/gate/gate_test.go`,
  `internal/watch/metrics_test.go`, `main_test.go` (add tests to the existing
  files)

**Interfaces:**

- Consumes: `jsonschema.ScanVersion`, `GateVersion`, `RBACVersion`,
  `WatchVersion` from Task 1; the exported roots from Task 2.
- Produces: every one of the six documents carries
  `"schemaVersion": "1.0"`. `rbac print --output json` and `rbac check --output
  json` change from an array root to an object root — a deliberate breaking
  change, documented in Task 6.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go`:

```go
func TestPrintInventoryStampsTheSchemaVersion(t *testing.T) {
	var out bytes.Buffer
	if err := PrintInventory(Input{}, "json", &out); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	var doc struct {
		SchemaVersion string `json:"schemaVersion"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.SchemaVersion != jsonschema.ScanVersion {
		t.Errorf("schemaVersion = %q, want %q", doc.SchemaVersion, jsonschema.ScanVersion)
	}
}
```

Add to `internal/gate/gate_test.go`:

```go
func TestDecideStampsTheSchemaVersion(t *testing.T) {
	v := Decide(scan.Result{}, Options{FailOn: findings.Critical})
	if v.SchemaVersion != jsonschema.GateVersion {
		t.Errorf("SchemaVersion = %q, want %q", v.SchemaVersion, jsonschema.GateVersion)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"schemaVersion":"`+jsonschema.GateVersion+`"`) {
		t.Errorf("verdict JSON has no schemaVersion:\n%s", raw)
	}
}
```

Add to `internal/sarif/sarif_test.go` — the new field must not leak into SARIF,
whose version is OASIS's to set:

```go
func TestRenderIgnoresTheGateSchemaVersion(t *testing.T) {
	v := gate.Verdict{Verdict: "pass", Code: 0, Scope: "cluster"}
	without, err := Render(v, "v0.0.0-test")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	v.SchemaVersion = "1.0"
	with, err := Render(v, "v0.0.0-test")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(without, with) {
		t.Error("the gate's schemaVersion changed the SARIF output; SARIF is versioned by OASIS")
	}
}
```

Add to `internal/watch/metrics_test.go`:

```go
func TestIssuesAndExplanationsJSONStampTheSchemaVersion(t *testing.T) {
	m := newMetrics([]string{"local"})
	for name, get := range map[string]func() ([]byte, error){
		"/issues":       m.issuesJSON,
		"/explanations": m.explanationsJSON,
	} {
		raw, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		if doc.SchemaVersion != jsonschema.WatchVersion {
			t.Errorf("%s schemaVersion = %q, want %q", name, doc.SchemaVersion, jsonschema.WatchVersion)
		}
	}
}
```

`newMetrics` is this package's existing constructor — match the call the
surrounding tests already make; if its signature differs, copy theirs.

Add to `main_test.go`:

```go
func TestRunRBACPrintJSONIsAVersionedObject(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runRBACPrint([]string{"--output", "json"}); err != nil {
			t.Fatalf("runRBACPrint: %v", err)
		}
	})
	var doc rbacprofile.RulesDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a RulesDocument: %v\n%s", err, out)
	}
	if doc.SchemaVersion != jsonschema.RBACVersion {
		t.Errorf("schemaVersion = %q, want %q", doc.SchemaVersion, jsonschema.RBACVersion)
	}
	if doc.RoleName != "kubeagent" {
		t.Errorf("roleName = %q, want the --role-name default", doc.RoleName)
	}
	if len(doc.Rules) == 0 {
		t.Error("rules is empty; the scan profile resolves to at least one rule")
	}
}
```

`rbac check` needs a cluster, so its shape is covered by the
`rbacprofile.CheckDocument` test from Task 2 plus the wiring review — do not add
a `runRBACCheck` test that would dial a cluster.

- [ ] **Step 2: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report ./internal/gate ./internal/sarif ./internal/watch . -p 2 2>&1 | head -30
```

Expected: `doc.SchemaVersion` empty / `v.SchemaVersion undefined` /
`json: cannot unmarshal array into Go value of type rbacprofile.RulesDocument`.

- [ ] **Step 3: Add the fields and stamp them**

`internal/report/report.go` — first field of `ScanReport`, and set it in
`PrintInventory`:

```go
type ScanReport struct {
	SchemaVersion string `json:"schemaVersion"`
	Cluster       clusterhealth.ClusterHealth `json:"cluster"`
	// … unchanged …
```

```go
		return enc.Encode(ScanReport{
			SchemaVersion:      jsonschema.ScanVersion,
			Cluster:            in.Cluster,
```

`internal/gate/gate.go` — first field of `Verdict`, set in `Decide`:

```go
type Verdict struct {
	SchemaVersion string `json:"schemaVersion"`
	Verdict       string `json:"verdict"`
	// … unchanged …
```

```go
	v := Verdict{
		SchemaVersion: jsonschema.GateVersion,
		FailOn:        opts.FailOn,
```

`internal/watch/metrics.go` — first field of both reports, set in both builders:

```go
type IssuesReport struct {
	SchemaVersion string        `json:"schemaVersion"`
	Clusters      []ClusterView `json:"clusters"`
	Active        []IssueView   `json:"active"`
	Resolved      []IssueView   `json:"resolved"`
	Stats         StatsView     `json:"stats"`
}
```

```go
	out := IssuesReport{
		SchemaVersion: jsonschema.WatchVersion,
		Clusters:      make([]ClusterView, 0, len(m.names)),
```

```go
type ExplanationsReport struct {
	SchemaVersion string            `json:"schemaVersion"`
	Explanations  []ExplanationView `json:"explanations"`
	Stats         ExplainStatsView  `json:"stats"`
}
```

```go
	out := ExplanationsReport{
		SchemaVersion: jsonschema.WatchVersion,
		Explanations:  []ExplanationView{},
```

`main.go` — `runRBACPrint`'s json branch:

```go
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rbacprofile.RulesDocument{
			SchemaVersion: jsonschema.RBACVersion,
			RoleName:      *roleName,
			Rules:         rules,
		})
```

`main.go` — `runRBACCheck`'s json branch:

```go
	if *output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rbacprofile.CheckDocument{
			SchemaVersion: jsonschema.RBACVersion,
			Features:      statuses,
		}); err != nil {
			return err
		}
	} else {
```

Add the `internal/jsonschema` import to each modified file.

- [ ] **Step 4: Run the tests and watch them pass**

```bash
go test ./internal/report ./internal/gate ./internal/sarif ./internal/watch . -p 2
go test ./internal/report -run TestGoldenScanOutput -p 2 -v
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: all pass; the golden file diff is empty. The new field is JSON-only
and touches no text renderer.

- [ ] **Step 5: Eyeball the two changed shapes**

```bash
go build -o /tmp/kubeagent-t3 . && /tmp/kubeagent-t3 rbac print --output json | head -12
```

Expected: an object opening with `"schemaVersion": "1.0"`, then `"roleName"`,
then `"rules": [`.

- [ ] **Step 6: Run the whole suite**

```bash
go test ./... -p 2 2>&1 | tail -40
```

Expected: no failures. Any test asserting the old array shape of the rbac JSON
must be updated to the document shape — that is the intended break, not a
regression to work around.

- [ ] **Step 7: Commit**

```bash
git add internal/report internal/gate internal/watch main.go main_test.go internal/sarif
git commit -s -m "feat: declare a schemaVersion in every machine-readable output"
```

---

## Task 4: `internal/schemadoc` — the registry, the guards, the committed files

**Files:**

- Create: `internal/schemadoc/schemadoc.go`
- Create: `internal/schemadoc/schemadoc_test.go`
- Create: `internal/schemadoc/enums_test.go`
- Create: `website/docs/schemas/*.json` (six files, generated with `-update`)

**Interfaces:**

- Consumes: `jsonschema.Generate`, `Meta`, `Schema`, `TypeKey`, `Major`, the
  version constants (Task 1); the exported roots (Task 2).
- Produces, relied on by Task 5: `schemadoc.Documents []Document`,
  `schemadoc.Generate(name string) ([]byte, error)`,
  `schemadoc.Names() []string`.

`internal/schemadoc` imports `report`, `gate`, `rbacprofile` and `watch`, so it
transitively reaches `internal/remediate` and `internal/explain`. That is
allowed and deliberate: the invariants constrain what `gate`, `mcp`, `tui`,
`rbacprofile`, `safetext` and `fuzzgen` **import**, not who imports them. Only
`main.go` and this package's own tests import `schemadoc`; it holds no client
and no context and makes no call.

- [ ] **Step 1: Write the registry and its generator entry point**

Create `internal/schemadoc/schemadoc.go`:

```go
// Package schemadoc names the JSON documents kubeagent publishes a schema for,
// and holds the two tables the generator cannot derive by reflection: which
// named types are enums, and which have a custom marshaler.
//
// One table drives the committed schema files, the `kubeagent schema` command
// and the drift test — the same shape as rbacprofile.Feature, which generates
// every RBAC manifest and the chart ClusterRole from one list.
package schemadoc

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/gitops"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/operators"
	"github.com/imantaba/kubeagent/internal/rbacprofile"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/watch"
)

// Document is one machine-readable JSON output kubeagent promises a shape for.
// Surface names the version that governs it: rbac-print and rbac-check share
// one, because a consumer that scripts one usually scripts both.
type Document struct {
	Name        string // "scan" — the file stem and the schema command's argument
	Surface     string // "scan" — which version constant applies
	Version     string
	Root        reflect.Type
	Title       string
	Description string
}

// Documents is the single source of truth. Order is the order `kubeagent
// schema` lists them in.
var Documents = []Document{
	{
		Name: "scan", Surface: "scan", Version: jsonschema.ScanVersion,
		Root:        reflect.TypeOf(report.ScanReport{}),
		Title:       "kubeagent scan report",
		Description: "The document written by `kubeagent scan --output json`: the cluster verdict, the prioritized workload inventory, and the findings of every check the run enabled.",
	},
	{
		Name: "gate", Surface: "gate", Version: jsonschema.GateVersion,
		Root:        reflect.TypeOf(gate.Verdict{}),
		Title:       "kubeagent gate verdict",
		Description: "The document written by `kubeagent gate --output json`: the verdict, its exit code, the failing and reported findings, and the reads that were refused.",
	},
	{
		Name: "rbac-print", Surface: "rbac", Version: jsonschema.RBACVersion,
		Root:        reflect.TypeOf(rbacprofile.RulesDocument{}),
		Title:       "kubeagent RBAC rules",
		Description: "The document written by `kubeagent rbac print --output json`: the ClusterRole name and the least-privilege rules the selected features need.",
	},
	{
		Name: "rbac-check", Surface: "rbac", Version: jsonschema.RBACVersion,
		Root:        reflect.TypeOf(rbacprofile.CheckDocument{}),
		Title:       "kubeagent RBAC check",
		Description: "The document written by `kubeagent rbac check --output json`: per feature, whether the current identity may run it and which actions it lacks.",
	},
	{
		Name: "watch-issues", Surface: "watch", Version: jsonschema.WatchVersion,
		Root:        reflect.TypeOf(watch.IssuesReport{}),
		Title:       "kubeagent watch issues",
		Description: "The document served by the watch daemon's GET /issues: each watched cluster's reachability, the active and resolved issues, and the run totals.",
	},
	{
		Name: "watch-explanations", Surface: "watch", Version: jsonschema.WatchVersion,
		Root:        reflect.TypeOf(watch.ExplanationsReport{}),
		Title:       "kubeagent watch explanations",
		Description: "The document served by the watch daemon's GET /explanations: the latest model explanation per object, and the explain budget's totals.",
	},
}

// enums is every named type in the six graphs whose values are a closed set.
// Written from the packages' own constants, so a rename is a compile error
// rather than a schema that quietly drifts.
var enums = map[string][]string{
	"findings.Level": {
		findings.Info.String(), findings.Warning.String(), findings.Critical.String(),
	},
	"gitops.State": {
		string(gitops.StateSynced), string(gitops.StatePending),
		string(gitops.StateStale), string(gitops.StateBlocked),
		string(gitops.StateUnknown),
	},
	"operators.State": {
		string(operators.StateHealthy), string(operators.StateProgressing),
		string(operators.StateUnhealthy), string(operators.StateSuspended),
		string(operators.StateUnknown),
	},
	"capacity.RuleName": {
		string(capacity.RuleNoRequests), string(capacity.RuleLimitNoRequest),
		string(capacity.RuleNeverSchedulable),
	},
}

// overrides describes the types whose JSON is not their Go kind. findings.Level
// is an int whose MarshalJSON emits the spelling; reflection alone would
// document an integer for a field a CI pipeline reads as a string.
var overrides = map[string]jsonschema.Schema{
	"findings.Level": {"type": "string"},
}

// freeFormStrings names document types that are named string types but hold no
// closed set. Empty today. An entry here is a deliberate statement that a
// consumer must not switch on the value — the guard test in this package is
// what forces the choice to be made rather than defaulted.
var freeFormStrings = map[string]bool{}

// Generate renders one document's schema.
func Generate(name string) ([]byte, error) {
	for _, d := range Documents {
		if d.Name != name {
			continue
		}
		return jsonschema.Generate(d.Root, jsonschema.Meta{
			Name:        d.Name,
			Version:     d.Version,
			Title:       d.Title,
			Description: d.Description,
			Enums:       enums,
			Overrides:   overrides,
		})
	}
	return nil, fmt.Errorf("unknown schema %q (want %s)", name, strings.Join(Names(), ", "))
}

// Names lists the document names in table order.
func Names() []string {
	out := make([]string, 0, len(Documents))
	for _, d := range Documents {
		out = append(out, d.Name)
	}
	return out
}

// Path is a document's committed location, relative to the repository root. The
// MAJOR only: a minor bump must not move a URL a consumer pinned.
func Path(d Document) (string, error) {
	major, err := jsonschema.Major(d.Version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("website/docs/schemas/%s-v%s.json", d.Name, major), nil
}
```

- [ ] **Step 2: Write the drift test and the guards**

Create `internal/schemadoc/schemadoc_test.go`:

```go
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
	if len(Documents) != 6 {
		t.Errorf("Documents has %d entries, want the six documented surfaces", len(Documents))
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
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("%s: %v", d.Name, err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatalf("%s: %v", d.Name, err)
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
				additive = append(additive, "no longer required: "+k)
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
```

- [ ] **Step 3: Write the enum-coverage test**

Reflection cannot see a `const` block, so the only way to notice a *new*
constant is to read the source. Create `internal/schemadoc/enums_test.go`:

```go
package schemadoc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// constantsOf parses a package directory and returns the string literals of
// every constant declared with the named type. Reflection cannot see a const
// block, so a constant added to the package without a matching enums entry
// would otherwise ship as a value no consumer was told about.
func constantsOf(t *testing.T, pkgDir, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", pkgDir, err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					id, ok := vs.Type.(*ast.Ident)
					if !ok || id.Name != typeName {
						continue
					}
					for _, v := range vs.Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", lit.Value, err)
						}
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

func TestEnumTablesListEveryConstant(t *testing.T) {
	for _, tc := range []struct{ key, dir, typeName string }{
		{"gitops.State", "../gitops", "State"},
		{"operators.State", "../operators", "State"},
		{"capacity.RuleName", "../capacity", "RuleName"},
	} {
		got := append([]string(nil), enums[tc.key]...)
		want := constantsOf(t, tc.dir, tc.typeName)
		if len(want) == 0 {
			t.Fatalf("%s: parsed no constants from %s — the test is not checking anything", tc.key, tc.dir)
		}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s enum = %v, but the package declares %v. Add the missing value to enums and bump the surface's MINOR — a consumer switching on this field was never told about it.", tc.key, got, want)
		}
	}
}

// findings.Level is an int with iota constants, so no literal to parse: count
// the names in the const block instead, and check the table spells each one.
func TestFindingsLevelEnumCoversEveryLevel(t *testing.T) {
	names := levelConstNames(t)
	if len(names) != len(enums["findings.Level"]) {
		t.Errorf("findings declares %d Level constants (%v) but the enum table lists %d (%v)",
			len(names), names, len(enums["findings.Level"]), enums["findings.Level"])
	}
}

func levelConstNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../findings", nil, 0)
	if err != nil {
		t.Fatalf("parse ../findings: %v", err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				typed := false
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "Level" {
						typed = true
					}
					if !typed {
						continue
					}
					for _, n := range vs.Names {
						out = append(out, n.Name)
					}
				}
			}
		}
	}
	return out
}
```

- [ ] **Step 4: Run the tests and watch the drift test fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/schemadoc -p 2 2>&1 | head -40
```

Expected: the guards, the table test and the enum tests PASS;
`TestSchemaDrift` fails with six "no such file or directory" errors naming the
regenerate command. That failure is the point: the committed files do not exist
yet.

If a guard fails instead, it has found something real — a foreign type, an
un-enumerated named string, a marshaler without an override. Fix the cause, not
the guard.

- [ ] **Step 5: Generate the six documents**

```bash
go test ./internal/schemadoc -run TestSchemaDrift -update -p 2 -v
ls -1 website/docs/schemas/
```

Expected: six files —
`gate-v1.json`, `rbac-check-v1.json`, `rbac-print-v1.json`, `scan-v1.json`,
`watch-explanations-v1.json`, `watch-issues-v1.json`.

- [ ] **Step 6: Read the generated scan document before trusting it**

```bash
python3 -m json.tool website/docs/schemas/scan-v1.json > /dev/null && echo "valid JSON"
head -40 website/docs/schemas/scan-v1.json
grep -c '"\$ref"' website/docs/schemas/scan-v1.json
grep -o '"enum": \[[^]]*\]' website/docs/schemas/gate-v1.json
```

Check by eye: `$id` ends `scan-v1.json`; `schemaVersion` carries the
`^1\.[0-9]+$` pattern; `$defs` holds `findings.Finding`-style keys; the gate
document's `failOn` is a string with the three level spellings, **not** an
integer. Confirm no path, hostname or IP appears anywhere:

```bash
grep -nE '(/home/|/root/|kubeconfig|10\.[0-9]|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)' website/docs/schemas/*.json || echo "clean"
```

Expected: `clean`.

- [ ] **Step 7: Confirm the drift test now passes and is not vacuous**

```bash
go test ./internal/schemadoc -p 2
# Prove the test actually bites:
python3 - <<'PY'
import json
p = "website/docs/schemas/gate-v1.json"
d = json.load(open(p))
d["properties"]["verdict"]["type"] = "integer"
open(p, "w").write(json.dumps(d, indent=2) + "\n")
PY
go test ./internal/schemadoc -run TestSchemaDrift -p 2 2>&1 | head -20
go test ./internal/schemadoc -run TestSchemaDrift -update -p 2
go test ./internal/schemadoc -p 2
```

Expected: the middle run FAILS and prints `BREAKING  changed:
/properties/verdict/type (integer → string)` and the MAJOR-bump instruction; the
final run passes again.

- [ ] **Step 8: Run the whole suite**

```bash
go test ./... -p 2 2>&1 | tail -40
```

- [ ] **Step 9: Commit**

```bash
git add internal/schemadoc website/docs/schemas
git commit -s -m "feat(schemadoc): generate, publish and drift-test the six JSON schemas"
```

---

## Task 5: `kubeagent schema [name]`

**Files:**

- Modify: `main.go` (dispatch at ~line 128-147, the usage string at ~line 147,
  and a new `runSchema`)
- Test: `main_test.go`

**Interfaces:**

- Consumes: `schemadoc.Documents`, `schemadoc.Generate`, `schemadoc.Names`.
- Produces: `func runSchema(args []string, w io.Writer) error`. It takes a
  writer so a test can capture output without redirecting `os.Stdout`.

- [ ] **Step 1: Write the failing tests**

Add to `main_test.go`:

```go
func TestRunSchema_ListsEveryDocument(t *testing.T) {
	var out bytes.Buffer
	if err := runSchema(nil, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	for _, d := range schemadoc.Documents {
		if !strings.Contains(out.String(), d.Name) {
			t.Errorf("listing does not mention %q:\n%s", d.Name, out.String())
		}
	}
	if !strings.Contains(out.String(), invokedAs+" schema") {
		t.Errorf("listing does not show how to print one:\n%s", out.String())
	}
}

func TestRunSchema_PrintsAValidDocument(t *testing.T) {
	var out bytes.Buffer
	if err := runSchema([]string{"scan"}, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got, _ := doc["$id"].(string); !strings.HasSuffix(got, "scan-v1.json") {
		t.Errorf("$id = %q, want it to end in scan-v1.json", got)
	}
}

// What the binary prints must be what the binary's types are: the committed
// file and the runtime output come from one code path, and this proves it.
func TestRunSchema_MatchesTheCommittedFile(t *testing.T) {
	var out bytes.Buffer
	if err := runSchema([]string{"gate"}, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("website", "docs", "schemas", "gate-v1.json"))
	if err != nil {
		t.Fatalf("read the committed file: %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Error("`schema gate` does not match website/docs/schemas/gate-v1.json")
	}
}

func TestRunSchema_UnknownNameNamesTheValidOnes(t *testing.T) {
	err := runSchema([]string{"nope"}, io.Discard)
	if err == nil {
		t.Fatal("want an error for an unknown document name")
	}
	if !strings.Contains(err.Error(), "scan") {
		t.Errorf("error %q does not name the valid documents", err)
	}
}

func TestRunSchema_RejectsExtraArguments(t *testing.T) {
	if err := runSchema([]string{"scan", "gate"}, io.Discard); err == nil {
		t.Fatal("want a usage error for two document names")
	}
}

// The command reads Go types: no kubeconfig, no cluster, no LLM call.
func TestRun_SchemaNeedsNoKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := run([]string{"schema", "scan"}); err != nil {
		t.Errorf("schema must not need a cluster: %v", err)
	}
}

func TestRun_UsageMentionsSchemaCommand(t *testing.T) {
	err := run(nil)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("usage does not mention the schema command: %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -p 2 2>&1 | head -20
```

Expected: `undefined: runSchema`.

- [ ] **Step 3: Implement the command**

Add to `main.go`, next to the other `run*` functions:

```go
// runSchema prints the JSON Schema for one machine-readable document, or lists
// them all. Generated at runtime from the same code path that writes the
// committed files, so what the binary prints is what the binary's types are —
// there is no embedded copy that could drift.
//
// Read-only in the strongest sense: it reads Go types. No cluster connection, no
// kubeconfig, no LLM call.
func runSchema(args []string, w io.Writer) error {
	if len(args) == 0 {
		for _, d := range schemadoc.Documents {
			fmt.Fprintf(w, "  %-19s surface %-6s v%s\n", d.Name, d.Surface, d.Version)
		}
		fmt.Fprintf(w, "\nPrint one:\n  %s schema <name>\n", invokedAs)
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: %s schema [name]", invokedAs)
	}
	doc, err := schemadoc.Generate(args[0])
	if err != nil {
		return err
	}
	_, err = w.Write(doc)
	return err
}
```

Add the dispatch in `run`, after the `rbac` clause and before the scan fallback:

```go
	if len(args) > 0 && args[0] == "schema" {
		return runSchema(args[1:], os.Stdout)
	}
```

Extend the usage string: insert `| %[1]s schema [name]` immediately before
`| %[1]s version`.

Add the `internal/schemadoc` import (and `io` if not already imported).

- [ ] **Step 4: Run the tests and watch them pass**

```bash
go test . -p 2
go build -o /tmp/kubeagent-t5 . && /tmp/kubeagent-t5 schema && /tmp/kubeagent-t5 schema rbac-print | head -8
```

Expected: the listing shows six documents and the `schema <name>` hint using the
invoked name; `schema rbac-print` prints a document.

- [ ] **Step 5: Confirm the plugin spelling reads correctly**

```bash
cp /tmp/kubeagent-t5 /tmp/kubectl-kubeagent && /tmp/kubectl-kubeagent schema | tail -3
/tmp/kubectl-kubeagent 2>&1 | grep -o 'kubectl-kubeagent schema \[name\]'
```

Expected: the hint reads `kubectl-kubeagent schema <name>`, and the usage line
names the schema command with the invoked spelling — `invokedAs`, not a
hardcoded `kubeagent`.

- [ ] **Step 6: Run the whole suite and commit**

```bash
go test ./... -p 2 2>&1 | tail -20
git add main.go main_test.go
git commit -s -m "feat: add the schema command"
```

---

## Task 6: Documentation

**Files:**

- Create: `website/docs/features/json-schema.md`
- Modify: `website/mkdocs.yml` (nav, after `Least-privilege RBAC`)
- Modify: `website/docs/features/ci-gate.md`, `website/docs/features/rbac.md`,
  `website/docs/features/watch-mode.md`, `website/docs/quickstart.md`
  (one cross-link each)
- Modify: `CHANGELOG.md` (`[Unreleased]`, `### Added` and `### Changed`)
- Modify: `website/docs/roadmap.md` (Theme H bullet)
- Modify: `CLAUDE.md` (invariant + Theme H paragraph)
- Modify: `docs/go-concepts.md` (reflection entry — **gitignored, do not `git add`**)

- [ ] **Step 1: Write the contract page**

Create `website/docs/features/json-schema.md`. It must cover, in this order:

1. **What is versioned.** The table of four surfaces and six documents, matching
   the spec's:

   | Document | Surface | Emitted by |
   |---|---|---|
   | `scan` | scan | `kubeagent scan --output json` |
   | `gate` | gate | `kubeagent gate --output json` |
   | `rbac-print` | rbac | `kubeagent rbac print --output json` |
   | `rbac-check` | rbac | `kubeagent rbac check --output json` |
   | `watch-issues` | watch | the watch daemon's `GET /issues` |
   | `watch-explanations` | watch | the watch daemon's `GET /explanations` |

   And what is deliberately **out of scope**, with the reason for each: SARIF
   (versioned by OASIS), the slack and alertmanager alert payloads (the
   receiver's shapes), the `--fix` audit journal (a write-side record),
   `/metrics` (Prometheus text, already versioned), `/healthz` and `/readyz`
   (plain text).

2. **What MINOR and MAJOR mean.** A MINOR bump adds a field or an enum value; a
   parser written against `1.0` still works against `1.3`. A MAJOR bump removes
   or renames a field, changes a type, makes an always-present field optional,
   or removes an enum value. All four surfaces start at `1.0`, and the schema
   version is **not** the kubeagent release version.

3. **How to pin.** Compare `schemaVersion`'s major; treat an unknown minor as
   compatible. Show it:

   ```bash
   major=$(kubeagent scan --output json | jq -r '.schemaVersion | split(".")[0]')
   [ "$major" = "1" ] || { echo "unsupported scan schema"; exit 1; }
   ```

4. **What is not promised.** The order of object properties. The order of array
   elements unless a list is documented as sorted. The exact wording of any
   human-readable string — `reason`, `summary`, `evidence`, `explanation` are
   prose for an operator, not data to match on. Anything under `explanation` or
   `investigation`, which is model output. Say plainly that matching on a
   `reason` string will break, and that the stable thing to match is `issue`.

5. **The published schemas**, with the resolving URLs:

   ```text
   https://k8sproject.top/schemas/scan-v1.json
   https://k8sproject.top/schemas/gate-v1.json
   https://k8sproject.top/schemas/rbac-print-v1.json
   https://k8sproject.top/schemas/rbac-check-v1.json
   https://k8sproject.top/schemas/watch-issues-v1.json
   https://k8sproject.top/schemas/watch-explanations-v1.json
   ```

   The `$id` carries the major only, so a minor bump does not move a pinned URL.
   A major bump publishes a new file beside the old one; the old file stays,
   because a document already in someone's CI does not stop existing when
   kubeagent moves on.

6. **Validating a captured document offline**, with a runnable example using a
   generally available validator and the note that `additionalProperties` is
   deliberately open, so a newer document validates against an older schema of
   the same major.

7. **The `kubeagent schema` command**:

   ```bash
   kubeagent schema              # list the documents, their surfaces and versions
   kubeagent schema scan         # print one to stdout
   ```

   Note that it needs no cluster and no kubeconfig, and that it prints what the
   running binary's types are.

8. **The `rbac` shape change**, prominently, with the before and after and the
   one-liner for anyone who needs the old shape:

   ```bash
   kubeagent rbac print --output json | jq '.rules'      # the pre-0.71 array
   kubeagent rbac check --output json | jq '.features'   # likewise
   ```

- [ ] **Step 2: Wire the nav and the cross-links**

In `website/mkdocs.yml`, after the `Least-privilege RBAC` line:

```yaml
      - JSON schema contract: features/json-schema.md
```

Add one line to each of `ci-gate.md`, `rbac.md` and `watch-mode.md` pointing at
the new page from wherever each already discusses its JSON output — for example:

```markdown
The shape of this document is versioned; see [JSON schema contract](json-schema.md).
```

In `website/docs/quickstart.md`, add the same pointer where `--output json` is
introduced (relative link `features/json-schema.md` from there).

- [ ] **Step 3: CHANGELOG**

Under `## [Unreleased]`, in `### Added`:

```markdown
- **Versioned JSON output** — every machine-readable document now declares a
  `schemaVersion` (`scan`, `gate`, `rbac print`, `rbac check`, and the watch
  daemon's `/issues` and `/explanations`), and each surface's JSON Schema is
  generated from the Go types and published at
  `https://k8sproject.top/schemas/<name>-v1.json`. A drift test fails when a
  document's shape moves without a version bump, and says whether the change was
  additive or breaking. See
  [the JSON schema contract](https://k8sproject.top/features/json-schema/).
- **`kubeagent schema [name]`** — print the schema of any output document, or
  list them. Generated at runtime from the running binary's own types; needs no
  cluster and no kubeconfig.
```

And in `### Changed` — the breaking change stated plainly:

```markdown
- **Breaking: `rbac print --output json` and `rbac check --output json` now emit
  an object, not a bare array.** An array root cannot carry a version field, so
  the rules moved under `rules` (alongside `roleName`) and the feature statuses
  under `features`, each beside a `schemaVersion`. Taken deliberately before
  1.0: an unversioned array root could never gain a version later without
  exactly this break. Recover the old shape with
  `jq '.rules'` or `jq '.features'`.
```

- [ ] **Step 4: Roadmap and CLAUDE.md**

In `website/docs/roadmap.md`, extend the Theme H entry to record that the
versioned JSON schema slice has shipped, in the style the neighbouring shipped
slices use (name the command, the published URLs and the drift test).

In `CLAUDE.md`, add to the invariants list:

```markdown
- **`internal/jsonschema` imports nothing from kubeagent** — it is the schema
  generator, importable by every surface package including the ones that may not
  import `internal/remediate` or `internal/explain`. `internal/schemadoc` is the
  opposite case and deliberately so: it imports the four surface packages to
  name the six document roots, so it transitively reaches `remediate` and
  `explain`. That is allowed — the invariants constrain what those packages
  import, not who imports them — and only `main.go` and `schemadoc`'s own tests
  import it. It holds no client and no context and makes no call.
- **The six JSON documents are a versioned contract.** Changing a field name, a
  type, or an enum value in `report.ScanReport`, `gate.Verdict`,
  `rbacprofile.RulesDocument`, `rbacprofile.CheckDocument`,
  `watch.IssuesReport` or `watch.ExplanationsReport` means bumping the surface's
  version in `internal/jsonschema` and regenerating with
  `go test ./internal/schemadoc -run TestSchemaDrift -update`. The drift test
  says whether the change was additive (MINOR) or breaking (MAJOR).
```

Extend the Theme H paragraph in the same file to name this slice as shipped,
matching how slices 1 and 2 are recorded.

- [ ] **Step 5: `docs/go-concepts.md` — reflection**

`reflect` appears in no production file in kubeagent before this branch, so it
is a new concept. Append a section in the established style — **a plain everyday
example first, then the kubeagent example**, and no Python comparisons. Match
the heading style of §§1-20 (not §21's). Sketch:

- The plain example: a `struct` and a loop over `reflect.TypeOf(x).Field(i)`
  reading a name and a tag, showing that reflection lets a program read a type's
  own shape at run time.
- Why a type would want that: `encoding/json` already does exactly this, which
  is why a `json:"…"` tag works at all.
- The kubeagent example: `internal/jsonschema.Generate` walking
  `report.ScanReport` to write its own JSON Schema, and the one thing reflection
  cannot do — see a `const` block — which is why the enum table exists and why a
  `go/ast` test guards it.

**Do not `git add docs/go-concepts.md`** — it is gitignored.

- [ ] **Step 6: Build the site and check for secrets**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
(cd website && mkdocs build --strict -f mkdocs.yml) 2>&1 | tail -20
grep -rnE '(/home/|/root/|/\.kube/|10\.[0-9]+\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)' website/docs/features/json-schema.md || echo "clean"
```

Expected: `Documentation built` with no `WARNING` about the new page (the red
"Material for MkDocs 2.0" banner is cosmetic); `clean` from the grep. Confirm
`website/site/schemas/scan-v1.json` exists in the build output, so the `$id`
resolves.

- [ ] **Step 7: Full suite, DCO check, commit**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2 2>&1 | tail -20
git add website CHANGELOG.md CLAUDE.md
git commit -s -m "docs: the versioned JSON schema contract"
scripts/dco-check.sh
```

Expected: no test failures; the DCO check passes for every commit on the branch.

---

## Self-Review

**Spec coverage.** Each spec section maps to a task: the surfaces table and the
version field → Task 3; the generator, its mapping rules, the root-version
pattern and the two things reflection gets wrong → Tasks 1 and 4; the document
registry and root exports → Tasks 2 and 4; publishing and the drift test → Task
4; the `schema` command → Task 5; the written contract, CHANGELOG, roadmap,
CLAUDE.md and go-concepts → Task 6. The spec's Gate section is not a task — it
runs after the branch review, per the section below.

**Type consistency.** `report.ScanReport`, `gate.Verdict`,
`rbacprofile.RulesDocument`, `rbacprofile.CheckDocument`, `watch.IssuesReport`
and `watch.ExplanationsReport` are named identically in Tasks 2, 3, 4 and the
tests. `jsonschema.Generate`, `Meta`, `Schema`, `TypeKey`, `Major` and the four
version constants are declared in Task 1 and used with the same signatures in
Tasks 3, 4 and 5. `schemadoc.Generate`, `Names`, `Path` and `Documents` are
declared in Task 4 and used in Task 5.

**Known ambiguity, resolved here so no implementer has to guess.** The spec's
mapping table says `map[string]T`; the scan graph actually holds
`map[gitops.State]int` and `map[operators.State]int`. The rule keys off the key
type's *kind*, and an enum-typed key also emits `propertyNames`. Both `Counts`
fields lack `omitempty`, so both are typed `["object", "null"]` — a nil map
marshals to `null`, and the code happening to initialize them makes the schema
permissive rather than wrong.

## After the branch

1. `superpowers:finishing-a-development-branch`.
2. The **full chaos gate** — `./chaos/run.sh --recreate` with
   `unset ANTHROPIC_API_KEY`. This branch touches `internal/watch` and
   `internal/rbacprofile`, and the standing rule is that anything touching the
   watch daemon or RBAC gets the full gate, overriding the decomposition's
   "smoke".
3. While the chaos cluster is up, check the four outputs the unit tests cannot:
   `rbac print --output json`, `rbac check --output json`, and the daemon's
   `/issues` and `/explanations` — each must open with `"schemaVersion": "1.0"`.
4. Release and checkpoint.
