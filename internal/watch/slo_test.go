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
		// want must match a complete rendered line, not merely a prefix of one:
		// render() terminates every sample with "\n", so anchoring on that
		// terminator is enough to stop "...} 0.99" from matching "...} 0.995".
		// An unanchored Contains would let the fast window silently render the
		// slow window's value (or vice versa) and still pass.
		if !strings.Contains(out, want+"\n") {
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
			// Anchored on the line terminator for the same reason as
			// TestRender_SLOSeries: c.want ending in "0" is a literal prefix
			// of any wrong clamp value like "0.05", so an unanchored Contains
			// would pass even if the clamp were broken.
			if out := m.render(); !strings.Contains(out, c.want+"\n") {
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
	// Anchored for the same reason as the other assertions in this file:
	// "kubeagent_issues_active 0" is a literal prefix of "kubeagent_issues_active 0.5",
	// so an unanchored Contains would not actually catch the gauge moving off
	// exactly zero.
	if !strings.Contains(out, "kubeagent_issues_active 0\n") {
		t.Error("kubeagent_issues_active moved off zero because of an SLO update")
	}
}

// TestRender_SLOMetricSet pins down the exact set of kubeagent_slo_* series
// render() emits, and the exact number of samples among them. Without this,
// nothing would notice a sixth kubeagent_slo_* series being added, or one of
// the five being renamed while a duplicate under the old name lingers behind.
func TestRender_SLOMetricSet(t *testing.T) {
	m := newMetrics()
	m.updateSLO(true, 0.999,
		slo.Report{Window: slo.Fast, Availability: 0.99, BurnRate: 10, Coverage: 1},
		slo.Report{Window: slo.Slow, Availability: 0.995, BurnRate: 5, Coverage: 0.75},
	)
	out := m.render()

	want := map[string]bool{
		"kubeagent_slo_target_ratio":                 true,
		"kubeagent_slo_availability_ratio":           true,
		"kubeagent_slo_burn_rate":                    true,
		"kubeagent_slo_error_budget_remaining_ratio": true,
		"kubeagent_slo_window_coverage_ratio":        true,
	}

	got := map[string]bool{}
	samples := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		isComment := strings.HasPrefix(line, "#")
		var name string
		if isComment {
			// "# HELP metricname help text..." / "# TYPE metricname gauge"
			if !strings.HasPrefix(line, "# HELP ") && !strings.HasPrefix(line, "# TYPE ") {
				continue
			}
			name = strings.Fields(line)[2]
		} else {
			// sample line: "metricname{labels} value" or "metricname value"
			name = line
			if i := strings.IndexAny(name, "{ "); i >= 0 {
				name = name[:i]
			}
		}
		if !strings.HasPrefix(name, "kubeagent_slo_") {
			continue
		}
		got[name] = true
		if !isComment {
			samples++
		}
	}

	for name := range got {
		if !want[name] {
			t.Errorf("render() emitted unexpected SLO metric %q; either a new series was added without updating this test, or a rename left a stale name behind", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("render() did not emit expected SLO metric %q", name)
		}
	}

	// target_ratio(1) + availability(2) + burn_rate(2) + window_coverage(2) +
	// error_budget_remaining(1): the three windowed metrics emit one sample per
	// window, the two scalars emit one sample total.
	const wantSamples = 8
	if samples != wantSamples {
		t.Errorf("got %d kubeagent_slo_* samples, want %d (excludes # HELP / # TYPE lines)", samples, wantSamples)
	}
}
