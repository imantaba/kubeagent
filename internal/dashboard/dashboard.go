// Package dashboard renders the watch daemon's tracked state as one
// self-contained HTML page: the URL you hand someone who asks what is broken
// right now.
//
// The document is deliberately inert. It carries no <script>, no external
// stylesheet, font, or image, so it survives a strict Content-Security-Policy
// and performs no third-party fetch. The only dynamic behaviour is a
// <meta http-equiv="refresh"> carrying an interval and no URL — so the page
// emits no URL at all.
//
// The package imports nothing from kubeagent. That is a security property, not
// a style choice: it makes reaching internal/remediate or internal/explain
// structurally impossible rather than a rule someone has to remember, and
// imports_test.go enforces it. It is also why the view types below are defined
// here rather than reused from internal/watch, which is the caller.
//
// Render performs no cluster call and makes no model call. Those are two
// separate promises: the daemon's --explain feature does call a model, from
// the incident pipeline, never from an HTTP handler.
package dashboard

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"sort"
	"strings"
	"time"
)

// RefreshSeconds is how often the page reloads itself. It is a constant with no
// flag behind it: a flag would be a stable surface forever, and the value buys
// nothing tunable — informers detect in roughly two seconds and the heartbeat
// is sixty, so thirty already sits between them.
const RefreshSeconds = 30

//go:embed dashboard.html.tmpl
var templateSource string

// tmpl is parsed once at package init so a malformed template fails in CI, not
// in front of an operator mid-incident. This is the same choice
// internal/htmlreport makes.
//
// html/template, never text/template: the contextual auto-escaping is this
// package's single escape boundary. Issue text, cluster names and model output
// are all free-form strings that land verbatim in a page a browser renders.
var tmpl = template.Must(template.New("dashboard").Parse(templateSource))

// Input is everything the page renders. It is a plain value: the caller copies
// its state in, and the renderer holds no reference into anything a goroutine
// can still mutate.
type Input struct {
	// Version is the kubeagent version stamped into the header.
	Version string
	// Now is the generation time. Zero means wall-clock, the same contract
	// report.Input.Now carries: a caller that forgets the clock gets today,
	// not year 1.
	Now time.Time
	// Clusters is the cluster strip. It is always rendered, even when both
	// incident lists are empty — an empty list from an unreachable cluster and
	// an empty list from a healthy one are not the same thing, and this band is
	// what tells them apart.
	Clusters []Cluster
	// Active and Resolved are the tracked incidents. Render sorts them into a
	// total order; the caller's order is not preserved and does not matter.
	Active   []Incident
	Resolved []Incident
	// Stats is the aggregate the daemon already keeps.
	Stats Stats
	// SLO is one entry per cluster with SLO tracking on. Empty means the
	// section does not render — a daemon running without --slo-target should
	// not carry an empty table.
	SLO []SLO
	// ExplainEnabled reports whether --explain is on. It gates the section
	// independently of Explanations, because "on and nothing explained yet" is
	// a state an operator paying for the feature needs to be able to see, and a
	// vanishing section would look identical to the feature being off.
	ExplainEnabled bool
	// Explanations is the latest explanation per object, as the incident
	// pipeline computed it. Rendering one makes no model call.
	Explanations []Explanation
}

// Cluster is one watched cluster's reachability.
type Cluster struct {
	Name string
	Up   bool
	// LastScan is RFC 3339, or empty when no evaluation has completed. Empty is
	// the starting state, and it renders differently from "down".
	LastScan string
	// Error is the last read failure. It arrives already redacted — the caller
	// passes redact.Error's output — so this package escapes it and nothing
	// more. It must not become a second sanitization site.
	Error string
}

// Incident is one tracked issue instance. Active records carry AgeSeconds and
// leave the resolution fields zero; resolved records the reverse. Two fields
// rather than one pointer each because the two lists are already separate, and
// a nil pointer in a template is a defect waiting to happen.
type Incident struct {
	Cluster           string
	Kind              string
	Namespace         string
	Name              string
	Issue             string
	Firings           int
	Flapping          bool
	AgeSeconds        int64
	ResolvedAt        string
	ResolutionSeconds int64
}

// Stats is the aggregate counter set behind the summary tiles.
type Stats struct {
	NewTotal               int64
	ResolvedTotal          int64
	FlapTotal              int64
	DroppedTotal           int64
	ResolutionSecondsSum   float64
	ResolutionSecondsCount int64
}

// SLO is one cluster's error-budget state.
type SLO struct {
	Cluster string
	// Target is the availability target as a ratio in (0,1).
	Target  float64
	Windows []SLOWindow
}

// SLOWindow is one measurement window. Name is the caller's label — the
// renderer never interprets it — and matches the `window` label on the
// kubeagent_slo_* series so the page and a Grafana panel name the same thing.
type SLOWindow struct {
	Name         string
	Availability float64
	BurnRate     float64
	Coverage     float64
}

