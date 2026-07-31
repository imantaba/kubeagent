// Package sarif renders a gate verdict as a SARIF 2.1.0 document, so a CI
// pipeline can upload kubeagent's findings to GitHub code scanning.
//
// SARIF results are keyed to a physicalLocation — an artifact URI and, usually,
// a line. kubeagent findings are cluster objects: there is no file to point at.
// Rather than invent one, this renderer emits a synthetic
// "k8s://<namespace>/<Kind>/<name>" URI and no region. Mapping findings back to
// repo YAML is a separate, much larger problem (Helm, kustomize,
// operator-created objects) and is deliberately out of scope.
//
// Pure: no cluster calls, no LLM calls.
package sarif

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
)

// InformationURI is the public project page. It is not a credential, and is the
// only URL this package emits.
const InformationURI = "https://github.com/imantaba/kubeagent"

type document struct {
	Schema  string `json:"$schema"`
	Version string `json:"version"`
	Runs    []run  `json:"runs"`
}

type run struct {
	Tool        tool         `json:"tool"`
	Results     []result     `json:"results"`
	Invocations []invocation `json:"invocations"`
}

type tool struct {
	Driver driver `json:"driver"`
}

type driver struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	InformationURI string `json:"informationUri"`
	Rules          []rule `json:"rules"`
}

type rule struct {
	ID                   string        `json:"id"`
	Name                 string        `json:"name"`
	ShortDescription     text          `json:"shortDescription"`
	DefaultConfiguration configuration `json:"defaultConfiguration"`
}

type configuration struct {
	Level string `json:"level"`
}

type text struct {
	Text string `json:"text"`
}

type result struct {
	RuleID    string     `json:"ruleId"`
	Level     string     `json:"level"`
	Message   text       `json:"message"`
	Locations []location `json:"locations"`
}

type location struct {
	PhysicalLocation physicalLocation `json:"physicalLocation"`
}

type physicalLocation struct {
	ArtifactLocation artifactLocation `json:"artifactLocation"`
}

type artifactLocation struct {
	URI string `json:"uri"`
}

type invocation struct {
	ExecutionSuccessful            bool           `json:"executionSuccessful"`
	ToolConfigurationNotifications []notification `json:"toolConfigurationNotifications"`
}

type notification struct {
	Descriptor descriptor `json:"descriptor"`
	Level      string     `json:"level"`
	Message    text       `json:"message"`
}

type descriptor struct {
	ID string `json:"id"`
}

// levelFor maps kubeagent severity onto SARIF's.
func levelFor(l findings.Level) string {
	switch l {
	case findings.Critical:
		return "error"
	case findings.Warning:
		return "warning"
	default:
		return "note"
	}
}

// artifactURI names the object a finding is about. A finding with no object —
// a policy rule that could not be evaluated against anything — names its kind
// alone rather than a URI with a trailing empty segment.
func artifactURI(f findings.Finding) string {
	switch {
	case f.Name == "":
		return fmt.Sprintf("k8s://%s", f.Kind)
	case f.Namespace == "":
		return fmt.Sprintf("k8s://%s/%s", f.Kind, f.Name)
	}
	return fmt.Sprintf("k8s://%s/%s/%s", f.Namespace, f.Kind, f.Name)
}

// Render serializes v. Both Failing and Reported findings appear: a code
// scanning upload should show everything kubeagent saw, and the exit code —
// not the document — is what fails the build.
func Render(v gate.Verdict, toolVersion string) ([]byte, error) {
	all := make([]findings.Finding, 0, len(v.Failing)+len(v.Reported))
	all = append(all, v.Failing...)
	all = append(all, v.Reported...)
	findings.Sort(all)

	results := make([]result, 0, len(all))
	rulesByID := map[string]rule{}
	for _, f := range all {
		results = append(results, result{
			RuleID:  f.Issue,
			Level:   levelFor(f.Level),
			Message: text{Text: messageFor(f)},
			Locations: []location{{PhysicalLocation: physicalLocation{
				ArtifactLocation: artifactLocation{URI: artifactURI(f)},
			}}},
		})
		// Only rules that actually fired are declared. A static catalogue of
		// every detector would describe checks this run may never have run.
		if _, seen := rulesByID[f.Issue]; !seen {
			rulesByID[f.Issue] = rule{
				ID: f.Issue, Name: f.Issue,
				ShortDescription:     text{Text: f.Issue},
				DefaultConfiguration: configuration{Level: levelFor(f.Level)},
			}
		}
	}

	ruleIDs := make([]string, 0, len(rulesByID))
	for id := range rulesByID {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Strings(ruleIDs)
	rules := make([]rule, 0, len(ruleIDs))
	for _, id := range ruleIDs {
		rules = append(rules, rulesByID[id])
	}

	notifications := make([]notification, 0, len(v.Inconclusive))
	blind := false
	for _, b := range v.Inconclusive {
		if !b.Waived {
			blind = true
		}
		notifications = append(notifications, notification{
			Descriptor: descriptor{ID: "partial-read"},
			Level:      "error",
			Message:    text{Text: fmt.Sprintf("could not read %s: %s", b.Resource, b.Reason)},
		})
	}

	doc := document{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []run{{
			Tool: tool{Driver: driver{
				Name: "kubeagent", Version: toolVersion,
				InformationURI: InformationURI, Rules: rules,
			}},
			Results: results,
			Invocations: []invocation{{
				// An upload must not look clean when the gate never saw the
				// cluster, or when the rollout it was verifying never settled.
				ExecutionSuccessful:            !blind && v.Verdict != "timeout",
				ToolConfigurationNotifications: notifications,
			}},
		}},
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// messageFor is what a code scanning annotation shows.
func messageFor(f findings.Finding) string {
	if f.Reason == "" {
		return f.Issue
	}
	return f.Reason
}
