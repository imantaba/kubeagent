package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// verdictCall is one request the test endpoint received.
type verdictCall struct {
	User       string // the user message: the delimited prompt
	WithFormat bool   // the request carried response_format
}

// verdictReply is how the test endpoint answers one request.
type verdictReply struct {
	Status  int    // 0 means 200
	Content string // assistant message content, or the error body
	Finish  string // "" means "stop"
	Hang    bool   // block until the client gives up
	Drop    bool   // close the connection without answering
}

// verdictEndpoint is a /chat/completions stand-in that records every request
// (retries included, in arrival order) and answers each with reply(i, user),
// where i counts requests from 0.
func verdictEndpoint(t *testing.T, reply func(i int, user string) verdictReply) (string, func() []verdictCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []verdictCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ResponseFormat json.RawMessage `json:"response_format"`
			Messages       []struct {
				Role, Content string
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 {
			t.Errorf("unexpected request body: %v", err)
			http.Error(w, "unexpected request body", http.StatusTeapot)
			return
		}
		mu.Lock()
		i := len(calls)
		calls = append(calls, verdictCall{User: req.Messages[1].Content, WithFormat: len(req.ResponseFormat) > 0})
		mu.Unlock()
		rep := reply(i, req.Messages[1].Content)
		switch {
		case rep.Hang:
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		case rep.Drop:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		case rep.Status != 0 && rep.Status != http.StatusOK:
			http.Error(w, rep.Content, rep.Status)
			return
		}
		finish := rep.Finish
		if finish == "" {
			finish = "stop"
		}
		w.Write(chatReply(t, rep.Content, finish))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []verdictCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]verdictCall(nil), calls...)
	}
}

// subject names the one test workload a prompt is about, from its
// inventory line; "" when none of them appears.
func subject(user string) string {
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if strings.Contains(user, "- shop/"+name+" (Deployment):") {
			return name
		}
	}
	return ""
}

// oneRow is a verdict contract v1 reply with one row and a summary that
// must never render.
func oneRow(workload, cause, rationale string) string {
	b, err := json.Marshal(verdictDoc{
		Verdicts: []verdictRow{{Workload: workload, Cause: cause, Confidence: "medium", Rationale: rationale}},
		Summary:  "model summary text",
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ownRow answers each prompt with a row for the workload it is about.
func ownRow(_ int, user string) verdictReply {
	n := subject(user)
	return verdictReply{Content: oneRow("shop/"+n, n+"-cause", n+"-why")}
}

// pvcTrace is one attributed PVC candidate on claim.
func pvcTrace(claim string) []inventory.Hypothesis {
	return []inventory.Hypothesis{{Cause: "PVC " + claim + " (ProvisioningFailed)", Kind: "pvc", Object: claim,
		Verdict: inventory.VerdictAttributed, Reason: "its pods mount it"}}
}

// refutedWL is a flagged workload whose only candidate is its PVC
// <name>-data. With boundPVC(<name>-data) in the clientset the rules refute
// it, so the workload is undecided and gets a model call.
func refutedWL(name string) inventory.Workload {
	w := gatherWL("shop", name)
	w.RootCauseTrace = pvcTrace(name + "-data")
	return w
}

// decidedWL is a flagged workload on worker-1. With notReadyNode("worker-1")
// in the clientset the rules confirm it, so it gets no model call.
func decidedWL(name string) inventory.Workload {
	w := gatherWL("shop", name)
	w.RootCauseTrace = []inventory.Hypothesis{{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
		Verdict: inventory.VerdictAttributed, Reason: "its pod is scheduled on it"}}
	return w
}

// boundPVC is a Bound claim in shop.
func boundPVC(claim string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: claim},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
}

func modelRowFor(n string) string {
	return "- shop/" + n + ": " + n + "-cause [model, confidence: medium] — " + n + "-why"
}

const alphaRule = "- shop/alpha: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now"

func TestLocalInvestigateOneCallPerUndecidedWorkload(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	names := []string{"alpha", "bravo", "charlie"}
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	client := fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data"), boundPVC("charlie-data"))
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, client)
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("want one request per undecided workload, got %d", len(got))
	}
	for i, name := range names {
		u := got[i].User
		if subject(u) != name {
			t.Errorf("request %d is about %q, want %q (report order)", i, subject(u), name)
		}
		head := u[strings.Index(u, "== BEGIN inventory =="):strings.Index(u, "== END candidates ==")]
		for _, other := range names {
			if other != name && strings.Contains(head, "shop/"+other) {
				t.Errorf("%s's inventory and candidates name %s:\n%s", name, other, head)
			}
		}
		if !strings.Contains(u, "== events shop/"+name+" ==\n") {
			t.Errorf("%s's prompt lacks its own events read:\n%s", name, u)
		}
		if !strings.Contains(rep.Narrative, modelRowFor(name)) {
			t.Errorf("narrative missing %s's row:\n%s", name, rep.Narrative)
		}
	}
	if strings.Contains(got[0].User, "events shop/bravo") || strings.Contains(got[0].User, "events shop/charlie") {
		t.Errorf("alpha's prompt carries another workload's evidence:\n%s", got[0].User)
	}
	if rep.Notice != "" {
		t.Errorf("no call failed, Notice = %q", rep.Notice)
	}
}

