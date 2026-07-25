package alert

import (
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

var (
	at      = time.Date(2026, 7, 25, 10, 4, 11, 0, time.UTC)
	cleared = time.Date(2026, 7, 25, 10, 8, 23, 0, time.UTC)

	firingNotif = alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonChanged,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: at,
	}
	resolvedNotif = alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
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
			want:   `{"status":"firing","reason":"changed","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"firingSince":"2026-07-25T10:04:11Z","flapping":false}`,
		},
		{
			name:   "json resolved carries an empty issue list, never null",
			format: FormatJSON,
			notif:  resolvedNotif,
			want:   `{"status":"resolved","reason":"resolved","kind":"Deployment","namespace":"shop","name":"web","issues":[],"firingSince":"2026-07-25T10:04:11Z","resolvedAt":"2026-07-25T10:08:23Z","flapping":false}`,
		},
		{
			name:   "slack firing",
			format: FormatSlack,
			notif:  firingNotif,
			want:   `{"text":"*FIRING* Deployment/shop/web\nissues: ImagePullBackOff\nfiring since 2026-07-25T10:04:11Z"}`,
		},
		{
			name:   "slack resolved reports the duration",
			format: FormatSlack,
			notif:  resolvedNotif,
			want:   `{"text":"*RESOLVED* Deployment/shop/web (fired for 4m12s)"}`,
		},
		{
			name:   "alertmanager firing omits endsAt and keeps issues in annotations",
			format: FormatAlertmanager,
			notif:  firingNotif,
			want:   `[{"labels":{"alertname":"KubeagentIssue","kind":"Deployment","name":"web","namespace":"shop"},"annotations":{"flapping":"false","issues":"ImagePullBackOff"},"startsAt":"2026-07-25T10:04:11Z"}]`,
		},
		{
			name:   "alertmanager resolved sets endsAt",
			format: FormatAlertmanager,
			notif:  resolvedNotif,
			want:   `[{"labels":{"alertname":"KubeagentIssue","kind":"Deployment","name":"web","namespace":"shop"},"annotations":{"flapping":"false","issues":""},"startsAt":"2026-07-25T10:04:11Z","endsAt":"2026-07-25T10:08:23Z"}]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encode(tc.format, tc.notif)
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
		Object:      alertstate.Object{Kind: "Node", Name: "worker-2"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"KubeletUnhealthy", "NotReady"},
		FiringSince: at,
		Flapping:    true,
	}
	got, err := encode(FormatSlack, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"text":"*FIRING* Node/worker-2\nissues: KubeletUnhealthy, NotReady\nfiring since 2026-07-25T10:04:11Z (flapping)"}`
	if string(got) != want {
		t.Errorf("encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncode_ClusterScopedAlertmanagerOmitsNamespaceLabel(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Node", Name: "worker-2"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"KubeletUnhealthy"},
		FiringSince: at,
	}
	got, err := encode(FormatAlertmanager, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `[{"labels":{"alertname":"KubeagentIssue","kind":"Node","name":"worker-2"},"annotations":{"flapping":"false","issues":"KubeletUnhealthy"},"startsAt":"2026-07-25T10:04:11Z"}]`
	if string(got) != want {
		t.Errorf("encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncode_UnknownFormatErrors(t *testing.T) {
	if _, err := encode(Format("teletype"), firingNotif); err == nil {
		t.Fatal("encode with an unknown format must error")
	}
}
