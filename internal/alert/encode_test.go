package alert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

var (
	at      = time.Date(2026, 7, 25, 10, 4, 11, 0, time.UTC)
	cleared = time.Date(2026, 7, 25, 10, 8, 23, 0, time.UTC)

	firingNotif = alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonChanged,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: at,
	}
	resolvedNotif = alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusResolved,
		Reason:      alertstate.ReasonResolved,
		FiringSince: at,
		ResolvedAt:  cleared,
	}
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		notif  alertstate.Notification
		want   string
	}{
		{
			name:   "json firing",
			format: FormatJSON,
			notif:  firingNotif,
			want:   `{"status":"firing","reason":"changed","cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"firingSince":"2026-07-25T10:04:11Z","flapping":false}`,
		},
		{
			name:   "json resolved carries an empty issue list, never null",
			format: FormatJSON,
			notif:  resolvedNotif,
			want:   `{"status":"resolved","reason":"resolved","cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":[],"firingSince":"2026-07-25T10:04:11Z","resolvedAt":"2026-07-25T10:08:23Z","flapping":false}`,
		},
		{
			name:   "slack firing",
			format: FormatSlack,
			notif:  firingNotif,
			want:   `{"text":"*FIRING* local/Deployment/shop/web\nissues: ImagePullBackOff\nfiring since 2026-07-25T10:04:11Z"}`,
		},
		{
			name:   "slack resolved reports the duration",
			format: FormatSlack,
			notif:  resolvedNotif,
			want:   `{"text":"*RESOLVED* local/Deployment/shop/web (fired for 4m12s)"}`,
		},
		{
			name:   "alertmanager firing omits endsAt and keeps issues in annotations",
			format: FormatAlertmanager,
			notif:  firingNotif,
			want:   `[{"labels":{"alertname":"KubeagentIssue","cluster":"local","kind":"Deployment","name":"web","namespace":"shop"},"annotations":{"flapping":"false","issues":"ImagePullBackOff"},"startsAt":"2026-07-25T10:04:11Z"}]`,
		},
		{
			name:   "alertmanager resolved sets endsAt",
			format: FormatAlertmanager,
			notif:  resolvedNotif,
			want:   `[{"labels":{"alertname":"KubeagentIssue","cluster":"local","kind":"Deployment","name":"web","namespace":"shop"},"annotations":{"flapping":"false","issues":""},"startsAt":"2026-07-25T10:04:11Z","endsAt":"2026-07-25T10:08:23Z"}]`,
		},
		{
			name:   "pagerduty firing new",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonNew,
				Issues:      []string{"ImagePullBackOff"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"reason":"new","flapping":false}}}`,
		},
		{
			name:   "pagerduty firing changed lists every issue in the summary",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonChanged,
				Issues:      []string{"ErrImagePull", "ImagePullBackOff"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ErrImagePull, ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ErrImagePull","ImagePullBackOff"],"reason":"changed","flapping":false}}}`,
		},
		{
			name:   "pagerduty repeat is a trigger on the same dedup key",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonRepeat,
				Issues:      []string{"ImagePullBackOff"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"reason":"repeat","flapping":false}}}`,
		},
		{
			name:   "pagerduty explanation carries the prose in custom_details, never in the summary",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonExplanation,
				Issues:      []string{"ImagePullBackOff"},
				FiringSince: at,
				Text:        "The image tag does not exist in the registry.",
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"reason":"explanation","flapping":false,"explanation":"The image tag does not exist in the registry."}}}`,
		},
		{
			name:   "pagerduty resolved carries only the three required fields",
			format: FormatPagerDuty,
			notif:  resolvedNotif,
			want:   `{"routing_key":"not-a-real-routing-key","event_action":"resolve","dedup_key":"local/Deployment/shop/web"}`,
		},
		{
			name:   "pagerduty flapping is a detail, not part of the summary",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonChanged,
				Issues:      []string{"CrashLoopBackOff"},
				FiringSince: at,
				Flapping:    true,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: CrashLoopBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["CrashLoopBackOff"],"reason":"changed","flapping":true}}}`,
		},
		{
			name:   "pagerduty cluster-scoped object omits the namespace",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Node", Name: "worker-2"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonNew,
				Issues:      []string{"NodeNotReady"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Node/worker-2","payload":{"summary":"local/Node/worker-2: NodeNotReady","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Node","name":"worker-2","issues":["NodeNotReady"],"reason":"new","flapping":false}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encode(tc.format, "not-a-real-routing-key", tc.notif)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("encode =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestEncode_SlackFlaggingAndClusterScope(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Node", Name: "worker-2"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"KubeletUnhealthy", "NotReady"},
		FiringSince: at,
		Flapping:    true,
	}
	got, err := encode(FormatSlack, "", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"text":"*FIRING* local/Node/worker-2\nissues: KubeletUnhealthy, NotReady\nfiring since 2026-07-25T10:04:11Z (flapping)"}`
	if string(got) != want {
		t.Errorf("encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncode_ClusterScopedAlertmanagerOmitsNamespaceLabel(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Node", Name: "worker-2"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"KubeletUnhealthy"},
		FiringSince: at,
	}
	got, err := encode(FormatAlertmanager, "", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `[{"labels":{"alertname":"KubeagentIssue","cluster":"local","kind":"Node","name":"worker-2"},"annotations":{"flapping":"false","issues":"KubeletUnhealthy"},"startsAt":"2026-07-25T10:04:11Z"}]`
	if string(got) != want {
		t.Errorf("encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncode_UnknownFormatErrors(t *testing.T) {
	if _, err := encode(Format("teletype"), "", firingNotif); err == nil {
		t.Fatal("encode with an unknown format must error")
	}
}

func TestEncodeJSONCarriesExplanationText(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonExplanation,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		Text:        "The image tag does not exist in the registry.",
	}
	body, err := encode(FormatJSON, "", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Reason string `json:"reason"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Reason != "explanation" {
		t.Errorf("reason = %q, want %q", got.Reason, "explanation")
	}
	if got.Text != n.Text {
		t.Errorf("text = %q, want %q", got.Text, n.Text)
	}
}

func TestEncodeJSONOmitsEmptyText(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	}
	body, err := encode(FormatJSON, "", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), `"text"`) {
		t.Errorf("empty Text must be omitted, got %s", body)
	}
}

func TestEncodeSlackRendersExplanation(t *testing.T) {
	n := alertstate.Notification{
		Object: alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status: alertstate.StatusFiring,
		Reason: alertstate.ReasonExplanation,
		Issues: []string{"ImagePullBackOff"},
		Text:   "The image tag does not exist in the registry.",
	}
	body, err := encode(FormatSlack, "", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(got.Text, "*EXPLANATION* local/Deployment/shop/web") {
		t.Errorf("slack text = %q, want it to start with the EXPLANATION header", got.Text)
	}
	if !strings.Contains(got.Text, n.Text) {
		t.Errorf("slack text = %q, want it to contain the explanation", got.Text)
	}
}

func TestEncodeAlertmanagerAnnotatesExplanation(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonExplanation,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		Text:        "The image tag does not exist in the registry.",
	}
	body, err := encode(FormatAlertmanager, "", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got []struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
	if got[0].Annotations["explanation"] != n.Text {
		t.Errorf("explanation annotation = %q, want %q", got[0].Annotations["explanation"], n.Text)
	}
}

func TestEncodeJSONCarriesTheCluster(t *testing.T) {
	body, err := encode(FormatJSON, "", alertstate.Notification{
		Object:      alertstate.Object{Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"CrashLoopBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["cluster"] != "prod-eu" {
		t.Errorf("cluster = %v, want prod-eu", got["cluster"])
	}
}

func TestEncodeAlertmanagerLabelsTheCluster(t *testing.T) {
	body, err := encode(FormatAlertmanager, "", alertstate.Notification{
		Object:      alertstate.Object{Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"CrashLoopBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got []struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
	if got[0].Labels["cluster"] != "prod-eu" {
		t.Errorf("labels[cluster] = %q, want prod-eu", got[0].Labels["cluster"])
	}
}

// Model output is untrusted text. It must survive encoding as data — never as
// markup that could restructure the payload a receiver parses.
func TestEncodeEscapesHostileModelOutput(t *testing.T) {
	hostile := "\"}]} <script>alert(1)</script>\n*not a header*\x07"
	for _, f := range []Format{FormatJSON, FormatSlack, FormatAlertmanager, FormatPagerDuty} {
		n := alertstate.Notification{
			Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
			Status:      alertstate.StatusFiring,
			Reason:      alertstate.ReasonExplanation,
			Issues:      []string{"ImagePullBackOff"},
			FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
			Text:        hostile,
		}
		body, err := encode(f, "not-a-real-routing-key", n)
		if err != nil {
			t.Fatalf("%s: encode: %v", f, err)
		}
		var any interface{}
		if err := json.Unmarshal(body, &any); err != nil {
			t.Errorf("%s: payload is not valid JSON after hostile text: %v", f, err)
		}
	}
}

// A Kubernetes name may be 253 characters on its own, so the
// cluster/kind/namespace/name concatenation can legally exceed PagerDuty's
// 255-character dedup_key cap. A flat truncation would silently merge two
// distinct objects onto one incident — one outage swallowing another, which is
// exactly the failure the per-object rollup exists to prevent. The digest
// suffix keeps two over-long neighbours apart.
func TestEncodePagerDuty_OverLongDedupKeyStaysDistinct(t *testing.T) {
	long := strings.Repeat("a", 250)
	mk := func(name string) string {
		body, err := encode(FormatPagerDuty, "not-a-real-routing-key", alertstate.Notification{
			Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: name},
			Status:      alertstate.StatusFiring,
			Reason:      alertstate.ReasonNew,
			Issues:      []string{"ImagePullBackOff"},
			FiringSince: at,
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var got struct {
			DedupKey string `json:"dedup_key"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		return got.DedupKey
	}

	a, b := mk(long+"one"), mk(long+"two")
	if len(a) > 255 {
		t.Errorf("dedup_key is %d bytes, want at most 255", len(a))
	}
	if a == b {
		t.Errorf("two distinct objects collapsed onto one dedup_key: %q", a)
	}
	if !strings.HasPrefix(a, "local/Deployment/shop/"+strings.Repeat("a", 20)) {
		t.Errorf("dedup_key lost its readable prefix: %q", a)
	}
	if a != mk(long+"one") {
		t.Errorf("dedup_key is not deterministic")
	}
}

// The same notification must encode byte-identically twice. Map iteration order
// is the classic way this breaks; the PagerDuty payload uses structs precisely
// so it cannot.
func TestEncodePagerDuty_IsDeterministic(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonExplanation,
		Issues:      []string{"ErrImagePull", "ImagePullBackOff"},
		FiringSince: at,
		Flapping:    true,
		Text:        "The image tag does not exist in the registry.",
	}
	first, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("encode is not deterministic:\n%s\n%s", first, again)
		}
	}
}
