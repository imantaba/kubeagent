// Package baseline learns what a workload's restart rate normally is on one
// cluster, and compares a later observation against it.
//
// The package is pure. It imports nothing from kubeagent and nothing outside
// the standard library, holds no client and no context, issues no cluster call
// and makes no model call — the last two are separate promises. The caller
// reduces pods to PodSample values, so no Kubernetes type crosses the
// boundary, which is what makes reaching internal/remediate or
// internal/explain impossible by construction rather than by rule.
// internal/baseline/imports_test.go enforces both halves of that.
//
// What the rate honestly measures: restarts over the lifetimes of the pods
// present when the sample was taken. It is not long-term history. A workload
// whose pods were all recreated an hour before capture shows only what those
// pods have done since. internal/capacity states the equivalent limit for
// metrics-server samples; this one is stated here, in the feature docs, and in
// the `baseline capture` help text.
package baseline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is the version Capture writes into every Document. It is
// spelled here rather than imported from internal/jsonschema because this
// package imports nothing from kubeagent;
// internal/schemadoc's TestBaselineSchemaVersionMatches pins the two together,
// so the duplication cannot drift silently.
const SchemaVersion = "1.0"

// The defaults. Exported because the CLI declares them as flag defaults and
// there must be exactly one spelling of each number.
const (
	// DefaultFactor is the multiplicative threshold: a workload must reach this
	// multiple of its baseline rate before it is a deviation.
	DefaultFactor = 3.0
	// DefaultFloor is the absolute threshold in restarts per hour, which is what
	// stops 0.001 -> 0.01 reading as a 10x alarm.
	DefaultFloor = 0.5
	// DefaultMinPodAge is how old a pod must be to count toward a rate.
	DefaultMinPodAge = time.Hour
)

// maxVersionLen bounds the schemaVersion string an operator-supplied document
// may carry, so a malformed file cannot put an arbitrary blob into an error
// message.
const maxVersionLen = 32

// PodSample is one pod's contribution, already resolved to its workload.
type PodSample struct {
	Kind       string // "Deployment" | "StatefulSet" | "DaemonSet" | "Job" | "CronJob" | "Pod"
	Namespace  string
	Name       string  // the WORKLOAD's name, not the pod's
	Restarts   int     // sum of ContainerStatus.RestartCount across the pod's containers
	AgeSeconds float64 // now - pod start
}

// Entry is one workload's learned normal.
type Entry struct {
	Kind            string  `json:"kind"`
	Namespace       string  `json:"namespace"`
	Name            string  `json:"name"`
	RestartsPerHour float64 `json:"restartsPerHour"`
	Pods            int     `json:"pods"`            // pods that counted
	ObservedSeconds float64 `json:"observedSeconds"` // total pod-seconds behind the rate
}

// Document is the artifact `kubeagent baseline capture` prints and
// `--baseline` reads. It is a published, versioned JSON document.
type Document struct {
	SchemaVersion    string  `json:"schemaVersion"`
	CapturedAt       string  `json:"capturedAt"` // RFC3339 UTC
	MinPodAgeSeconds float64 `json:"minPodAgeSeconds"`
	Workloads        []Entry `json:"workloads"`
}

// Deviation is one workload whose current rate is abnormal for this cluster.
// Tagged because Report is embedded in report.ScanReport and therefore lands in
// `scan --output json` and in the published scan schema.
type Deviation struct {
	Kind         string  `json:"kind"`
	Namespace    string  `json:"namespace"`
	Name         string  `json:"name"`
	BaselineRate float64 `json:"baselineRestartsPerHour"`
	CurrentRate  float64 `json:"currentRestartsPerHour"`
	Pods         int     `json:"pods"` // pods behind CurrentRate
}

// Report is what Compare returns.
type Report struct {
	// Deviations is always non-nil: a run that found nothing encodes
	// "deviations": [], which says the comparison happened, where an absent key
	// would not.
	Deviations      []Deviation `json:"deviations"`
	Compared        int         `json:"compared"`        // present in both the document and the cluster
	NotInBaseline   int         `json:"notInBaseline"`   // in the cluster, absent from the document
	GoneFromCluster int         `json:"goneFromCluster"` // in the document, absent from the cluster
}

// CompareOptions tunes the deviation rule. A zero field takes its default, the
// same convention watchstate.Options uses.
type CompareOptions struct {
	Factor float64 // default DefaultFactor
	Floor  float64 // default DefaultFloor
}

