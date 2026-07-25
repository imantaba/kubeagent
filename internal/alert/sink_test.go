package alert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSink_DeliversAndCounts(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	s.Enqueue(resolvedNotif)
	waitFor(t, "two deliveries", func() bool {
		st := s.Stats()
		return st.FiringOK == 1 && st.ResolvedOK == 1
	})
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"status":"firing"`) {
		t.Fatalf("bodies = %v", bodies)
	}
	if st := s.Stats(); st.LastSuccessUnix == 0 {
		t.Error("LastSuccessUnix must be set after a successful delivery")
	}
}

func TestSink_RetriesServerErrorsThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.backoffBase = time.Millisecond // keep the test fast
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	waitFor(t, "delivery after retries", func() bool { return s.Stats().FiringOK == 1 })
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("server saw %d calls, want 3", calls)
	}
	if st := s.Stats(); st.DroppedRetriesExhausted != 0 {
		t.Errorf("DroppedRetriesExhausted = %d, want 0", st.DroppedRetriesExhausted)
	}
}

func TestSink_GivesUpAfterThreeAttempts(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.backoffBase = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	waitFor(t, "the sink to give up", func() bool { return s.Stats().DroppedRetriesExhausted == 1 })
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("server saw %d calls, want 3", calls)
	}
	if st := s.Stats(); st.FiringFailed != 1 || st.FiringOK != 0 {
		t.Errorf("stats = %+v, want one failed firing", st)
	}
}

func TestSink_ClientErrorIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.backoffBase = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	waitFor(t, "the failure to be counted", func() bool { return s.Stats().FiringFailed == 1 })
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("server saw %d calls, want 1 — a 4xx will not fix itself", calls)
	}
}

func TestSink_QueueFullDropsAndCounts(t *testing.T) {
	// No Start: nothing drains the queue, so it fills at exactly queueSize.
	s, err := New(Config{URL: "https://example.test/hook", Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < queueSize+5; i++ {
		s.Enqueue(firingNotif)
	}
	if got := s.Stats().DroppedQueueFull; got != 5 {
		t.Errorf("DroppedQueueFull = %d, want 5", got)
	}
	if got := len(s.queue); got != queueSize {
		t.Errorf("queue depth = %d, want %d", got, queueSize)
	}
}

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"good json", Config{URL: "https://example.test/hook", Format: FormatJSON, Repeat: time.Hour}, false},
		{"unknown format", Config{URL: "https://example.test/hook", Format: "teletype", Repeat: time.Hour}, true},
		{"bad url", Config{URL: "nope", Format: FormatJSON, Repeat: time.Hour}, true},
		{"alertmanager within the cadence limit", Config{URL: "http://am:9093", Format: FormatAlertmanager, Repeat: time.Minute}, false},
		{"alertmanager repeat too long", Config{URL: "http://am:9093", Format: FormatAlertmanager, Repeat: 4 * time.Hour}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, nil)
			if tc.wantErr != (err != nil) {
				t.Fatalf("New error = %v, wantErr %v", err, tc.wantErr)
			}
			// "webhook" legitimately appears in these messages; what must never
			// appear is the URL itself.
			if err != nil && tc.cfg.URL != "" && strings.Contains(err.Error(), tc.cfg.URL) {
				t.Errorf("validation error echoes the URL: %v", err)
			}
		})
	}
}

func TestDefaultRepeat(t *testing.T) {
	if got := DefaultRepeat(FormatAlertmanager); got != time.Minute {
		t.Errorf("DefaultRepeat(alertmanager) = %s, want 1m0s", got)
	}
	for _, f := range []Format{FormatJSON, FormatSlack} {
		if got := DefaultRepeat(f); got != 4*time.Hour {
			t.Errorf("DefaultRepeat(%s) = %s, want 4h0m0s", f, got)
		}
	}
}
