package watch

import (
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/slo"
)

// sloBase is a fixed instant for the SLO tests. Deliberately not named `base`:
// metrics_test.go already uses that name as a function-local, and a
// package-level `base` would be silently shadowed there.
var sloBase = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestRender_OmitsSLOSeriesWhenDisabled(t *testing.T) {
	m := newMetrics()
	out := m.render()
	if strings.Contains(out, "kubeagent_slo_") {
		t.Error("SLO series rendered while SLO tracking is off; --slo-target unset must mean no series")
	}
}

func TestRender_SLOSeries(t *testing.T) {
	m := newMetrics()
	m.updateSLO(true, 0.999,
		slo.Report{Window: slo.Fast, Availability: 0.99, BurnRate: 10, Coverage: 1},
		slo.Report{Window: slo.Slow, Availability: 0.995, BurnRate: 5, Coverage: 0.75},
	)
	out := m.render()
	for _, want := range []string{
		"kubeagent_slo_target_ratio 0.999",
		`kubeagent_slo_availability_ratio{window="fast"} 0.99`,
		`kubeagent_slo_availability_ratio{window="slow"} 0.995`,
		`kubeagent_slo_burn_rate{window="fast"} 10`,
		`kubeagent_slo_burn_rate{window="slow"} 5`,
		`kubeagent_slo_window_coverage_ratio{window="fast"} 1`,
		`kubeagent_slo_window_coverage_ratio{window="slow"} 0.75`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in:\n%s", want, out)
		}
	}
}

func TestRender_ErrorBudgetRemaining(t *testing.T) {
	cases := []struct {
		name     string
		slowBurn float64
		want     string
	}{
		{"quarter spent", 0.25, "kubeagent_slo_error_budget_remaining_ratio 0.75"},
		{"exactly spent", 1, "kubeagent_slo_error_budget_remaining_ratio 0"},
		{"overspent clamps at zero", 12, "kubeagent_slo_error_budget_remaining_ratio 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMetrics()
			m.updateSLO(true, 0.999,
				slo.Report{Window: slo.Fast},
				slo.Report{Window: slo.Slow, BurnRate: c.slowBurn},
			)
			if out := m.render(); !strings.Contains(out, c.want) {
				t.Errorf("missing %q in:\n%s", c.want, out)
			}
		})
	}
}

func TestRender_SLODoesNotTouchIssueSeries(t *testing.T) {
	// The burn signal must never inflate the object-issue gauges. An operator
	// reading kubeagent_issues_active as "how many objects are broken" must not
	// see a budget breach counted there.
	m := newMetrics()
	m.updateSLO(true, 0.999,
		slo.Report{Window: slo.Fast, BurnRate: 50, Coverage: 1},
		slo.Report{Window: slo.Slow, BurnRate: 50, Coverage: 1},
	)
	out := m.render()
	if !strings.Contains(out, "kubeagent_issues_active 0") {
		t.Error("kubeagent_issues_active moved off zero because of an SLO update")
	}
}