// Capture reduces a cluster's pods to one entry per workload. now is passed in
// rather than read, matching RestartLoopDetector's injected instant and
// watchstate's "the caller passes now".
func Capture(pods []PodSample, minPodAge time.Duration, now time.Time) Document {
	minSeconds := minPodAge.Seconds()
	if minSeconds < 0 || math.IsNaN(minSeconds) {
		minSeconds = 0
	}
	return Document{
		SchemaVersion:    SchemaVersion,
		CapturedAt:       now.UTC().Format(time.RFC3339),
		MinPodAgeSeconds: minSeconds,
		Workloads:        rates(pods, minSeconds),
	}
}

// Compare judges pods against doc.
//
// The minimum pod age comes from doc.MinPodAgeSeconds, never from the caller.
// A capture and a compare run with different floors would silently produce
// garbage, so the symmetry is read out of the document rather than left to a
// caller's discipline.
func Compare(doc Document, pods []PodSample, opts CompareOptions) Report {
	if opts.Factor <= 0 || math.IsNaN(opts.Factor) {
		opts.Factor = DefaultFactor
	}
	if opts.Floor <= 0 || math.IsNaN(opts.Floor) {
		opts.Floor = DefaultFloor
	}

	base := make(map[string]Entry, len(doc.Workloads))
	for _, e := range doc.Workloads {
		base[key(e.Kind, e.Namespace, e.Name)] = e
	}

	rep := Report{Deviations: []Deviation{}}
	seen := make(map[string]bool, len(doc.Workloads))
	for _, e := range rates(pods, doc.MinPodAgeSeconds) {
		k := key(e.Kind, e.Namespace, e.Name)
		b, ok := base[k]
		if !ok {
			rep.NotInBaseline++
			continue
		}
		seen[k] = true
		rep.Compared++
		if !deviates(b.RestartsPerHour, e.RestartsPerHour, opts) {
			continue
		}
		rep.Deviations = append(rep.Deviations, Deviation{
			Kind: e.Kind, Namespace: e.Namespace, Name: e.Name,
			BaselineRate: b.RestartsPerHour, CurrentRate: e.RestartsPerHour,
			Pods: e.Pods,
		})
	}
	for _, e := range doc.Workloads {
		if !seen[key(e.Kind, e.Namespace, e.Name)] {
			rep.GoneFromCluster++
		}
	}
	sortDeviations(rep.Deviations)
	return rep
}

// deviates applies the two-threshold rule. BOTH must hold: the multiplicative
// test catches "this got much worse relative to itself", and the absolute floor
// is what stops 0.001 -> 0.01 reading as a 10x alarm. The floor also carries
// the baseline-is-zero case, where the multiplicative test is trivially true.
// Only increases deviate — a workload restarting less than its baseline is not
// reported, because nobody is paged for a thing improving.
func deviates(baseRate, currentRate float64, opts CompareOptions) bool {
	return currentRate >= baseRate*opts.Factor && currentRate-baseRate >= opts.Floor
}

// rates is the shared maths: one entry per workload that has at least one
// counted pod, sorted. Capture and Compare both go through it, so the two sides
// can never compute a rate two different ways.
func rates(pods []PodSample, minSeconds float64) []Entry {
	type acc struct {
		id       PodSample
		restarts int
		seconds  float64
		pods     int
	}
	totals := map[string]*acc{}
	for _, p := range pods {
		if !counts(p, minSeconds) {
			continue
		}
		k := key(p.Kind, p.Namespace, p.Name)
		a, ok := totals[k]
		if !ok {
			a = &acc{id: p}
			totals[k] = a
		}
		a.restarts += p.Restarts
		a.seconds += p.AgeSeconds
		a.pods++
	}

	out := make([]Entry, 0, len(totals))
	for _, a := range totals {
		out = append(out, Entry{
			Kind: a.id.Kind, Namespace: a.id.Namespace, Name: a.id.Name,
			RestartsPerHour: ratePerHour(a.restarts, a.seconds),
			Pods:            a.pods,
			ObservedSeconds: a.seconds,
		})
	}
	sortEntries(out)
	return out
}

// counts reports whether a sample contributes to a rate. A pod younger than the
// floor is excluded from BOTH the numerator and the denominator: a
// 30-second-old pod with 2 restarts implies 240 restarts/hour and would swamp
// every older pod in its workload. A non-finite or non-positive age is refused
// for the same reason — it cannot contribute meaningful pod-seconds, and a NaN
// would poison the whole workload's sum.
func counts(p PodSample, minSeconds float64) bool {
	if math.IsNaN(p.AgeSeconds) || math.IsInf(p.AgeSeconds, 0) || p.AgeSeconds <= 0 {
		return false
	}
	if p.Restarts < 0 {
		return false
	}
	return p.AgeSeconds >= minSeconds
}

