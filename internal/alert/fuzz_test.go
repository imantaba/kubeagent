package alert

import (
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

// FuzzEncodePagerDuty asserts the three properties the receiver depends on, for
// any object name and any explanation prose: the encoder never panics, it always
// produces valid JSON, and dedup_key never exceeds PagerDuty's cap. The third is
// the one worth fuzzing — a Kubernetes name may be 253 characters on its own, so
// the cluster/kind/namespace/name concatenation reaches the cap with entirely
// legal input.
//
// The count is in characters, not bytes: json.Marshal replaces an invalid UTF-8
// byte with U+FFFD, so the marshalled key can be longer in bytes than what
// dedupKey returned, but never longer in characters.
func FuzzEncodePagerDuty(f *testing.F) {
	f.Add("local", "Deployment", "shop", "web", "ImagePullBackOff", "")
	f.Add("prod-eu", "Node", "", "worker-2", "NodeNotReady", "The kubelet stopped reporting.")
	f.Add("local", "Deployment", "shop", "web", "CrashLoopBackOff", "\"}]} <script>alert(1)</script>\n*not a header*\x07")
	f.Add("", "", "", "", "", "")
	f.Add("\xff\xfe", "Deployment", "shop", "\xc3", "ImagePullBackOff", "\xff")
	f.Add("local", "Deployment", "shop", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ImagePullBackOff", "")

	f.Fuzz(func(t *testing.T, cluster, kind, namespace, name, issue, text string) {
		for _, status := range []alertstate.Status{alertstate.StatusFiring, alertstate.StatusResolved} {
			n := alertstate.Notification{
				Object:      alertstate.Object{Cluster: cluster, Kind: kind, Namespace: namespace, Name: name},
				Status:      status,
				Reason:      alertstate.ReasonNew,
				Issues:      []string{issue},
				FiringSince: time.Date(2026, 8, 5, 10, 4, 11, 0, time.UTC),
				Text:        text,
			}
			body, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
			if err != nil {
				t.Fatalf("encode returned an error: %v", err)
			}
			var got struct {
				DedupKey string `json:"dedup_key"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("encoder produced invalid JSON: %v", err)
			}
			if c := utf8.RuneCountInString(got.DedupKey); c > pdMaxDedupKey {
				t.Errorf("dedup_key is %d characters, want at most %d", c, pdMaxDedupKey)
			}
			// Determinism: the same notification must encode byte-identically.
			again, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
			if err != nil {
				t.Fatalf("encode returned an error on the second call: %v", err)
			}
			if string(again) != string(body) {
				t.Errorf("encode is not deterministic")
			}
		}
	})
}
