// Package credlint flags credentials stored in the clear — in ConfigMap values
// or pod env `value:` literals. It is pure and security-conscious: a Finding
// records only WHERE a credential lives and WHAT pattern matched, never the
// value itself.
package credlint

import (
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Finding is one credential-in-the-clear warning. It deliberately has no value.
type Finding struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`     // "ConfigMap" | "Pod"
	Location  string `json:"location"` // ConfigMap data key, or "container/ENV_NAME"
	Pattern   string `json:"pattern"`  // what matched (never the value)
}

var (
	awsKeyRe  = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	ghTokenRe = regexp.MustCompile(`ghp_[0-9A-Za-z]{36}|github_pat_[0-9A-Za-z_]{22,}`)
	jwtRe     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	credName  = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)`)
	numericRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
)

// classify returns a pattern label and true when (name, value) looks like a
// credential in the clear. It never returns the value.
func classify(name, value string) (string, bool) {
	switch {
	case awsKeyRe.MatchString(value):
		return "AWS access key", true
	case strings.Contains(value, "-----BEGIN ") && strings.Contains(value, "PRIVATE KEY-----"):
		return "private key", true
	case ghTokenRe.MatchString(value):
		return "GitHub token", true
	case jwtRe.MatchString(value):
		return "JWT", true
	case credName.MatchString(name) && !isFileRef(name) && looksLikeLiteralSecret(value):
		return "credential-like name with a literal value", true
	}
	return "", false
}

// isFileRef reports whether name follows the *_FILE convention, where the value
// is a path to a file holding the secret (the secure pattern) rather than the
// secret itself. The value patterns above still fire if a real secret is present.
func isFileRef(name string) bool {
	return strings.HasSuffix(strings.ToUpper(name), "_FILE")
}

// looksLikeLiteralSecret excludes empties, numbers, booleans, and shell/template
// references so the name heuristic doesn't fire on benign config.
func looksLikeLiteralSecret(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || numericRe.MatchString(v) || strings.HasPrefix(v, "$") {
		return false
	}
	switch strings.ToLower(v) {
	case "true", "false":
		return false
	}
	return true
}

// Scan flags credential-shaped values in ConfigMap data and pod (init + regular)
// container env literals. Result is sorted by (Namespace, Name, Location).
func Scan(configMaps []corev1.ConfigMap, pods []corev1.Pod) []Finding {
	var out []Finding
	for _, c := range configMaps {
		for key, value := range c.Data {
			if pat, ok := classify(key, value); ok {
				out = append(out, Finding{Namespace: c.Namespace, Name: c.Name, Kind: "ConfigMap", Location: key, Pattern: pat})
			}
		}
	}
	for _, p := range pods {
		containers := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
		for _, ctr := range containers {
			for _, e := range ctr.Env {
				if e.ValueFrom != nil || e.Value == "" {
					continue // references are the safe pattern; nothing to lint
				}
				if pat, ok := classify(e.Name, e.Value); ok {
					out = append(out, Finding{Namespace: p.Namespace, Name: p.Name, Kind: "Pod", Location: ctr.Name + "/" + e.Name, Pattern: pat})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Location < out[j].Location
	})
	return out
}
