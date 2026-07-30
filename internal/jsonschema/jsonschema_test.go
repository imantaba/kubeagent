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
