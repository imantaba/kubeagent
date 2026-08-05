package alert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

// Format is the wire format the receiver expects.
type Format string

const (
	FormatJSON         Format = "json"
	FormatSlack        Format = "slack"
	FormatAlertmanager Format = "alertmanager"
	FormatPagerDuty    Format = "pagerduty"
)

// encode renders one notification in the configured format. routingKey is the
// PagerDuty integration key and is ignored by every other format: PagerDuty is
// the one receiver that authenticates in the request body rather than with the
// URL itself.
func encode(f Format, routingKey string, n alertstate.Notification) ([]byte, error) {
	switch f {
	case FormatJSON:
		return encodeJSON(n)
	case FormatSlack:
		return encodeSlack(n)
	case FormatAlertmanager:
		return encodeAlertmanager(n)
	case FormatPagerDuty:
		return encodePagerDuty(routingKey, n)
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, alertmanager, or pagerduty)", f)
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

// PagerDuty Events API v2 constants.
const (
	// pdSeverity is a constant because kubeagent has no severity model on
	// diagnose.Finding. "error" is the same default Alertmanager's own PagerDuty
	// notifier picks, and it is the honest middle: kubeagent knows something is
	// broken, not how badly.
	pdSeverity = "error"

	// pdMaxDedupKey is PagerDuty's documented dedup_key cap. It is applied in
	// bytes, which satisfies both readings of "255 characters": a string of at
	// most 255 bytes is also at most 255 characters, whatever the encoding.
	pdMaxDedupKey = 255
	// pdDedupKeyPrefix leaves room for the "/" separator and the 8-hex digest.
	pdDedupKeyPrefix = pdMaxDedupKey - 1 - 8

	// pdMaxSummary matches what Alertmanager's PagerDuty notifier allows.
	pdMaxSummary = 1024
)

// pdEvent is one PagerDuty Events API v2 event. Payload is a pointer so a
// resolve can omit it entirely: PagerDuty requires only routing_key,
// event_action and dedup_key to close an incident, and it computes the incident
// duration itself — anything kubeagent added would be a second copy free to
// disagree with the first.
type pdEvent struct {
	RoutingKey  string     `json:"routing_key"`
	EventAction string     `json:"event_action"`
	DedupKey    string     `json:"dedup_key"`
	Payload     *pdPayload `json:"payload,omitempty"`
}

type pdPayload struct {
	Summary       string    `json:"summary"`
	Source        string    `json:"source"`
	Severity      string    `json:"severity"`
	Timestamp     string    `json:"timestamp"`
	CustomDetails pdDetails `json:"custom_details"`
}

// pdDetails is a struct rather than a map[string]any so the shape is documented
// by the type and cannot pick up a stray key, and so the encoded field order is
// fixed — a map would iterate differently on every call.
type pdDetails struct {
	Cluster     string   `json:"cluster"`
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	Reason      string   `json:"reason"`
	Flapping    bool     `json:"flapping"`
	Explanation string   `json:"explanation,omitempty"`
}

// encodePagerDuty renders one Events API v2 event. An explanation is a trigger
// on the same dedup_key rather than a new event kind: the object is still
// firing, and an explanation is additional detail about one state rather than a
// transition to another.
func encodePagerDuty(routingKey string, n alertstate.Notification) ([]byte, error) {
	e := pdEvent{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		DedupKey:    dedupKey(n.Object),
	}
	if n.Status == alertstate.StatusResolved {
		e.EventAction = "resolve"
		return json.Marshal(e)
	}
	issues := n.Issues
	if issues == nil {
		issues = []string{}
	}
	e.Payload = &pdPayload{
		Summary:  pdSummary(n),
		Source:   n.Object.Cluster,
		Severity: pdSeverity,
		// FiringSince, not the current wall clock: the alert is stamped when the
		// object actually broke rather than when the daemon noticed.
		Timestamp: n.FiringSince.UTC().Format(time.RFC3339),
		CustomDetails: pdDetails{
			Cluster:     n.Object.Cluster,
			Kind:        n.Object.Kind,
			Namespace:   n.Object.Namespace,
			Name:        n.Object.Name,
			Issues:      issues,
			Reason:      string(n.Reason),
			Flapping:    n.Flapping,
			Explanation: n.Text,
		},
	}
	return json.Marshal(e)
}

// dedupKey identifies the incident. It is Object.String() — derived from
// identity, not from state, so it survives a daemon restart and the restart's
// re-trigger lands on the open alert instead of opening a second one. An
// over-long key keeps a readable prefix and appends a digest of the whole
// string, so two objects that share the first 246 bytes still get two
// incidents.
func dedupKey(o alertstate.Object) string {
	s := o.String()
	if len(s) <= pdMaxDedupKey {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return trimBytes(s, pdDedupKeyPrefix) + "/" + hex.EncodeToString(sum[:4])
}

// pdSummary is the line a human reads in a push notification at 3am, so it is
// the object and its issues and nothing else. Model-written prose never enters
// it: n.Text can run to paragraphs and travels in custom_details instead.
func pdSummary(n alertstate.Notification) string {
	s := n.Object.String()
	if len(n.Issues) > 0 {
		s += ": " + strings.Join(n.Issues, ", ")
	}
	return trimRunes(s, pdMaxSummary)
}

// trimBytes cuts s to at most n bytes without splitting a rune. Cutting one in
// half would leave a byte that json.Marshal replaces with U+FFFD — three bytes
// where one was cut, which is mojibake in an operator-facing incident key.
func trimBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// trimRunes cuts s to at most n runes.
func trimRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// validateRoutingKey rejects a value that cannot be an integration key: empty,
// or carrying a space or any byte outside printable ASCII — which is what a
// Secret with a trailing newline or a pasted multi-line blob looks like.
// Catching it at startup beats catching it at the first HTTP 400. The error
// names the variable to fix and never echoes the value.
//
// The length is deliberately not checked against PagerDuty's 32 characters:
// pinning an upstream length is a hostage to fortune, and it would force every
// fixture to be a 32-character string that reads like a real key.
func validateRoutingKey(key string) error {
	if key == "" {
		return errors.New("the pagerduty alert format needs KUBEAGENT_ALERT_ROUTING_KEY set to the integration key")
	}
	for i := 0; i < len(key); i++ {
		if b := key[i]; b <= ' ' || b > '~' {
			return errors.New("KUBEAGENT_ALERT_ROUTING_KEY must be one token of printable ASCII with no spaces (a trailing newline from a Secret is the usual cause)")
		}
	}
	return nil
}
