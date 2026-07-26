package alert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

// Format is the wire format the receiver expects.
type Format string

const (
	FormatJSON         Format = "json"
	FormatSlack        Format = "slack"
	FormatAlertmanager Format = "alertmanager"
)

// encode renders one notification in the configured format.
func encode(f Format, n alertstate.Notification) ([]byte, error) {
	switch f {
	case FormatJSON:
		return encodeJSON(n)
	case FormatSlack:
		return encodeSlack(n)
	case FormatAlertmanager:
		return encodeAlertmanager(n)
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, or alertmanager)", f)
	}
}

// jsonPayload is kubeagent's native alert body.
type jsonPayload struct {
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	Cluster     string   `json:"cluster"`
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	Text        string   `json:"text,omitempty"`
	FiringSince string   `json:"firingSince"`
	ResolvedAt  string   `json:"resolvedAt,omitempty"`
	Flapping    bool     `json:"flapping"`
}

func encodeJSON(n alertstate.Notification) ([]byte, error) {
	issues := n.Issues
	if issues == nil {
		issues = []string{}
	}
	p := jsonPayload{
		Status:      string(n.Status),
		Reason:      string(n.Reason),
		Cluster:     n.Object.Cluster,
		Kind:        n.Object.Kind,
		Namespace:   n.Object.Namespace,
		Name:        n.Object.Name,
		Issues:      issues,
		Text:        n.Text,
		FiringSince: n.FiringSince.UTC().Format(time.RFC3339),
		Flapping:    n.Flapping,
	}
	if !n.ResolvedAt.IsZero() {
		p.ResolvedAt = n.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(p)
}

func encodeSlack(n alertstate.Notification) ([]byte, error) {
	var text string
	switch {
	case n.Reason == alertstate.ReasonExplanation:
		text = fmt.Sprintf("*EXPLANATION* %s\n%s", n.Object, n.Text)
	case n.Status == alertstate.StatusResolved:
		text = fmt.Sprintf("*RESOLVED* %s (fired for %s)", n.Object, n.ResolvedAt.Sub(n.FiringSince).Round(time.Second))
	default:
		text = fmt.Sprintf("*FIRING* %s\nissues: %s\nfiring since %s",
			n.Object, strings.Join(n.Issues, ", "), n.FiringSince.UTC().Format(time.RFC3339))
		if n.Flapping {
			text += " (flapping)"
		}
	}
	return json.Marshal(struct {
		Text string `json:"text"`
	}{text})
}

// amAlert is one entry of Alertmanager's POST /api/v2/alerts array.
type amAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt,omitempty"`
}

// encodeAlertmanager keeps the issue list in an annotation rather than a label:
// the set changes as a failure evolves, and a changing label value would create a
// second Alertmanager alert instead of updating the open one — reintroducing the
// false-recovery problem the per-object rollup exists to prevent.
func encodeAlertmanager(n alertstate.Notification) ([]byte, error) {
	labels := map[string]string{
		"alertname": "KubeagentIssue",
		"cluster":   n.Object.Cluster,
		"kind":      n.Object.Kind,
		"name":      n.Object.Name,
	}
	if n.Object.Namespace != "" {
		labels["namespace"] = n.Object.Namespace
	}
	a := amAlert{
		Labels: labels,
		Annotations: map[string]string{
			"issues":   strings.Join(n.Issues, ","),
			"flapping": fmt.Sprintf("%t", n.Flapping),
		},
		StartsAt: n.FiringSince.UTC().Format(time.RFC3339),
	}
	if n.Text != "" {
		a.Annotations["explanation"] = n.Text
	}
	if n.Status == alertstate.StatusResolved {
		a.EndsAt = n.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal([]amAlert{a})
}