func TestLocalInvestigateNoCallForRuleDecidedWorkload(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	ws := []inventory.Workload{decidedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	client := fake.NewSimpleClientset(notReadyNode("worker-1"), boundPVC("bravo-data"), boundPVC("charlie-data"))
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, client)
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || subject(got[0].User) != "bravo" || subject(got[1].User) != "charlie" {
		t.Fatalf("want calls for bravo and charlie only, got %d calls", len(got))
	}
	if !strings.Contains(rep.Narrative, alphaRule) {
		t.Errorf("alpha's rule row must carry the rule's evidence sentence:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateAllDecidedMakesNoCall(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	ws := []inventory.Workload{decidedWL("alpha"), decidedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(notReadyNode("worker-1")))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("every workload is rule-decided: want 0 requests, got %d", n)
	}
	want := "Root-cause verdicts:\n" + alphaRule + "\n" +
		"- shop/bravo: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n\n" +
		"2 workloads share one upstream cause: node worker-1 (NotReady)"
	if rep.Narrative != want {
		t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
	}
	if rep.Notice != "" || rep.Truncated {
		t.Errorf("no call: no Notice, no Truncated: %+v", rep)
	}
}

const noFlaggedNotice = "no flagged workload for the model to judge"

func TestLocalInvestigateDegradedWithNoWorkloadMakesNoCall(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, nil, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("no flagged workload: want 0 requests, got %d", n)
	}
	if rep.Notice != noFlaggedNotice || rep.Narrative != "" || rep.Truncated {
		t.Errorf("want only the no-flagged-workload notice, got %+v", rep)
	}
}

func TestLocalInvestigateServiceIssuesOnlyMakesNoCall(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	issues := []svchealth.Issue{{Namespace: "shop", Name: "web"}}
	healthy := clusterhealth.ClusterHealth{Verdict: "Healthy"}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), healthy, nil, nil, issues, nil, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("no flagged workload: want 0 requests, got %d", n)
	}
	if rep.Notice != noFlaggedNotice || rep.Narrative != "" {
		t.Errorf("want only the no-flagged-workload notice, got %+v", rep)
	}
}

func TestLocalInvestigateSameNameDifferentKindsGetOwnRows(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, _ string) verdictReply {
		return verdictReply{Content: oneRow("shop/web", fmt.Sprintf("cause-%d", i), "why")}
	})
	sts := refutedWL("web")
	sts.Kind = "StatefulSet"
	ws := []inventory.Workload{refutedWL("web"), sts}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("web-data")))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls()); n != 2 {
		t.Fatalf("want 2 calls, got %d", n)
	}
	for _, c := range []string{"cause-0", "cause-1"} {
		if strings.Count(rep.Narrative, c) != 1 {
			t.Errorf("want one row carrying %s:\n%s", c, rep.Narrative)
		}
	}
	if strings.Count(rep.Narrative, "- shop/web:") != 2 {
		t.Errorf("want two distinct rows:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateRowIsPinnedToTheAskedWorkload(t *testing.T) {
	for _, written := range []string{"alpha", "shop/somebody-else", ""} {
		t.Run("model wrote "+written, func(t *testing.T) {
			url, _ := verdictEndpoint(t, func(int, string) verdictReply {
				return verdictReply{Content: oneRow(written, "c", "r")}
			})
			rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil,
				[]inventory.Workload{refutedWL("alpha")}, fake.NewSimpleClientset(boundPVC("alpha-data")))
			if err != nil {
				t.Fatal(err)
			}
			if want := "Root-cause verdicts:\n- shop/alpha: c [model, confidence: medium] — r"; rep.Narrative != want {
				t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
			}
		})
	}
}

