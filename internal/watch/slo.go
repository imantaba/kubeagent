package watch

import (
	"fmt"
	"math"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/slo"
)

// The burn alert's fixed identity. It is deliberately NOT an object in the
// cluster: a budget breach is a property of the estate over time, not of a
// Deployment. It never enters watchstate or alertstate tracking, so it does not
// appear in /issues or inflate any kubeagent_issues_* series — an operator
// reading those as object counts would be misled if it did.
const (
	sloAlertKind  = "SLO"
	sloAlertName  = "error-budget"
	sloAlertIssue = "ErrorBudgetBurn" // not "FastBurn": firing needs BOTH windows
)

// defaultSLORepeat mirrors alertstate's unexported defaultRepeat (4h). It is
// not reused directly because alertstate does not export it; keeping the same
// value here means newSLONotifier degrades the same way alertstate.New does
// when given a zero repeat.
const defaultSLORepeat = 4 * time.Hour

// sloNotifier turns a stream of verdicts into the edge-triggered notifications
// the alert sink expects: one on the firing edge, a periodic re-send while the
// breach persists, and exactly one on the clearing edge.
//
// This is deliberately a separate, much smaller state machine than
// alertstate.Roller. The roller's job is rolling many per-issue records up to
// per-object alerts; there is exactly one error budget, so all that remains is
// the firing edge and the repeat clock.
//
// An sloNotifier is not safe for concurrent use; the daemon touches it only from
// its reconcile loop, exactly as it does watchstate.Tracker and
// alertstate.Roller.
type sloNotifier struct {
	cluster  string
	repeat   time.Duration
	firing   bool
	since    time.Time
	lastSent time.Time
}

// newSLONotifier returns a notifier re-sending a still-firing breach every
// repeat. It shares --alert-repeat with object alerts so an Alertmanager
// receiver refreshes the budget alert before resolve_timeout expires it, exactly
// as it does for object alerts.
//
// A non-positive repeat takes defaultSLORepeat, the same normalization
// alertstate.New applies to Options.Repeat. --alert-repeat defaults to 0
// ("use the format default") and main.go resolves that before building the
// daemon's config, but a zero repeat reaching step unguarded would make
// now.Sub(n.lastSent) >= n.repeat true on every call — re-sending a webhook
// notification on every reconcile cycle for as long as the breach persists.
func newSLONotifier(cluster string, repeat time.Duration) *sloNotifier {
	if repeat <= 0 {
		repeat = defaultSLORepeat
	}
	return &sloNotifier{cluster: cluster, repeat: repeat}
}

// step folds one verdict in and reports the notification to send, if any.
func (n *sloNotifier) step(v slo.Verdict, now time.Time) (alertstate.Notification, bool) {
	switch {
	case v.Firing && !n.firing:
		n.firing, n.since, n.lastSent = true, v.FiringSince, now
		return n.notification(alertstate.StatusFiring, alertstate.ReasonNew, time.Time{}), true

	case v.Firing && now.Sub(n.lastSent) >= n.repeat:
		n.lastSent = now
		return n.notification(alertstate.StatusFiring, alertstate.ReasonRepeat, time.Time{}), true

	case !v.Firing && n.firing:
		n.firing = false
		return n.notification(alertstate.StatusResolved, alertstate.ReasonResolved, now), true
	}
	return alertstate.Notification{}, false
}

// notification builds the payload. Issues is empty on resolve, matching the
// convention alertstate.Notification documents and the encoders rely on.
func (n *sloNotifier) notification(s alertstate.Status, r alertstate.Reason, resolvedAt time.Time) alertstate.Notification {
	out := alertstate.Notification{
		Object:      alertstate.Object{Cluster: n.cluster, Kind: sloAlertKind, Name: sloAlertName},
		Status:      s,
		FiringSince: n.since,
		ResolvedAt:  resolvedAt,
		Reason:      r,
	}
	if s == alertstate.StatusFiring {
		out.Issues = []string{sloAlertIssue}
	}
	return out
}

// validateSLOTarget rejects a target that cannot produce a burn rate. 1.0 is
// rejected explicitly: a 100% target makes the error budget zero, and the burn
// rate would divide by it. NaN needs its own check for the same reason: every
// comparison against NaN is false, so target == 0, target <= 0, and
// target >= 1 all fail to catch it, and it would otherwise fall through as a
// silently accepted, enabled target.
func validateSLOTarget(target float64) error {
	if target == 0 {
		return nil // disabled
	}
	if math.IsNaN(target) || target <= 0 || target >= 1 {
		return fmt.Errorf("invalid --slo-target: %g%% (must be greater than 0 and less than 100)", target*100)
	}
	return nil
}

// newSLOTracker returns the tracker and its notifier, or nils when SLO tracking
// is off. Like *alerter, the nil case is the switched-off state.
func newSLOTracker(cluster string, cfg Config) (*slo.Tracker, *sloNotifier) {
	if cfg.SLOTarget == 0 {
		return nil, nil
	}
	gap := 2 * cfg.Heartbeat
	tr := slo.New(slo.Options{Target: cfg.SLOTarget, MaxSampleGap: gap})
	return tr, newSLONotifier(cluster, cfg.AlertRepeat)
}

// logSLO prints the burn transition. It mirrors logDelta's NEW/RESOLVED shape so
// the two alert sources read the same way in the daemon's log.
func logSLO(cluster string, n alertstate.Notification, v slo.Verdict) {
	if n.Status == alertstate.StatusResolved {
		clusterLogf(cluster, "RESOLVED SLO/error-budget (burn back under threshold; fast=%.1fx slow=%.1fx)",
			v.Fast.BurnRate, v.Slow.BurnRate)
		return
	}
	clusterLogf(cluster, "%s SLO/error-budget:ErrorBudgetBurn (fast=%.1fx slow=%.1fx, coverage fast=%.0f%% slow=%.0f%%)",
		map[alertstate.Reason]string{alertstate.ReasonNew: "NEW", alertstate.ReasonRepeat: "REPEAT"}[n.Reason],
		v.Fast.BurnRate, v.Slow.BurnRate, v.Fast.Coverage*100, v.Slow.Coverage*100)
}