// Explanation is one object's latest explanation. Text is model output and gets
// exactly the same escaping as everything else: it is laid out with
// white-space: pre-wrap and never parsed as markdown, since parsing means
// unescaping.
type Explanation struct {
	Cluster     string
	Kind        string
	Namespace   string
	Name        string
	Issues      []string
	ExplainedAt string
	Model       string
	Text        string
}

// Render writes the complete HTML page to w. It performs no cluster call and
// makes no model call.
func Render(w io.Writer, in Input) error {
	return tmpl.Execute(w, newView(in))
}

// view is the flat shape the template ranges over. Every decision lives in
// newView so the template stays free of logic beyond ranging and conditionals.
type view struct {
	Version        string
	Generated      string
	RefreshSeconds int
	// MultiCluster drops the Cluster column from the incident tables when only
	// one cluster is watched, where it would repeat the same value on every row.
	MultiCluster     bool
	Clusters         []clusterRow
	Active           []incidentRow
	Resolved         []incidentRow
	Tiles            tiles
	SLO              []sloView
	ShowExplanations bool
	Explanations     []explanationRow
}

// clusterRow is one line of the cluster strip. State is a fixed keyword used as
// a CSS class, so the class and the visible label can never disagree.
type clusterRow struct {
	Name     string
	State    string // "up" | "down" | "pending"
	Label    string
	LastScan string
	Error    string
}

// incidentRow is one row of either incident table. Target is namespace/name, or
// name alone for a cluster-scoped object.
type incidentRow struct {
	Cluster    string
	Kind       string
	Target     string
	Issue      string
	Duration   string
	Firings    int
	Flapping   bool
	ResolvedAt string
	Resolution string
}

// tiles is the summary band. MTTR is a string because "nothing has resolved
// yet" is a legitimate state that no number expresses.
type tiles struct {
	New      int64
	Resolved int64
	Flapping int64
	Dropped  int64
	MTTR     string
}

// sloView is one cluster's SLO block.
type sloView struct {
	Cluster string
	Target  string
	Windows []sloWindowRow
}

// sloWindowRow is one window's line. Suppressed carries the same threshold the
// kubeagent_slo_window_coverage_ratio help text documents: below 0.6 the burn
// alert is suppressed, and a reader looking at a high burn rate needs to know
// that before acting on it.
type sloWindowRow struct {
	Name            string
	Availability    string
	BurnRate        string
	BudgetRemaining string
	Coverage        string
	Suppressed      bool
}

// explanationRow is one explanation as the page shows it.
type explanationRow struct {
	Cluster     string
	Kind        string
	Target      string
	Issues      string
	Model       string
	ExplainedAt string
	Text        string
}

// coverageFloor is the coverage below which the burn alert is suppressed. It
// matches internal/watch's metric help text; the two must not drift.
const coverageFloor = 0.6

// none is what every field prints when it has no value. One constant so the
// page never mixes spellings.
const none = "—"

// newView flattens Input into the template's model.
func newView(in Input) view {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	v := view{
		Version:        in.Version,
		Generated:      now.UTC().Format("2006-01-02 15:04:05 UTC"),
		RefreshSeconds: RefreshSeconds,
		MultiCluster:   len(in.Clusters) > 1,
		Tiles: tiles{
			New:      in.Stats.NewTotal,
			Resolved: in.Stats.ResolvedTotal,
			Flapping: in.Stats.FlapTotal,
			Dropped:  in.Stats.DroppedTotal,
			MTTR:     meanResolution(in.Stats.ResolutionSecondsSum, in.Stats.ResolutionSecondsCount),
		},
	}
	for _, c := range in.Clusters {
		state, label := clusterState(c)
		lastScan := c.LastScan
		if lastScan == "" {
			lastScan = none
		}
		errText := c.Error
		if errText == "" {
			errText = none
		}
		v.Clusters = append(v.Clusters, clusterRow{
			Name: c.Name, State: state, Label: label, LastScan: lastScan, Error: errText,
		})
	}

	active := append([]Incident(nil), in.Active...)
	sort.Slice(active, func(i, j int) bool {
		if active[i].AgeSeconds != active[j].AgeSeconds {
			return active[i].AgeSeconds > active[j].AgeSeconds // longest-firing first
		}
		return lessKey(active[i], active[j])
	})
	for _, r := range active {
		v.Active = append(v.Active, incidentRow{
			Cluster: r.Cluster, Kind: r.Kind, Target: target(r), Issue: r.Issue,
			Duration: humanDuration(r.AgeSeconds), Firings: r.Firings, Flapping: r.Flapping,
		})
	}

	resolved := append([]Incident(nil), in.Resolved...)
	sort.Slice(resolved, func(i, j int) bool {
		// ResolvedAt is RFC 3339 in UTC, fixed width and zero-padded, so a
		// lexicographic comparison is a chronological one.
		if resolved[i].ResolvedAt != resolved[j].ResolvedAt {
			return resolved[i].ResolvedAt > resolved[j].ResolvedAt // most recent first
		}
		return lessKey(resolved[i], resolved[j])
	})
	for _, r := range resolved {
		at := r.ResolvedAt
		if at == "" {
			at = none
		}
		v.Resolved = append(v.Resolved, incidentRow{
			Cluster: r.Cluster, Kind: r.Kind, Target: target(r), Issue: r.Issue,
			ResolvedAt: at, Resolution: humanDuration(r.ResolutionSeconds),
			Firings: r.Firings, Flapping: r.Flapping,
		})
	}

	for _, s := range in.SLO {
		sv := sloView{Cluster: s.Cluster, Target: percent(s.Target)}
		for _, w := range s.Windows {
			sv.Windows = append(sv.Windows, sloWindowRow{
				Name:            w.Name,
				Availability:    percent(w.Availability),
				BurnRate:        ratio(w.BurnRate),
				BudgetRemaining: budgetRemaining(w.BurnRate),
				Coverage:        percent(w.Coverage),
				Suppressed:      finite(w.Coverage) && w.Coverage < coverageFloor,
			})
		}
		v.SLO = append(v.SLO, sv)
	}

	v.ShowExplanations = in.ExplainEnabled
	for _, x := range in.Explanations {
		v.Explanations = append(v.Explanations, explanationRow{
			Cluster:     x.Cluster,
			Kind:        x.Kind,
			Target:      target(Incident{Namespace: x.Namespace, Name: x.Name}),
			Issues:      strings.Join(x.Issues, ", "),
			Model:       x.Model,
			ExplainedAt: x.ExplainedAt,
			Text:        x.Text,
		})
	}
	return v
}

