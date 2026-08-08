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

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/fleet"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/gitops"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/operators"
	"github.com/imantaba/kubeagent/internal/policy"
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
	{
		Name: "baseline", Surface: "baseline", Version: jsonschema.BaselineVersion,
		Root:        reflect.TypeOf(baseline.Document{}),
		Title:       "kubeagent restart-rate baseline",
		Description: "kubeagent's learned restart-rate baseline for a cluster: one learned restart rate per workload, the minimum pod age behind it, and when it was captured.",
	},
	{
		Name: "fleet", Surface: "fleet", Version: jsonschema.FleetVersion,
		Root:        reflect.TypeOf(fleet.Report{}),
		Title:       "kubeagent fleet report",
		Description: "The document written by `kubeagent fleet --output json`: one summary per selected cluster, worst first, plus the clusters that could not be judged. A summary carries counts and issue kinds — it never names a node, namespace, pod or workload.",
	},
}

// enums is every named type in the eight graphs whose values are a closed set.
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
	"policy.Level": {
		string(policy.LevelInfo), string(policy.LevelWarning), string(policy.LevelCritical),
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
