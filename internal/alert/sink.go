package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

const (
	queueSize      = 64               // bounded: a slow receiver can never grow memory
	attempts       = 3                // then the notification is dropped
	httpTimeout    = 10 * time.Second // per attempt
	defaultBackoff = time.Second      // 1s, then 2s

	// maxAlertmanagerRepeat keeps the re-send interval inside Alertmanager's
	// default resolve_timeout (5m). A longer interval would let a still-firing
	// alert expire and then re-fire — a false recovery.
	maxAlertmanagerRepeat = 4 * time.Minute
)

// Config configures the sink. Repeat is used only to validate the cadence against
// the chosen format; the re-send itself is driven by alertstate.
type Config struct {
	URL    string
	Format Format
	Repeat time.Duration
}

// Stats are monotonic process-lifetime delivery counters.
type Stats struct {
	FiringOK                int64
	FiringFailed            int64
	ResolvedOK              int64
	ResolvedFailed          int64
	DroppedQueueFull        int64
	DroppedRetriesExhausted int64
	LastSuccessUnix         int64
}

// Sink POSTs notifications to one endpoint from a single background goroutine.
// Enqueue never blocks the caller, so a hung receiver cannot stall the daemon's
// reconcile loop.
type Sink struct {
	url         string
	format      Format
	client      *http.Client
	queue       chan alertstate.Notification
	done        chan struct{}
	backoffBase time.Duration

	mu    sync.Mutex
	stats Stats
}

// DefaultRepeat is the re-send interval for a format when the operator did not
// choose one. Alertmanager needs a short cadence because it expires an alert
// resolve_timeout after the last POST; json and slack are notification channels
// where a chatty default is alert fatigue.
func DefaultRepeat(f Format) time.Duration {
	if f == FormatAlertmanager {
		return time.Minute
	}
	return 4 * time.Hour
}

// New validates the configuration and returns a sink. Pass a nil client for the
// default (a 10s per-attempt timeout). Errors never echo the URL.
func New(cfg Config, c *http.Client) (*Sink, error) {
	switch cfg.Format {
	case FormatJSON, FormatSlack, FormatAlertmanager:
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, or alertmanager)", cfg.Format)
	}
	if cfg.Format == FormatAlertmanager && cfg.Repeat > maxAlertmanagerRepeat {
		return nil, fmt.Errorf("alert repeat interval %s exceeds %s, the safe maximum for the alertmanager format (Alertmanager expires an alert resolve_timeout — 5m by default — after the last POST)", cfg.Repeat, maxAlertmanagerRepeat)
	}
	u, err := resolveURL(cfg.URL, cfg.Format)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = &http.Client{Timeout: httpTimeout}
	}
	return &Sink{
		url:         u,
		format:      cfg.Format,
		client:      c,
		queue:       make(chan alertstate.Notification, queueSize),
		done:        make(chan struct{}),
		backoffBase: defaultBackoff,
	}, nil
}

// Start launches the sender goroutine, which runs until ctx is cancelled.
func (s *Sink) Start(ctx context.Context) {
	go func() {
		defer close(s.done)
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-s.queue:
				s.deliver(ctx, n)
			}
		}
	}()
}

// Close waits for the sender goroutine to exit. The caller cancels the context
// passed to Start first.
func (s *Sink) Close() { <-s.done }

// Enqueue hands a notification to the sender without blocking. When the queue is
// full the oldest queued notification is dropped: the newest state is the useful
// one, and unbounded buffering is not an option in a long-lived daemon.
func (s *Sink) Enqueue(n alertstate.Notification) {
	select {
	case s.queue <- n:
		return
	default:
	}
	select {
	case dropped := <-s.queue:
		s.countDrop(dropped, "oldest")
	default:
	}
	select {
	case s.queue <- n:
	default:
		s.countDrop(n, "newest")
	}
}

func (s *Sink) countDrop(n alertstate.Notification, which string) {
	s.mu.Lock()
	s.stats.DroppedQueueFull++
	s.mu.Unlock()
	log.Printf("kubeagent: alert queue full, dropped the %s notification for %s", which, n.Object)
}

// Stats returns the delivery counters.
func (s *Sink) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// deliver encodes and POSTs one notification, retrying server-side failures.
func (s *Sink) deliver(ctx context.Context, n alertstate.Notification) {
	body, err := encode(s.format, n)
	if err != nil {
		log.Printf("kubeagent: encoding alert for %s: %v", n.Object, err)
		s.record(n, false)
		return
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		status, err := s.post(ctx, body)
		switch {
		case err == nil && status < 300:
			s.record(n, true)
			return
		case err == nil && status < 500:
			log.Printf("kubeagent: alert for %s rejected by %s: HTTP %d (not retrying)", n.Object, RedactURL(s.url), status)
			s.record(n, false)
			return
		case err != nil:
			log.Printf("kubeagent: alert delivery to %s failed (attempt %d/%d): %v", RedactURL(s.url), attempt, attempts, err)
		default:
			log.Printf("kubeagent: alert delivery to %s failed (attempt %d/%d): HTTP %d", RedactURL(s.url), attempt, attempts, status)
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			s.record(n, false)
			return
		case <-time.After(s.backoffBase * time.Duration(1<<(attempt-1))):
		}
	}
	s.record(n, false)
	s.mu.Lock()
	s.stats.DroppedRetriesExhausted++
	s.mu.Unlock()
	log.Printf("kubeagent: dropping alert for %s after %d failed attempts", n.Object, attempts)
}

// post sends one attempt and returns the status code. Transport errors are
// sanitized: net/http embeds the full URL in *url.Error.
func (s *Sink) post(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("building the alert request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, sanitizeErr(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// record folds one delivery outcome into the counters.
func (s *Sink) record(n alertstate.Notification, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case n.Status == alertstate.StatusResolved && ok:
		s.stats.ResolvedOK++
	case n.Status == alertstate.StatusResolved:
		s.stats.ResolvedFailed++
	case ok:
		s.stats.FiringOK++
	default:
		s.stats.FiringFailed++
	}
	if ok {
		s.stats.LastSuccessUnix = time.Now().Unix()
	}
}