// ratePerHour normalises a workload's restarts across its pods' observed
// seconds. counts already refuses a non-positive age, so zero seconds cannot
// reach here today; the guard stays because a division that could produce +Inf
// must not depend on a caller's invariant holding.
func ratePerHour(restarts int, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(restarts) / (seconds / 3600)
}

func key(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

func sortEntries(e []Entry) {
	sort.Slice(e, func(i, j int) bool {
		if e[i].Kind != e[j].Kind {
			return e[i].Kind < e[j].Kind
		}
		if e[i].Namespace != e[j].Namespace {
			return e[i].Namespace < e[j].Namespace
		}
		return e[i].Name < e[j].Name
	})
}

func sortDeviations(d []Deviation) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Kind != d[j].Kind {
			return d[i].Kind < d[j].Kind
		}
		if d[i].Namespace != d[j].Namespace {
			return d[i].Namespace < d[j].Namespace
		}
		return d[i].Name < d[j].Name
	})
}

// Marshal renders the document the way `kubeagent baseline capture` prints it:
// two-space indented with a trailing newline, matching every other JSON
// document kubeagent writes.
func (d Document) Marshal() ([]byte, error) {
	if d.Workloads == nil {
		d.Workloads = []Entry{}
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Load parses a captured document.
//
// It accepts any document whose schemaVersion has the same MAJOR as this build
// writes and rejects a different MAJOR by name. That matches the published
// schemas' own contract: additionalProperties is unset on purpose, so a
// document from a later MINOR must still load here, unknown keys and all.
//
// What Load returns is arithmetic-safe: no NaN, no Inf, no negative rate. JSON
// has no NaN or Inf literal and encoding/json refuses an out-of-range
// magnitude, so those cannot arrive through Decode today — the checks stay
// because that guarantee is Load's contract and must not depend on a property
// of the decoder.
func Load(b []byte) (Document, error) {
	var d Document
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&d); err != nil {
		return Document{}, fmt.Errorf("parsing baseline: %w", err)
	}
	if d.SchemaVersion == "" {
		return Document{}, errors.New("baseline has no schemaVersion")
	}
	got, err := majorOf(d.SchemaVersion)
	if err != nil {
		return Document{}, err
	}
	want, err := majorOf(SchemaVersion)
	if err != nil {
		return Document{}, err
	}
	if got != want {
		return Document{}, fmt.Errorf(
			"baseline schemaVersion %s is major version %s; this build reads major version %s — recapture it with `kubeagent baseline capture`",
			d.SchemaVersion, got, want)
	}
	if !usableNumber(d.MinPodAgeSeconds) || d.MinPodAgeSeconds < 0 {
		return Document{}, errors.New("baseline minPodAgeSeconds is not a usable number")
	}
	for i, e := range d.Workloads {
		// The index, not the name: an error message is not the place to reprint
		// cluster-shaped data from an operator-supplied file.
		if e.Kind == "" || e.Name == "" {
			return Document{}, fmt.Errorf("baseline workload %d has no kind or no name", i)
		}
		if !usableNumber(e.RestartsPerHour) || e.RestartsPerHour < 0 {
			return Document{}, fmt.Errorf("baseline workload %d has an unusable restartsPerHour", i)
		}
		if !usableNumber(e.ObservedSeconds) || e.ObservedSeconds < 0 {
			return Document{}, fmt.Errorf("baseline workload %d has an unusable observedSeconds", i)
		}
	}
	if d.Workloads == nil {
		d.Workloads = []Entry{}
	}
	return d, nil
}

func usableNumber(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// majorOf returns a version string's MAJOR component. Every kubeagent
// schemaVersion is MAJOR.MINOR, both decimal.
//
// The version is quoted with %q in the error: it comes from an
// operator-supplied file, and %q Go-escapes every control byte, so a hostile
// value cannot reach a terminal raw.
func majorOf(v string) (string, error) {
	if len(v) > maxVersionLen {
		return "", fmt.Errorf("baseline schemaVersion is %d bytes, over the %d-byte cap", len(v), maxVersionLen)
	}
	i := strings.Index(v, ".")
	if i <= 0 || i == len(v)-1 {
		return "", fmt.Errorf("baseline schemaVersion %q is not MAJOR.MINOR", v)
	}
	if _, err := strconv.Atoi(v[:i]); err != nil {
		return "", fmt.Errorf("baseline schemaVersion %q is not MAJOR.MINOR", v)
	}
	if _, err := strconv.Atoi(v[i+1:]); err != nil {
		return "", fmt.Errorf("baseline schemaVersion %q is not MAJOR.MINOR", v)
	}
	return v[:i], nil
}