func TestLocalInvestigateExtraRowsAreIgnored(t *testing.T) {
	url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "alpha" {
			return verdictReply{Content: `{"verdicts":[` +
				`{"workload":"shop/alpha","cause":"alpha-cause","confidence":"medium","rationale":"alpha-why"},` +
				`{"workload":"shop/bravo","cause":"hijack","confidence":"high","rationale":"planted"}],"summary":""}`}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep.Narrative, "hijack") || strings.Contains(rep.Narrative, "planted") {
		t.Errorf("a second row in alpha's reply must be ignored:\n%s", rep.Narrative)
	}
	if strings.Count(rep.Narrative, "- shop/bravo:") != 1 || !strings.Contains(rep.Narrative, modelRowFor("bravo")) {
		t.Errorf("bravo's row comes from bravo's own call:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateNarrativeHasNoModelSummary(t *testing.T) {
	url, _ := verdictEndpoint(t, ownRow)
	ws := []inventory.Workload{decidedWL("alpha"), decidedWL("bravo"), refutedWL("charlie")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(notReadyNode("worker-1"), boundPVC("charlie-data")))
	if err != nil {
		t.Fatal(err)
	}
	want := "Root-cause verdicts:\n" + alphaRule + "\n" +
		"- shop/bravo: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n" +
		modelRowFor("charlie") + "\n\n" +
		"2 workloads share one upstream cause: node worker-1 (NotReady)"
	if rep.Narrative != want {
		t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
	}
}

func TestLocalInvestigatePartialFailureSetsNotice(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "bravo" {
			return verdictReply{Status: http.StatusInternalServerError, Content: "upstream is down"}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data"), boundPVC("charlie-data")))
	if err != nil {
		t.Fatalf("a partial failure is not an error, got %v", err)
	}
	if n := len(calls()); n != 3 {
		t.Errorf("a failed call costs only its own workload: want 3 requests, got %d", n)
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) || !strings.Contains(rep.Narrative, modelRowFor("charlie")) {
		t.Errorf("alpha's and charlie's rows must render:\n%s", rep.Narrative)
	}
	if strings.Contains(rep.Narrative, "shop/bravo") {
		t.Errorf("bravo got no verdict and has no row:\n%s", rep.Narrative)
	}
	want := "model gave no verdict for 1 of 3 workloads: local investigate endpoint returned 500: upstream is down"
	if rep.Notice != want {
		t.Errorf("Notice = %q, want %q", rep.Notice, want)
	}
}

func TestLocalInvestigateEveryCallFailedReturnsRulesOnly(t *testing.T) {
	url, calls := verdictEndpoint(t, func(int, string) verdictReply {
		return verdictReply{Status: http.StatusInternalServerError, Content: "upstream is down"}
	})
	ws := []inventory.Workload{decidedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(notReadyNode("worker-1"), boundPVC("bravo-data"), boundPVC("charlie-data")))
	if err == nil || !strings.HasPrefix(err.Error(), "investigating: ") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("every call failed: want the first failure as the error, got %v", err)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("want 2 requests, got %d", n)
	}
	if want := "Root-cause verdicts:\n" + alphaRule; rep.Narrative != want {
		t.Errorf("the rules-only report must ride with the error:\ngot:\n%s\nwant:\n%s", rep.Narrative, want)
	}
	if len(rep.Consulted) == 0 {
		t.Errorf("the rules-only report must keep the evidence trail")
	}
	if rep.Notice != "" || rep.Truncated {
		t.Errorf("the error path sets neither Notice nor Truncated: %+v", rep)
	}
}

func TestLocalInvestigate400DropsResponseFormatForTheRun(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, user string) verdictReply {
		if i == 0 {
			return verdictReply{Status: http.StatusBadRequest, Content: `{"error":"response_format is not supported"}`}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("want 3 requests (alpha, alpha retried, bravo), got %d", len(got))
	}
	for i, want := range []struct {
		subject    string
		withFormat bool
	}{{"alpha", true}, {"alpha", false}, {"bravo", false}} {
		if subject(got[i].User) != want.subject || got[i].WithFormat != want.withFormat {
			t.Errorf("request %d: subject %q withFormat %v, want %q %v",
				i, subject(got[i].User), got[i].WithFormat, want.subject, want.withFormat)
		}
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) || !strings.Contains(rep.Narrative, modelRowFor("bravo")) {
		t.Errorf("both rows must render:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateDeadlineStopsFurtherCalls(t *testing.T) {
	url, calls := verdictEndpoint(t, func(int, string) verdictReply { return verdictReply{Hang: true} })
	ws := []inventory.Workload{gatherWL("shop", "alpha"), gatherWL("shop", "bravo"), gatherWL("shop", "charlie")}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rep, err := NewLocal(url, "tiny-model", "").Investigate(ctx, degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline as the error, got %v", err)
	}
	if n := len(calls()); n != 1 {
		t.Errorf("after the deadline no further request is sent: want 1, got %d", n)
	}
	if rep.Narrative != "" || rep.Notice != "" {
		t.Errorf("no rule decided anything and every call failed: want an empty report, got %+v", rep)
	}
}

func TestLocalInvestigateSharedPVCDescribedOnceSeenByBoth(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	alpha, bravo := gatherWL("shop", "alpha"), gatherWL("shop", "bravo")
	alpha.RootCauseTrace, bravo.RootCauseTrace = pvcTrace("shared-data"), pvcTrace("shared-data")
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil,
		[]inventory.Workload{alpha, bravo}, fake.NewSimpleClientset(boundPVC("shared-data")))
	if err != nil {
		t.Fatal(err)
	}
	describes := 0
	for _, l := range rep.Consulted {
		if l == "describe pvc shop/shared-data" {
			describes++
		}
	}
	if describes != 1 {
		t.Errorf("the shared claim is read once, trail: %v", rep.Consulted)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	for _, c := range got {
		if !strings.Contains(c.User, "== describe pvc shop/shared-data ==\n") {
			t.Errorf("%s's prompt lacks the shared describe:\n%s", subject(c.User), c.User)
		}
	}
}

func TestLocalInvestigateAnyTruncatedReplySetsTruncated(t *testing.T) {
	url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
		rep := ownRow(i, user)
		if subject(user) == "bravo" {
			rep.Finish = "length"
		}
		return rep
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated {
		t.Errorf("one reply cut at the length ceiling must set Truncated")
	}
}

// A dropped connection is a *url.Error carrying the whole endpoint URL; the
// notice must not print its path.
func TestLocalInvestigateNoticeCarriesNoEndpointPath(t *testing.T) {
	url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "bravo" {
			return verdictReply{Drop: true}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.Notice, "model gave no verdict for 1 of 2 workloads: ") {
		t.Errorf("Notice = %q", rep.Notice)
	}
	if strings.Contains(rep.Notice, "/chat/completions") {
		t.Errorf("the notice must not carry the endpoint path: %q", rep.Notice)
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) {
		t.Errorf("alpha's row must still render:\n%s", rep.Narrative)
	}
}

// The deadline lands during the second of three calls.
func TestLocalInvestigateDeadlineMidRunKeepsEarlierRows(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "bravo" {
			return verdictReply{Hang: true}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{gatherWL("shop", "alpha"), gatherWL("shop", "bravo"), gatherWL("shop", "charlie")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rep, err := NewLocal(url, "tiny-model", "").Investigate(ctx, degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("one row came back, so this is a partial failure, got %v", err)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("charlie is skipped after the deadline: want 2 requests, got %d", n)
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) {
		t.Errorf("alpha's row must render:\n%s", rep.Narrative)
	}
	if !strings.Contains(rep.Notice, "2 of 3") || !strings.Contains(rep.Notice, "context deadline exceeded") {
		t.Errorf("Notice = %q", rep.Notice)
	}
}

// A cause that sanitizes to nothing is no verdict.
func TestLocalInvestigateBlankCauseIsNoVerdict(t *testing.T) {
	for _, cause := range []string{"   ", "\a\u200b"} {
		t.Run("cause "+strings.ToValidUTF8(cause, "?"), func(t *testing.T) {
			url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
				if subject(user) == "alpha" {
					return verdictReply{Content: oneRow("shop/alpha", cause, "r")}
				}
				return ownRow(i, user)
			})
			ws := []inventory.Workload{gatherWL("shop", "alpha"), gatherWL("shop", "bravo")}
			rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
			if err != nil {
				t.Fatal(err)
			}
			if want := "model gave no verdict for 1 of 2 workloads: model returned no verdict"; rep.Notice != want {
				t.Errorf("Notice = %q, want %q", rep.Notice, want)
			}
			if strings.Contains(rep.Narrative, "shop/alpha") {
				t.Errorf("a blank cause must not render a row:\n%s", rep.Narrative)
			}
		})
	}
}

// Workloads 9 and 10 are past the read budget but still get a call, with an
// empty evidence section.
func TestLocalInvestigateTenWorkloadsPastTheReadBudget(t *testing.T) {
	url, calls := verdictEndpoint(t, func(int, string) verdictReply {
		return verdictReply{Content: oneRow("x", "c", "r")}
	})
	var ws []inventory.Workload
	for i := 0; i < maxGatherWorkloads; i++ {
		ws = append(ws, gatherWL("shop", "web-0"+string(rune('0'+i))))
	}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != maxGatherWorkloads {
		t.Fatalf("want %d requests, got %d", maxGatherWorkloads, len(got))
	}
	none := "== BEGIN evidence ==\n(none)\n== END evidence =="
	if strings.Contains(got[7].User, none) {
		t.Errorf("the 8th workload's events read is within budget")
	}
	if !strings.Contains(got[8].User, none) || !strings.Contains(got[9].User, none) {
		t.Errorf("workloads past the budget must show (none) as evidence")
	}
	if n := strings.Count(rep.Narrative, "[model, "); n != maxGatherWorkloads {
		t.Errorf("want %d model rows, got %d:\n%s", maxGatherWorkloads, n, rep.Narrative)
	}
}