// lessKey is the tiebreaker chain both tables share. Cluster, kind, namespace,
// name and issue are a tracked issue's identity — the daemon's key is
// kind/namespace/name/issue and a cluster name is unique within a daemon — so
// the chain is a total order, and two distinct rows can never compare equal.
// A partial order would let equal rows swap places between renders, which on a
// thirty-second reload is genuinely unusable.
func lessKey(a, b Incident) bool {
	if a.Cluster != b.Cluster {
		return a.Cluster < b.Cluster
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Issue < b.Issue
}

// target is how an object is named in a row: namespace/name, or name alone for
// a cluster-scoped object.
func target(r Incident) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// clusterState classifies a cluster for the strip. The never-scanned case is
// first because a starting daemon reports up=false, and "starting up" is not
// the same statement as "unreachable".
func clusterState(c Cluster) (state, label string) {
	switch {
	case c.LastScan == "":
		return "pending", "starting up — not scanned yet"
	case c.Up:
		return "up", "up"
	default:
		return "down", "unreachable"
	}
}

// humanDuration spells a whole-second span the way an operator reads it. A
// negative span is impossible from the daemon (it floors at zero already) and
// is floored again here so a hostile Input cannot produce "-3m -20s".
func humanDuration(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", sec/86400, (sec%86400)/3600)
	}
}

// maxRenderableSeconds bounds what meanResolution will convert to an integer.
// A float64 outside int64's range converts with implementation-defined results
// in Go, which is exactly the class of defect the fuzz campaign behind Theme H
// slice 3 found in the DNS health parser. Anything past this is not a duration
// anyone will read, so it prints as no value at all.
const maxRenderableSeconds = 1e15

// meanResolution is the mean-time-to-resolution tile. Zero resolutions is a
// legitimate state, not a division to be papered over.
func meanResolution(sum float64, count int64) string {
	if count <= 0 || !finite(sum) {
		return none
	}
	avg := sum / float64(count)
	if avg < 0 || avg > maxRenderableSeconds {
		return none
	}
	return humanDuration(int64(avg))
}

// finite reports whether f is a real number: NaN fails f == f, and ±Inf fails
// f-f == 0, since ∞-∞ is NaN. math.IsNaN and math.IsInf say the same thing;
// these comparisons keep the package's import list to the standard-library
// packages the design named, which is the list imports_test.go asserts against.
func finite(f float64) bool { return f == f && f-f == 0 }

// percent renders a ratio as a percentage. A non-finite value prints as no
// value: a burn rate is a quotient, and a target of exactly 1 makes it
// infinite.
func percent(f float64) string {
	if !finite(f) {
		return none
	}
	return fmt.Sprintf("%.2f%%", f*100)
}

// ratio renders a plain multiple, such as a burn rate.
func ratio(f float64) string {
	if !finite(f) {
		return none
	}
	return fmt.Sprintf("%.2f", f)
}

// budgetRemaining is the fraction of the error budget left over the window,
// clamped to [0,1] — the same definition the
// kubeagent_slo_error_budget_remaining_ratio series carries. A burn above 1x
// means the budget is already spent.
func budgetRemaining(burn float64) string {
	if !finite(burn) {
		return none
	}
	left := 1 - burn
	if left < 0 {
		left = 0
	}
	if left > 1 {
		left = 1
	}
	return percent(left)
}
