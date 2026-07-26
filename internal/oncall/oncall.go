package oncall

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

const (
	// queueSize bounds admitted-but-not-yet-run jobs. Small on purpose: a
	// backlog of stale explanations helps nobody, and the drop is counted.
	queueSize = 8
	// maxLatest caps the served explanation store. An unbounded map in a
	// process designed to run for months is not an option.
	maxLatest = 100
	// maxLatestAge drops explanations nobody will look at again.
	maxLatestAge = 24 * time.Hour
	// defaultTimeout bounds one model call.
	defaultTimeout = 60 * time.Second
	// closeGrace bounds how long Close waits for the worker, matching the
	// metrics server's shutdown grace in internal/watch.
	closeGrace = 5 * time.Second
)

// errEmpty marks a model response that parsed but said nothing.
var errEmpty = errors.New("model returned no text")

// IncidentExplainer is the model call. *explain.Client satisfies it; tests use a
// fake. Note what is absent: nothing in this package accepts a Kubernetes
// client, so an explainer structurally cannot read the cluster.
type IncidentExplainer interface {
	ExplainIncident(ctx context.Context, prompt string) (string, error)
}

// Explanation is one delivered explanation, as served by /explanations.
type Explanation struct {
	Kind        string
	Namespace   string
	Name        string
	Issues      []string
	ExplainedAt time.Time
	Model       string
	Text        string
}

// Stats are process-lifetime counters plus the current budget reading.
type Stats struct {
	Allowed         int64
	Throttled       int64
	Failed          int64
	Dropped         int64
	BudgetRemaining float64
}

// Config configures an Explainer.
type Config struct {
	Client   IncidentExplainer
	Model    string
	Cooldown time.Duration
	Budget   int
	Notify   func(alertstate.Notification)
	Timeout  time.Duration // 0 takes defaultTimeout
}

// job is one self-contained unit of work. The prompt is rendered on the
// reconcile goroutine so nothing the worker touches can be mutated underneath
// it by the next evaluation.
type job struct {
	obj         alertstate.Object
	issues      []string
	firingSince time.Time
	prompt      string
}

// Explainer turns object transitions into model-written follow-up
// notifications, bounded by a throttle and a small job queue. A nil *Explainer
// is a valid, inert explainer: every method is a no-op, so the reconcile loop
// needs no conditional.
type Explainer struct {
	client  IncidentExplainer
	model   string
	notify  func(alertstate.Notification)
	timeout time.Duration
	th      *Throttle
	jobs    chan job
	done    chan struct{}

	// primed guards the cold start and is touched only by Consider, on the
	// reconcile goroutine. dropped likewise.
	primed  bool
	dropped int64

	mu      sync.Mutex
	started bool
	failed  int64
	latest  map[string]Explanation
}

// New builds an Explainer. Start must be called before it does any work.
func New(cfg Config) *Explainer {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Explainer{
		client:  cfg.Client,
		model:   cfg.Model,
		notify:  cfg.Notify,
		timeout: timeout,
		th:      NewThrottle(cfg.Cooldown, cfg.Budget),
		jobs:    make(chan job, queueSize),
		done:    make(chan struct{}),
		latest:  map[string]Explanation{},
	}
}

// Start launches the single worker goroutine, which runs until ctx is
// cancelled. After the first call, Start is a no-op.
func (e *Explainer) Start(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	go func() {
		defer close(e.done)
		for {
			select {
			case <-ctx.Done():
				return
			case j := <-e.jobs:
				e.run(ctx, j)
			}
		}
	}()
}

// Close waits for the worker to exit, up to closeGrace. The caller cancels the
// context passed to Start first. Close on an explainer whose Start was never
// called returns immediately rather than blocking forever.
func (e *Explainer) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-e.done:
	case <-time.After(closeGrace):
		log.Printf("kubeagent: explanation worker did not stop within %s", closeGrace)
	}
}

// Consider admits the objects that just broke. It runs on the reconcile
// goroutine and never blocks: the throttle is in-memory and the send is
// non-blocking.
//
// The trigger is watchstate.Delta.New deduplicated by object. That fires for an
// already-broken object acquiring an additional issue as well as for a genuine
// clean-to-flagged transition, which the per-object cooldown absorbs: a second
// finding minutes later is inside the cooldown, and a new failure mode an hour
// later is an escalation that deserves a fresh explanation.
func (e *Explainer) Consider(d watchstate.Delta, cluster clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue, now time.Time) {
	if e == nil {
		return
	}
	// The first reconcile is the initial snapshot, not a set of transitions.
	// Explaining it would spend the whole budget on pre-existing problems every
	// time the daemon restarts.
	if !e.primed {
		e.primed = true
		return
	}

	for _, obj := range objectsFrom(d.New) {
		if !e.th.Allow(obj.key, now) {
			continue
		}
		j := job{
			obj:         obj.obj,
			issues:      obj.issues,
			firingSince: obj.firingSince,
			prompt:      explain.BuildIncidentPrompt(obj.obj.String(), obj.issues, cluster, flagged, serviceIssues),
		}
		select {
		case e.jobs <- j:
		default:
			// Admitted but not run: the token and the cooldown stamp are
			// already spent. Counted separately from a throttle refusal
			// because the cause and the operator's response differ — a full
			// queue means the endpoint is slow, not that policy said no.
			e.dropped++
			log.Printf("kubeagent: explanation queue full, dropped %s", obj.obj)
		}
	}
}

