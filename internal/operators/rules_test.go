package operators

import (
	"strings"
	"testing"
)

// condObj builds a CR carrying one status condition.
func condObj(condType, status, reason, message string) map[string]any {
	c := map[string]any{"type": condType, "status": status}
	if reason != "" {
		c["reason"] = reason
	}
	if message != "" {
		c["message"] = message
	}
	return map[string]any{"status": map[string]any{"conditions": []any{c}}}
}

func TestConditionRule_TrueIsHealthyWithNoReason(t *testing.T) {
	state, reason := ConditionRule{Type: "Ready"}.Evaluate(condObj("Ready", "True", "Issued", ""))
	if state != StateHealthy {
		t.Errorf("state = %q, want %q", state, StateHealthy)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty (healthy resources are never enumerated)", reason)
	}
}

func TestConditionRule_FalseIsUnhealthyAndNamesTheReason(t *testing.T) {
	state, reason := ConditionRule{Type: "Ready"}.Evaluate(condObj("Ready", "False", "IssuerNotFound", ""))
	if state != StateUnhealthy {
		t.Errorf("state = %q, want %q", state, StateUnhealthy)
	}
	if reason != "Ready=False: IssuerNotFound" {
		t.Errorf("reason = %q, want %q", reason, "Ready=False: IssuerNotFound")
	}
}

func TestConditionRule_UnknownIsProgressing(t *testing.T) {
	state, _ := ConditionRule{Type: "Ready"}.Evaluate(condObj("Ready", "Unknown", "Pending", ""))
	if state != StateProgressing {
		t.Errorf("state = %q, want %q", state, StateProgressing)
	}
}

func TestConditionRule_MissingConditionIsUnknown(t *testing.T) {
	// The condition list exists but carries a different type: kubeagent cannot
	// tell, and "cannot tell" must never read as an outage.
	state, _ := ConditionRule{Type: "Ready"}.Evaluate(condObj("Synced", "False", "Whatever", ""))
	if state != StateUnknown {
		t.Errorf("state = %q, want %q", state, StateUnknown)
	}
}

func TestConditionRule_NoStatusAtAllIsUnknown(t *testing.T) {
	state, _ := ConditionRule{Type: "Ready"}.Evaluate(map[string]any{"spec": map[string]any{}})
	if state != StateUnknown {
		t.Errorf("state = %q, want %q", state, StateUnknown)
	}
}

func TestConditionRule_NeverReportsTheConditionMessage(t *testing.T) {
	// A condition message is arbitrary operator prose. cert-manager puts ACME
	// order URLs in it, and a URL can carry a token — only the reason is read.
	obj := condObj("Ready", "False", "Pending",
		"waiting on order https://acme.example.invalid/order/abc?token=LEAKED")
	_, reason := ConditionRule{Type: "Ready"}.Evaluate(obj)
	for _, bad := range []string{"acme.example.invalid", "LEAKED", "https://"} {
		if strings.Contains(reason, bad) {
			t.Errorf("reason %q leaked the condition message (found %q)", reason, bad)
		}
	}
}

func TestConditionRule_ReasonIsSingleLineAndCapped(t *testing.T) {
	obj := condObj("Ready", "False", strings.Repeat("x", 400)+"\nsecond line", "")
	_, reason := ConditionRule{Type: "Ready"}.Evaluate(obj)
	if strings.ContainsAny(reason, "\r\n") {
		t.Errorf("reason %q spans more than one line", reason)
	}
	if n := len([]rune(reason)); n > maxReasonRunes+1 { // +1 for the ellipsis
		t.Errorf("reason is %d runes, want at most %d", n, maxReasonRunes+1)
	}
}

func TestFieldRule_MapsEachValueSet(t *testing.T) {
	rule := FieldRule{
		Path:        []string{"status", "health", "status"},
		Healthy:     []string{"Healthy"},
		Progressing: []string{"Progressing"},
		Unhealthy:   []string{"Degraded", "Missing"},
		Suspended:   []string{"Suspended"},
	}
	cases := map[string]State{
		"Healthy":     StateHealthy,
		"Progressing": StateProgressing,
		"Degraded":    StateUnhealthy,
		"Missing":     StateUnhealthy,
		"Suspended":   StateSuspended,
	}
	for value, want := range cases {
		obj := map[string]any{"status": map[string]any{"health": map[string]any{"status": value}}}
		if got, _ := rule.Evaluate(obj); got != want {
			t.Errorf("value %q: state = %q, want %q", value, got, want)
		}
	}
}

func TestFieldRule_UnrecognizedValueIsUnknown(t *testing.T) {
	// A value from a CRD version kubeagent has not seen must not be an outage.
	rule := FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}, Unhealthy: []string{"degraded"}}
	obj := map[string]any{"status": map[string]any{"robustness": "rebuilding-v3"}}
	if got, _ := rule.Evaluate(obj); got != StateUnknown {
		t.Errorf("state = %q, want %q", got, StateUnknown)
	}
}

func TestFieldRule_MissingPathIsUnknown(t *testing.T) {
	rule := FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}}
	if got, _ := rule.Evaluate(map[string]any{"status": map[string]any{}}); got != StateUnknown {
		t.Errorf("state = %q, want %q", got, StateUnknown)
	}
}

func TestFieldRule_ReasonNamesTheFieldBelowStatus(t *testing.T) {
	rule := FieldRule{Path: []string{"status", "robustness"}, Unhealthy: []string{"degraded"}}
	obj := map[string]any{"status": map[string]any{"robustness": "degraded"}}
	_, reason := rule.Evaluate(obj)
	if reason != "robustness=degraded" {
		t.Errorf("reason = %q, want %q", reason, "robustness=degraded")
	}

	nested := FieldRule{Path: []string{"status", "health", "status"}, Unhealthy: []string{"Degraded"}}
	obj = map[string]any{"status": map[string]any{"health": map[string]any{"status": "Degraded"}}}
	_, reason = nested.Evaluate(obj)
	if reason != "health.status=Degraded" {
		t.Errorf("reason = %q, want %q", reason, "health.status=Degraded")
	}
}

func TestFieldRule_HealthyReportsNoReason(t *testing.T) {
	rule := FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}}
	obj := map[string]any{"status": map[string]any{"robustness": "healthy"}}
	if _, reason := rule.Evaluate(obj); reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}
