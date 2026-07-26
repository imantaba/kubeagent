// Package oncall decides which broken objects earn a model-written explanation
// and delivers those explanations without ever touching the cluster. Nothing
// here holds a Kubernetes client: the caller passes findings that have already
// been collected, which is what keeps the watch daemon's read-only guarantee a
// property of the type signatures rather than a convention.
package oncall

import "time"

// Throttle decides which objects earn a model call. It is pure — no I/O, no
// goroutines, and no wall-clock reads: the caller passes now. A Throttle is not
// safe for concurrent use; the daemon touches it only from its reconcile loop.
//
// Two guards, for two different ways spend runs away. The per-object cooldown
// stops one flapping workload from being re-explained every reconcile. The
// global token bucket bounds a mass outage, where many distinct objects each
// legitimately clear their cooldown at once.
type Throttle struct {
	cooldown time.Duration
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
	seen     map[string]time.Time

	allowed   int64
	throttled int64
}

// NewThrottle returns a throttle with a per-object cooldown and a budget in
// calls per hour. The budget is also the bucket's capacity, so a genuine mass
// outage gets its whole allowance immediately and then drips.
func NewThrottle(cooldown time.Duration, budget int) *Throttle {
	if budget < 1 {
		budget = 1
	}
	return &Throttle{
		cooldown: cooldown,
		capacity: float64(budget),
		perSec:   float64(budget) / 3600,
		tokens:   float64(budget),
		seen:     map[string]time.Time{},
	}
}

// Allow reports whether this object may be explained now, and records the
// decision. The cooldown is checked first because it costs nothing: were the
// budget checked first, an object that is about to be refused anyway would burn
// a token some other object needs.
func (t *Throttle) Allow(key string, now time.Time) bool {
	t.refill(now)
	t.prune(now)

	if stamped, ok := t.seen[key]; ok && now.Sub(stamped) < t.cooldown {
		t.throttled++
		return false
	}
	if t.tokens < 1 {
		t.throttled++
		return false
	}
	// Stamp only on success. A budget-denied object was never explained, so
	// stamping it would silence it twice for the same refusal.
	t.tokens--
	t.seen[key] = now
	t.allowed++
	return true
}

// Counters returns the process-lifetime decision counts.
func (t *Throttle) Counters() (allowed, throttled int64) {
	return t.allowed, t.throttled
}

// Remaining projects the tokens available at now without consuming or refilling
// anything, so a metrics read can never change an admission decision.
func (t *Throttle) Remaining(now time.Time) float64 {
	if t.last.IsZero() {
		return t.capacity
	}
	r := t.tokens + now.Sub(t.last).Seconds()*t.perSec
	if r > t.capacity {
		r = t.capacity
	}
	if r < 0 {
		r = 0
	}
	return r
}

func (t *Throttle) refill(now time.Time) {
	if t.last.IsZero() {
		t.last = now
		return
	}
	if elapsed := now.Sub(t.last).Seconds(); elapsed > 0 {
		t.tokens += elapsed * t.perSec
		if t.tokens > t.capacity {
			t.tokens = t.capacity
		}
	}
	t.last = now
}

// prune drops stamps that can no longer block anything. Only allowed calls are
// stamped and every stamp expires after the cooldown, so this bounds the map at
// roughly budget-rate x cooldown entries — about 20 at the defaults.
func (t *Throttle) prune(now time.Time) {
	for k, stamped := range t.seen {
		if now.Sub(stamped) >= t.cooldown {
			delete(t.seen, k)
		}
	}
}
