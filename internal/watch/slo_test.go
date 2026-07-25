package watch

import (
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
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

// TestRender_SLOMetricHelpTypeCardinality pins the plan's other half of the
// SLO rendering constraint: one # HELP / # TYPE pair per metric name, not per
// sample. TestRender_SLOMetricSet above cannot see this — it collects names
// into a map, which dedups, so it cannot distinguish "one HELP line" from
// "the same HELP line repeated before every sample," and it would not notice
// a missing TYPE line as long as some other comment line still carried the
// name. This test counts HELP and TYPE lines per metric by exact line-field
// comparison instead of substring matching, so a metric name that happens to
// prefix another (or be prefixed by one) cannot make the count creep.
func TestRender_SLOMetricHelpTypeCardinality(t *testing.T) {
	m := newMetrics()
	m.updateSLO(true, 0.999,
		slo.Report{Window: slo.Fast, Availability: 0.99, BurnRate: 10, Coverage: 1},
		slo.Report{Window: slo.Slow, Availability: 0.995, BurnRate: 5, Coverage: 0.75},
	)
	out := m.render()

	names := []string{
		"kubeagent_slo_target_ratio",
		"kubeagent_slo_availability_ratio",
		"kubeagent_slo_burn_rate",
		"kubeagent_slo_error_budget_remaining_ratio",
		"kubeagent_slo_window_coverage_ratio",
	}

	helpCount := map[string]int{}
	typeCount := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		// "# HELP <name> <help text...>" / "# TYPE <name> <type>": split on
		// whitespace and compare the name field exactly, not with HasPrefix or
		// Contains, so no metric name can be miscounted just because it is a
		// prefix (or superstring) of another.
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "#" {
			continue
		}
		switch fields[1] {
		case "HELP":
			helpCount[fields[2]]++
		case "TYPE":
			typeCount[fields[2]]++
		}
	}

	for _, name := range names {
		if got := helpCount[name]; got != 1 {
			t.Errorf("render() emitted %d # HELP line(s) for metric %q, want exactly 1", got, name)
		}
		if got := typeCount[name]; got != 1 {
			t.Errorf("render() emitted %d # TYPE line(s) for metric %q, want exactly 1", got, name)
		}
	}
}

func firing(since time.Time) slo.Verdict {
	return slo.Verdict{Firing: true, FiringSince: since}
}

func TestSLONotifier_SilentWhileNotFiring(t *testing.T) {
	n := newSLONotifier(time.Hour)
	if _, ok := n.step(slo.Verdict{}, sloBase); ok {
		t.Error("emitted a notification while the verdict was not firing")
	}
}

func TestSLONotifier_EmitsOnTheFiringEdge(t *testing.T) {
	n := newSLONotifier(time.Hour)
	got, ok := n.step(firing(sloBase), sloBase)
	if !ok {
		t.Fatal("no notification on the firing edge")
	}
	want := alertstate.Notification{
		Object:      alertstate.Object{Kind: "SLO", Name: "error-budget"},
		Status:      alertstate.StatusFiring,
		Issues:      []string{"ErrorBudgetBurn"},
		FiringSince: sloBase,
		Reason:      alertstate.ReasonNew,
	}
	if got.Object != want.Object || got.Status != want.Status || got.Reason != want.Reason {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
	if len(got.Issues) != 1 || got.Issues[0] != "ErrorBudgetBurn" {
		t.Errorf("Issues = %v, want [ErrorBudgetBurn]", got.Issues)
	}
	if !got.FiringSince.Equal(sloBase) {
		t.Errorf("FiringSince = %v, want %v", got.FiringSince, sloBase)
	}
	if got.Object.Namespace != "" {
		t.Errorf("Namespace = %q, want empty (the budget is cluster-scoped)", got.Object.Namespace)
	}
}

func TestSLONotifier_SilentWhileStillFiringInsideRepeat(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	if _, ok := n.step(firing(sloBase), sloBase.Add(59*time.Minute)); ok {
		t.Error("re-sent inside the repeat interval")
	}
}

func TestSLONotifier_RepeatsAfterTheInterval(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	got, ok := n.step(firing(sloBase), sloBase.Add(time.Hour))
	if !ok {
		t.Fatal("no re-send after the repeat interval elapsed")
	}
	if got.Reason != alertstate.ReasonRepeat {
		t.Errorf("Reason = %q, want %q", got.Reason, alertstate.ReasonRepeat)
	}
	if !got.FiringSince.Equal(sloBase) {
		t.Errorf("FiringSince = %v, want the original %v", got.FiringSince, sloBase)
	}
}

func TestSLONotifier_EmitsResolvedOnce(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	clear := sloBase.Add(2 * time.Hour)
	got, ok := n.step(slo.Verdict{}, clear)
	if !ok {
		t.Fatal("no resolved notification when the breach cleared")
	}
	if got.Status != alertstate.StatusResolved || got.Reason != alertstate.ReasonResolved {
		t.Errorf("status/reason = %q/%q, want resolved/resolved", got.Status, got.Reason)
	}
	if len(got.Issues) != 0 {
		t.Errorf("Issues = %v, want empty on resolve", got.Issues)
	}
	if !got.ResolvedAt.Equal(clear) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, clear)
	}
	if _, ok := n.step(slo.Verdict{}, clear.Add(time.Minute)); ok {
		t.Error("emitted a second resolved notification; resolve must fire once")
	}
}

