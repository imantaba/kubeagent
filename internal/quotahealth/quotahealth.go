// Package quotahealth flags ResourceQuota entries whose used/hard ratio is at or
// over a threshold — a namespace near or past a quota limit, before it silently
// blocks object creation. Pure and read-only: the caller supplies the quotas and
// the threshold. Mirrors pdbhealth/hpahealth.
package quotahealth

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// Issue is one ResourceQuota entry at or over the usage threshold.
type Issue struct {
	Namespace string  `json:"namespace"`
	Quota     string  `json:"quota"`    // ResourceQuota object name
	Resource  string  `json:"resource"` // e.g. "pods", "requests.cpu"
	Used      string  `json:"used"`     // Quantity.String()
	Hard      string  `json:"hard"`     // Quantity.String()
	Ratio     float64 `json:"ratio"`
	Severity  string  `json:"severity"` // "exhausted" | "near"
}

// Assess flags each ResourceQuota status.hard entry whose used/hard ratio is
// >= threshold. Entries with hard <= 0 are skipped (a deliberate zero quota, and
// it avoids a divide-by-zero). Output is sorted exhausted-first, then by ratio
// descending, then by (Namespace, Quota, Resource).
func Assess(quotas []corev1.ResourceQuota, threshold float64) []Issue {
	issues := []Issue{}
	for _, q := range quotas {
		for name, hard := range q.Status.Hard {
			hf := hard.AsApproximateFloat64()
			if hf <= 0 {
				continue
			}
			used := q.Status.Used[name]
			ratio := used.AsApproximateFloat64() / hf
			if ratio < threshold {
				continue
			}
			sev := "near"
			if ratio >= 1.0 {
				sev = "exhausted"
			}
			issues = append(issues, Issue{
				Namespace: q.Namespace,
				Quota:     q.Name,
				Resource:  string(name),
				Used:      used.String(),
				Hard:      hard.String(),
				Ratio:     ratio,
				Severity:  sev,
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if ra, rb := sevRank(a.Severity), sevRank(b.Severity); ra != rb {
			return ra < rb
		}
		if a.Ratio != b.Ratio {
			return a.Ratio > b.Ratio
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Quota != b.Quota {
			return a.Quota < b.Quota
		}
		return a.Resource < b.Resource
	})
	return issues
}

func sevRank(s string) int {
	if s == "exhausted" {
		return 0
	}
	return 1
}