// Latest returns the stored explanations, newest first, pruned by age.
func (e *Explainer) Latest() []Explanation {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Explanation, 0, len(e.latest))
	for _, x := range e.latest {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExplainedAt.After(out[j].ExplainedAt) })
	return out
}

// Stats returns the counters and the current budget reading. It is called from
// the reconcile goroutine, alongside the other tracker snapshots.
func (e *Explainer) Stats(now time.Time) Stats {
	if e == nil {
		return Stats{}
	}
	allowed, throttled := e.th.Counters()
	e.mu.Lock()
	failed := e.failed
	e.mu.Unlock()
	return Stats{
		Allowed:         allowed,
		Throttled:       throttled,
		Failed:          failed,
		Dropped:         e.dropped,
		BudgetRemaining: e.th.Remaining(now),
	}
}

// run performs one model call and delivers the result.
func (e *Explainer) run(ctx context.Context, j job) {
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	text, err := e.client.ExplainIncident(callCtx, j.prompt)
	cancel()
	if err == nil && strings.TrimSpace(text) == "" {
		// Defensive: the interface is the seam, so an implementation that
		// returns blank text without an error must not become a blank page.
		err = errEmpty
	}
	if err != nil {
		e.mu.Lock()
		e.failed++
		e.mu.Unlock()
		// The error may name the endpoint; log the object and the failure
		// class only, never the error's URL or any credential.
		log.Printf("kubeagent: explanation failed for %s", j.obj)
		return
	}
	text = strings.TrimSpace(text)

	now := time.Now()
	e.store(Explanation{
		Kind: j.obj.Kind, Namespace: j.obj.Namespace, Name: j.obj.Name,
		Issues: j.issues, ExplainedAt: now, Model: e.model, Text: text,
	})
	if e.notify != nil {
		e.notify(alertstate.Notification{
			Object:      j.obj,
			Status:      alertstate.StatusFiring,
			Reason:      alertstate.ReasonExplanation,
			Issues:      j.issues,
			FiringSince: j.firingSince,
			Text:        text,
		})
	}
}

// store records the newest explanation for an object, pruning by age and
// evicting the oldest entry once the store is full.
func (e *Explainer) store(x Explanation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, old := range e.latest {
		if x.ExplainedAt.Sub(old.ExplainedAt) > maxLatestAge {
			delete(e.latest, k)
		}
	}
	key := x.Kind + "/" + x.Namespace + "/" + x.Name
	if _, replacing := e.latest[key]; !replacing {
		for len(e.latest) >= maxLatest {
			oldestKey, oldestAt := "", time.Time{}
			for k, old := range e.latest {
				if oldestAt.IsZero() || old.ExplainedAt.Before(oldestAt) {
					oldestKey, oldestAt = k, old.ExplainedAt
				}
			}
			delete(e.latest, oldestKey)
		}
	}
	e.latest[key] = x
}

// objectRef is one object's worth of a delta, with its issues collected.
type objectRef struct {
	key         string
	obj         alertstate.Object
	issues      []string
	firingSince time.Time
}

// objectsFrom folds per-issue records into one entry per object, in a stable
// order so a storm produces a deterministic admission sequence rather than one
// that depends on map iteration.
func objectsFrom(records []watchstate.Record) []objectRef {
	index := map[string]*objectRef{}
	var order []string
	for _, r := range records {
		key := r.Key.Kind + "/" + r.Key.Namespace + "/" + r.Key.Name
		ref, ok := index[key]
		if !ok {
			ref = &objectRef{
				key: key,
				obj: alertstate.Object{Kind: r.Key.Kind, Namespace: r.Key.Namespace, Name: r.Key.Name},
			}
			index[key] = ref
			order = append(order, key)
		}
		ref.issues = append(ref.issues, r.Key.Issue)
		if ref.firingSince.IsZero() || r.FiringSince.Before(ref.firingSince) {
			ref.firingSince = r.FiringSince
		}
	}
	out := make([]objectRef, 0, len(order))
	for _, key := range order {
		ref := index[key]
		sort.Strings(ref.issues)
		out = append(out, *ref)
	}
	return out
}