func TestSLONotifier_ReFiresAfterResolving(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	n.step(slo.Verdict{}, sloBase.Add(time.Hour))
	second := sloBase.Add(2 * time.Hour)
	got, ok := n.step(firing(second), second)
	if !ok {
		t.Fatal("no notification on the second firing edge")
	}
	if got.Reason != alertstate.ReasonNew {
		t.Errorf("Reason = %q, want %q on a fresh breach", got.Reason, alertstate.ReasonNew)
	}
	if !got.FiringSince.Equal(second) {
		t.Errorf("FiringSince = %v, want the new breach start %v", got.FiringSince, second)
	}
}

// TestSLONotifier_FiringSinceComesFromTheVerdictNotNow guards against
// n.since being set from the observation instant rather than the verdict's
// own FiringSince. Every other test in this file happens to call step with
// now equal to the verdict's FiringSince (both come from the same `firing(t)`
// call passed as `firing(t), t`), so none of them can tell "since = v.FiringSince"
// apart from "since = now". This test uses a verdict whose breach began before
// this notifier's first observation of it — the daemon restarting partway
// through an already-firing window is exactly when that happens — so the two
// timestamps differ and the notification must carry the breach's own start.
func TestSLONotifier_FiringSinceComesFromTheVerdictNotNow(t *testing.T) {
	n := newSLONotifier(time.Hour)
	began := sloBase.Add(-30 * time.Minute)
	observedAt := sloBase
	got, ok := n.step(firing(began), observedAt)
	if !ok {
		t.Fatal("no notification on the firing edge")
	}
	if !got.FiringSince.Equal(began) {
		t.Errorf("FiringSince = %v, want the verdict's breach start %v (not the observation time %v)", got.FiringSince, began, observedAt)
	}
}

// TestSLONotifier_ZeroRepeatUsesTheDefault pins the required normalization: a
// zero repeat (what --alert-repeat defaults to before main.go resolves it to
// the format default) must not make the notifier re-send on every reconcile.
// Without normalization, now.Sub(lastSent) >= 0 is true on every call after the
// firing edge, which floods the alert sink with a webhook per cycle for as long
// as the breach persists — exactly what alertstate.New's own `Repeat <= 0`
// guard exists to prevent for object alerts.
func TestSLONotifier_ZeroRepeatUsesTheDefault(t *testing.T) {
	n := newSLONotifier(0)
	n.step(firing(sloBase), sloBase)
	if _, ok := n.step(firing(sloBase), sloBase.Add(time.Second)); ok {
		t.Error("re-sent one second after the firing edge with a zero repeat; want the default interval honored")
	}
	got, ok := n.step(firing(sloBase), sloBase.Add(defaultSLORepeat))
	if !ok {
		t.Fatal("no re-send after the default repeat interval elapsed")
	}
	if got.Reason != alertstate.ReasonRepeat {
		t.Errorf("Reason = %q, want %q", got.Reason, alertstate.ReasonRepeat)
	}
}

// TestSLONotifier_RepeatClockRestartsAfterARepeat proves the repeat clock
// restarts from the last SEND, not the original firing edge: a re-send at one
// interval must not be immediately followed by another. TestSLONotifier_
// RepeatsAfterTheInterval only checks the first re-send; this checks the one
// after it, which would fire immediately if lastSent were never updated on the
// repeat path.
func TestSLONotifier_RepeatClockRestartsAfterARepeat(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	n.step(firing(sloBase), sloBase.Add(time.Hour)) // first re-send

	if _, ok := n.step(firing(sloBase), sloBase.Add(time.Hour+59*time.Minute)); ok {
		t.Error("re-sent inside the repeat interval measured from the previous re-send")
	}

	got, ok := n.step(firing(sloBase), sloBase.Add(2*time.Hour))
	if !ok {
		t.Fatal("no re-send a full interval after the previous re-send")
	}
	if got.Reason != alertstate.ReasonRepeat {
		t.Errorf("Reason = %q, want %q", got.Reason, alertstate.ReasonRepeat)
	}
}

// TestSLONotifier_ResolvedAtZeroWhileFiring pins the other half of the
// convention alertstate.Notification documents: ResolvedAt is zero unless
// resolved. The brief's tests only assert ResolvedAt on the resolved
// notification; this asserts it stays zero on both the firing-edge and the
// repeat notifications, so a future change that starts stamping `now` into
// ResolvedAt on a still-firing send would be caught.
func TestSLONotifier_ResolvedAtZeroWhileFiring(t *testing.T) {
	n := newSLONotifier(time.Hour)

	got, ok := n.step(firing(sloBase), sloBase)
	if !ok {
		t.Fatal("no notification on the firing edge")
	}
	if !got.ResolvedAt.IsZero() {
		t.Errorf("ResolvedAt = %v, want the zero time on the firing edge", got.ResolvedAt)
	}

	got, ok = n.step(firing(sloBase), sloBase.Add(time.Hour))
	if !ok {
		t.Fatal("no re-send after the repeat interval elapsed")
	}
	if !got.ResolvedAt.IsZero() {
		t.Errorf("ResolvedAt = %v, want the zero time on a repeat notification", got.ResolvedAt)
	}
}
