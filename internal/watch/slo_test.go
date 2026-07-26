package watch

import (
	"errors"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/slo"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// sloBase is a fixed instant for the SLO tests. Deliberately not named `base`:
// metrics_test.go already uses that name as a function-local, and a
// package-level `base` would be silently shadowed there.
var sloBase = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestRender_OmitsSLOSeriesWhenDisabled(t *testing.T) {
	m := newMetrics([]string{"local"})
	out := m.render()
	if strings.Contains(out, "kubeagent_slo_") {
		t.Error("SLO series rendered while SLO tracking is off; --slo-target unset must mean no series")
	}
}

func TestRender_SLOSeries(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.updateSLO("local", true, 0.999,
		slo.Report{Window: slo.Fast, Availability: 0.99, BurnRate: 10, Coverage: 1},
		slo.Report{Window: slo.Slow, Availability: 0.995, BurnRate: 5, Coverage: 0.75},
	)
	out := m.render()
	for _, want := range []string{
		`kubeagent_slo_target_ratio{cluster="local"} 0.999`,
		`kubeagent_slo_availability_ratio{cluster="local",window="fast"} 0.99`,
		`kubeagent_slo_availability_ratio{cluster="local",window="slow"} 0.995`,
		`kubeagent_slo_burn_rate{cluster="local",window="fast"} 10`,
		`kubeagent_slo_burn_rate{cluster="local",window="slow"} 5`,
		`kubeagent_slo_window_coverage_ratio{cluster="local",window="fast"} 1`,
		`kubeagent_slo_window_coverage_ratio{cluster="local",window="slow"} 0.75`,
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

// TestRender_SLOAvailabilityHelpTextMatchesThePredicate pins the HELP text on
// kubeagent_slo_availability_ratio to the real predicate. This series' HELP
// string used to describe the pre-fix "no findings" numerator (the exact
// defect this branch corrected in Prioritize) and nothing caught it drifting
// out of sync with the code — it is the most operator-visible artifact of the
// whole feature, baked into every /metrics scrape.
func TestRender_SLOAvailabilityHelpTextMatchesThePredicate(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.updateSLO("local", true, 0.999,
		slo.Report{Window: slo.Fast, Availability: 0.99, BurnRate: 10, Coverage: 1},
		slo.Report{Window: slo.Slow, Availability: 0.995, BurnRate: 5, Coverage: 0.75},
	)
	out := m.render()
	want := "# HELP kubeagent_slo_availability_ratio Time-weighted fraction of workload-seconds that are not flagged, over the window\n"
	if !strings.Contains(out, want) {
		t.Errorf("missing HELP line %q in:\n%s", want, out)
	}
}

func TestRender_ErrorBudgetRemaining(t *testing.T) {
	cases := []struct {
		name     string
		slowBurn float64
		want     string
	}{
		{"quarter spent", 0.25, `kubeagent_slo_error_budget_remaining_ratio{cluster="local"} 0.75`},
		{"exactly spent", 1, `kubeagent_slo_error_budget_remaining_ratio{cluster="local"} 0`},
		{"overspent clamps at zero", 12, `kubeagent_slo_error_budget_remaining_ratio{cluster="local"} 0`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMetrics([]string{"local"})
			m.updateSLO("local", true, 0.999,
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
	m := newMetrics([]string{"local"})
	m.updateSLO("local", true, 0.999,
		slo.Report{Window: slo.Fast, BurnRate: 50, Coverage: 1},
		slo.Report{Window: slo.Slow, BurnRate: 50, Coverage: 1},
	)
	out := m.render()
	// Anchored for the same reason as the other assertions in this file:
	// "kubeagent_issues_active{cluster="local"} 0" is a literal prefix of
	// "kubeagent_issues_active{cluster="local"} 0.5", so an unanchored Contains
	// would not actually catch the gauge moving off exactly zero.
	if !strings.Contains(out, `kubeagent_issues_active{cluster="local"} 0`+"\n") {
		t.Error("kubeagent_issues_active moved off zero because of an SLO update")
	}
}

// TestRender_SLOMetricSet pins down the exact set of kubeagent_slo_* series
// render() emits, and the exact number of samples among them. Without this,
// nothing would notice a sixth kubeagent_slo_* series being added, or one of
// the five being renamed while a duplicate under the old name lingers behind.
func TestRender_SLOMetricSet(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.updateSLO("local", true, 0.999,
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
	m := newMetrics([]string{"local"})
	m.updateSLO("local", true, 0.999,
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
	n := newSLONotifier(defaultClusterName, time.Hour)
	if _, ok := n.step(slo.Verdict{}, sloBase); ok {
		t.Error("emitted a notification while the verdict was not firing")
	}
}

func TestSLONotifier_EmitsOnTheFiringEdge(t *testing.T) {
	n := newSLONotifier(defaultClusterName, time.Hour)
	got, ok := n.step(firing(sloBase), sloBase)
	if !ok {
		t.Fatal("no notification on the firing edge")
	}
	want := alertstate.Notification{
		Object:      alertstate.Object{Cluster: defaultClusterName, Kind: "SLO", Name: "error-budget"},
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
	n := newSLONotifier(defaultClusterName, time.Hour)
	n.step(firing(sloBase), sloBase)
	if _, ok := n.step(firing(sloBase), sloBase.Add(59*time.Minute)); ok {
		t.Error("re-sent inside the repeat interval")
	}
}

func TestSLONotifier_RepeatsAfterTheInterval(t *testing.T) {
	n := newSLONotifier(defaultClusterName, time.Hour)
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
	n := newSLONotifier(defaultClusterName, time.Hour)
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
	n := newSLONotifier(defaultClusterName, time.Hour)
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
	n := newSLONotifier(defaultClusterName, time.Hour)
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
	n := newSLONotifier(defaultClusterName, 0)
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
	n := newSLONotifier(defaultClusterName, time.Hour)
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
	n := newSLONotifier(defaultClusterName, time.Hour)

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

// TestApplyResult_ErrorDoesNotSample pins the invariant that makes the SLI
// trustworthy: a failed evaluation is neither "all clear" nor "all broken", so
// it must not become a sample. Without this the first API blip would count as
// an outage of the entire estate.
//
// The baseline sample must be genuinely clean (its Census must read good=total,
// not good=0) and the error-path result must carry a broken workload (the
// realistic case: a real scan.Evaluate error can still return a partially
// populated, unhealthy inventory). Feeding &scan.Result{} on every call — an
// empty result censuses to (0, 0), which Tracker.Observe treats as "no data"
// and skips regardless of whether the error check ran — would make this test
// pass even against an implementation that samples on the error path, because
// there would be nothing for the mutant sample to corrupt.
//
// The timing shape is what makes the coverage assertion discriminate: the
// healthy baseline sits in the single bucket at sloBase..sloBase+1m. By the
// time the fast (1h) report is read at sloBase+62m, that bucket has slid
// entirely out of the window, so a correct implementation reports coverage
// exactly 0. A mutant that samples errors would instead fill 61 more minutes of
// (bad) data into the ring, driving coverage back up toward 1 and availability
// down from 1 — either symptom alone catches the mutant, but Coverage's exact
// value (0, not merely "less than before") is the tightest check.
func TestApplyResult_ErrorDoesNotSample(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.sloTr, w.sloN = sloTr, sloN

	// applyResult reads Inventory.Census, not Inventory.Workloads, to feed
	// Tracker.Observe; Workloads is set here only for realism (a partially
	// populated inventory), the Census field is what actually drives the sample.
	healthy := &scan.Result{Inventory: inventory.Result{
		Workloads: []inventory.Workload{{Name: "a"}},
		Census:    inventory.Census{Good: 1, Total: 1},
	}}

	// A healthy sample to establish the baseline, then a minute of healthy time.
	captureLog(t, func() {
		w.applyResult(healthy, time.Millisecond, sloBase, nil)
	})
	captureLog(t, func() {
		w.applyResult(healthy, time.Millisecond, sloBase.Add(time.Minute), nil)
	})
	before := sloTr.Report(slo.Fast, sloBase.Add(time.Minute))

	// An hour of nothing but errors, each carrying a broken workload in the
	// result — the dangerous case a mutant could turn into an "all broken"
	// sample.
	for i := 2; i <= 62; i++ {
		captureLog(t, func() {
			w.applyResult(sampleResult(), time.Millisecond,
				sloBase.Add(time.Duration(i)*time.Minute), errors.New("boom"))
		})
	}
	after := sloTr.Report(slo.Fast, sloBase.Add(62*time.Minute))

	if after.Availability != 1 {
		t.Errorf("availability = %v after an hour of errors, want 1: errors must not be sampled", after.Availability)
	}
	if after.Coverage != 0 {
		t.Errorf("coverage = %v, want exactly 0: the healthy baseline must have slid out of the 1h window with nothing behind it", after.Coverage)
	}
	if after.Coverage >= before.Coverage {
		t.Errorf("coverage %v did not drop below %v; an error gap must reduce coverage, not be invisible",
			after.Coverage, before.Coverage)
	}
}

// TestApplyResult_SLODisabledIsInert proves the nil path: with SLOTarget unset
// the reconcile loop must not panic and must render no SLO series.
func TestApplyResult_SLODisabledIsInert(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{Heartbeat: time.Minute})
	if sloTr != nil || sloN != nil {
		t.Fatal("newSLOTracker returned a tracker with --slo-target unset")
	}
	w := testWorker(m, tr)
	captureLog(t, func() {
		w.applyResult(sampleResult(), time.Millisecond, sloBase, nil)
	})
	if strings.Contains(m.render(), "kubeagent_slo_") {
		t.Error("SLO series rendered with SLO tracking off")
	}
}

func TestValidateSLOTarget(t *testing.T) {
	cases := []struct {
		target  float64
		wantErr bool
	}{
		{0, false},           // disabled
		{0.999, false},       // typical
		{0.5, false},         // permissive but legal
		{1, true},            // zero error budget: burn rate divides by zero
		{1.5, true},          // nonsense
		{-0.1, true},         // nonsense
		{math.NaN(), true},   // every comparison against NaN is false; needs its own check
		{math.Inf(1), true},  // already caught by target >= 1
		{math.Inf(-1), true}, // already caught by target <= 0
	}
	for _, c := range cases {
		err := validateSLOTarget(c.target)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSLOTarget(%v) error = %v, wantErr = %v", c.target, err, c.wantErr)
		}
	}
}

func TestValidateSLOTarget_NaNMessage(t *testing.T) {
	// A NaN target is not caught by any of validateSLOTarget's range checks
	// (target == 0, target <= 0, target >= 1 are all false for NaN), so it
	// needs its own guard. This pins the exact error text: %g renders NaN as
	// "NaN", so the message reads "NaN%", not some other stringification.
	err := validateSLOTarget(math.NaN())
	if err == nil {
		t.Fatal("expected an error for a NaN --slo-target")
	}
	want := "invalid --slo-target: NaN% (must be greater than 0 and less than 100)"
	if err.Error() != want {
		t.Fatalf("error = %q, want exactly %q", err.Error(), want)
	}
}

// TestApplyResult_ConcurrentWithRender makes updateSLO's mutex load-bearing.
// Before this task wired a real caller into the reconcile loop, nothing drove
// updateSLO concurrently with render, so deleting its Lock/Unlock passed every
// test — including under -race, which can only report a race it actually
// observes. This test drives applyResult (with SLO tracking enabled, so
// updateSLO fires on every iteration) from one goroutine while concurrently
// calling render from another, giving -race a genuine, repeated read/write
// overlap on m.slo to catch.
func TestApplyResult_ConcurrentWithRender(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.sloTr, w.sloN = sloTr, sloN

	const n = 500
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			now := sloBase.Add(time.Duration(i) * time.Minute)
			w.applyResult(sampleResult(), time.Millisecond, now, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			_ = m.render()
		}
	}()
	wg.Wait()
}

// TestApplyResult_MismatchedSLOPairDoesNotPanic pins down the applyResult
// nil-guard decision directly: newSLOTracker only ever returns (nil, nil) or
// (non-nil, non-nil), but applyResult's signature takes sloTr and sloN as two
// independent parameters, so nothing stops a caller from passing a mismatched
// pair. Guarding on "sloTr != nil" alone would call sloN.step on a nil
// *sloNotifier whenever sloTr is set but sloN is not, and step dereferences
// n.firing on entry — a nil-pointer panic, not a graceful no-op. Checking both
// pointers, as applyResult does, keeps the mismatched-pair case inert instead
// of a crash.
func TestApplyResult_MismatchedSLOPairDoesNotPanic(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, _ := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.sloTr = sloTr

	captureLog(t, func() {
		w.applyResult(sampleResult(), time.Millisecond, sloBase, nil)
	})
}

// TestApplyResult_FastAndSlowWindowsDoNotSwap pins the argument order in
// applyResult's m.updateSLO(true, sloTr.Target(), v.Fast, v.Slow) call. Every
// other SLO render test drives updateSLO directly, so none of them would
// notice v.Fast and v.Slow being swapped at that one call site; this test has
// to go through applyResult itself, and needs a history where the fast (1h)
// and slow (6h) windows genuinely disagree so a swap is visible rather than
// coincidentally correct.
//
// Five hours of healthy samples followed by one broken hour makes the fast
// window (which only sees the last hour) read Availability 0, while the slow
// window (which still has five clean hours behind the one broken hour) reads
// something between 0 and 1. Comparing m.slo against sloTr.Report(...) read
// independently at the same instant, rather than against hand-picked
// constants, ties the assertion to whatever the tracker actually computed.
func TestApplyResult_FastAndSlowWindowsDoNotSwap(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.sloTr, w.sloN = sloTr, sloN

	healthy := &scan.Result{Inventory: inventory.Result{
		Workloads: []inventory.Workload{{Name: "a"}},
		Census:    inventory.Census{Good: 1, Total: 1},
	}}
	broken := sampleResult()

	now := sloBase
	for i := 0; i < 300; i++ { // 5 healthy hours
		captureLog(t, func() {
			w.applyResult(healthy, time.Millisecond, now, nil)
		})
		now = now.Add(time.Minute)
	}
	for i := 0; i < 60; i++ { // 1 broken hour
		captureLog(t, func() {
			w.applyResult(broken, time.Millisecond, now, nil)
		})
		now = now.Add(time.Minute)
	}

	wantFast := sloTr.Report(slo.Fast, now)
	wantSlow := sloTr.Report(slo.Slow, now)
	if wantFast.Availability == wantSlow.Availability {
		t.Fatal("test setup did not separate fast and slow availability; fixture needs adjusting")
	}

	got := m.clusters[defaultClusterName].slo
	if got.Fast.Availability != wantFast.Availability {
		t.Errorf("m.slo.Fast.Availability = %v, want %v (sloTr's fast report): applyResult must pass v.Fast into the fast slot",
			got.Fast.Availability, wantFast.Availability)
	}
	if got.Slow.Availability != wantSlow.Availability {
		t.Errorf("m.slo.Slow.Availability = %v, want %v (sloTr's slow report): applyResult must pass v.Slow into the slow slot",
			got.Slow.Availability, wantSlow.Availability)
	}
}

// TestApplyResult_RendersSLOSeriesThroughTheRealPath drives applyResult
// itself with SLO tracking configured on, rather than calling m.updateSLO
// directly the way every TestRender_SLO* test above does. Those tests
// hardcode the enabled argument as a literal true or false and never go
// through applyResult's own m.updateSLO(true, sloTr.Target(), v.Fast, v.Slow)
// call site, so none of them would notice that literal flipped to false:
// applyResult would keep computing the verdict and calling updateSLO exactly
// as before, but every kubeagent_slo_* series would silently stop rendering
// in production while every existing test in this file stayed green.
//
// One reconcile is enough: sloTr.Target() is read straight from the tracker's
// config, independent of any sample having landed yet, so
// kubeagent_slo_target_ratio renders (or does not) purely on the strength of
// the enabled flag applyResult passes through.
func TestApplyResult_RendersSLOSeriesThroughTheRealPath(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.sloTr, w.sloN = sloTr, sloN

	healthy := &scan.Result{Inventory: inventory.Result{
		Workloads: []inventory.Workload{{Name: "a"}},
		Census:    inventory.Census{Good: 1, Total: 1},
	}}
	captureLog(t, func() {
		w.applyResult(healthy, time.Millisecond, sloBase, nil)
	})

	out := m.render()
	// Anchored on the line terminator for the same reason as
	// TestRender_SLOSeries: "0.999" ending the line is exactly the configured
	// target, but an unanchored Contains would also pass against an unrelated
	// rendered value that merely starts with it.
	want := `kubeagent_slo_target_ratio{cluster="local"} 0.999` + "\n"
	if !strings.Contains(out, want) {
		t.Errorf("applyResult with SLO tracking enabled did not render %q through the real m.updateSLO call site; got:\n%s", want, out)
	}
}
