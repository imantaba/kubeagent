package watch

import (
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
type sloNotifier struct {
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
func newSLONotifier(repeat time.Duration) *sloNotifier {
	if repeat <= 0 {
		repeat = defaultSLORepeat
	}
	return &sloNotifier{repeat: repeat}
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
		Object:      alertstate.Object{Kind: sloAlertKind, Name: sloAlertName},
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
