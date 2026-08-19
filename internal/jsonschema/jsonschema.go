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
	// ScanVersion is `kubeagent scan --output json`. 1.1 added `policy`; 1.2
	// added `baseline`; 1.3 added `state` on a pod row, the kubectl-style
	// display value computed beside the raw `phase`, which is unchanged; 1.4
	// added `unreachable` on `nodehealth.Report`, the count of probed kubelets
	// whose /healthz never answered (transport failure or a 502/503/504 from
	// the proxy), tracked separately from `forbidden` so a permission problem
	// and a dead kubelet are not conflated; 1.5 added `podsAnswered` on
	// `dnshealth.Report`, the count of the probed CoreDNS pods that actually
	// returned a 200 from /metrics, tracked separately from `podsProbed` (the
	// count selected) so a partial read is visible in the JSON the same way it
	// already is in the text report; 1.6 added `suggestion` on a finding, the
	// deterministic next step `scan --suggest` already prints in the text
	// report, populated only when the flag is set; 1.7 added `kind` on a
	// finding, the kind of the object the finding's `pod` names when it is
	// not a pod ("Job" or "CronJob"), set only by the JobFailed producer;
	// 1.8 added `rootCauseTrace` and `rootCauseConfidence` on a workload row
	// — every root-cause candidate the attribution pass evaluated, each with
	// a closed-set verdict, plus the stored confidence of the winning
	// attribution. All nine are additive: every
	// added property is omitempty and absent from `required`, so a document
	// produced without them still validates against the older schema. `state`,
	// `unreachable`, `podsAnswered`, `suggestion` and `kind` are omitempty for
	// that reason and no other — `state` is set on every row of every real
	// scan, `podsAnswered` is set anywhere `podsProbed` is on a real scan, a
	// run with no unreachable kubelet legitimately encodes no `unreachable`
	// key, a scan without `--suggest` legitimately encodes no `suggestion`
	// key, a pod-level finding legitimately encodes no `kind` key, a workload
	// with no evaluated candidate legitimately encodes no `rootCauseTrace`
	// key and an unattributed one no `rootCauseConfidence` key, but a
	// property in `required` is a MAJOR change however new or however often
	// it is set.
	ScanVersion     = "1.8"
	GateVersion     = "1.1"
	RBACVersion     = "1.0"
	WatchVersion    = "1.0"
	BaselineVersion = "1.0"

	// FleetVersion is `kubeagent fleet --output json`. 1.1 added the optional
	// `shared` array; 1.2 added the optional `name` on a cluster summary and on
	// an unreachable cluster, written only when the row identity differs from
	// the kubeconfig context. Both bumps are additive: every property is
	// omitempty and absent from `required`, so a document produced without
	// them still validates against the older schema.
	FleetVersion = "1.2"
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
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		// encoding/json special-cases an anonymous struct-typed field: it
		// promotes the embedded type's exported fields even when the field
		// itself reads as unexported (an embedded type whose name starts
		// lowercase makes the field name — and so reflect's PkgPath check —
		// unexported too). So this has to run before the plain-unexported
		// skip below, or a flattened embed like this package's own tests
		// would be silently dropped instead of rejected.
		if f.Anonymous && name == "" {
			return nil, fmt.Errorf("%s.%s: an embedded field with no json name would be flattened; give it a name or project it into a named field", TypeKey(t), f.Name)
		}
		if f.PkgPath != "" { // unexported, non-embedded: encoding/json never emits it
			continue
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
