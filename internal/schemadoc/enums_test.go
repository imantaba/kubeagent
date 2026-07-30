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
