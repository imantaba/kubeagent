package credlint

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cm(ns, name string, data map[string]string) corev1.ConfigMap {
	return corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data}
}

func podEnv(ns, name, container string, env []corev1.EnvVar) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Env: env}}},
	}
}

func TestScan_ConfigMapValuePatterns(t *testing.T) {
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{
		"aws":  "AKIAIOSFODNN7EXAMPLE",
		"pem":  "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----",
		"gh":   "ghp_1234567890abcdefghijklmnopqrstuvwx",
		"jwt":  "eyJhbGciOi.eyJzdWIiOi.sIgnaTURE",
		"note": "just a normal config value",
	})}
	got := Scan(cms, nil)
	want := map[string]string{"aws": "AWS access key", "pem": "private key", "gh": "GitHub token", "jwt": "JWT"}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for _, f := range got {
		if f.Kind != "ConfigMap" || f.Name != "cfg" {
			t.Errorf("unexpected finding object: %+v", f)
		}
		if want[f.Location] != f.Pattern {
			t.Errorf("location %q: pattern = %q, want %q", f.Location, f.Pattern, want[f.Location])
		}
	}
}

func TestScan_CredentialLikeNameWithLiteral(t *testing.T) {
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{
		"DB_PASSWORD": "hunter2pass",
		"TOKEN_TTL":   "3600",       // numeric → skipped
		"DEBUG":       "true",       // boolean → skipped
		"API_KEY":     "${API_KEY}", // reference → skipped
		"MAX_CONNS":   "50",         // numeric → skipped
	})}
	got := Scan(cms, nil)
	if len(got) != 1 || got[0].Location != "DB_PASSWORD" || got[0].Pattern != "credential-like name with a literal value" {
		t.Fatalf("want one DB_PASSWORD finding, got %+v", got)
	}
}

func TestScan_PodEnvLiteralAndValueFromSkipped(t *testing.T) {
	pods := []corev1.Pod{podEnv("default", "web", "app", []corev1.EnvVar{
		{Name: "AWS_SECRET", Value: "AKIAIOSFODNN7EXAMPLE"},                                               // literal → flagged
		{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}}}, // ref → skipped
		{Name: "PORT", Value: "8080"},                                                                     // numeric → skipped
	})}
	got := Scan(nil, pods)
	if len(got) != 1 || got[0].Kind != "Pod" || got[0].Location != "app/AWS_SECRET" || got[0].Pattern != "AWS access key" {
		t.Fatalf("want one pod-env finding at app/AWS_SECRET, got %+v", got)
	}
}

func TestScan_NeverRecordsTheValue(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{"aws": secret, "DB_PASSWORD": "hunter2pass"})}
	got := Scan(cms, nil)
	if len(got) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range got {
		for _, field := range []string{f.Namespace, f.Name, f.Kind, f.Location, f.Pattern} {
			if strings.Contains(field, secret) || strings.Contains(field, "hunter2pass") {
				t.Errorf("a finding field leaked a secret value: %+v", f)
			}
		}
	}
}

func TestScan_SortedAndStable(t *testing.T) {
	cms := []corev1.ConfigMap{
		cm("b", "y", map[string]string{"aws": "AKIAIOSFODNN7EXAMPLE"}),
		cm("a", "z", map[string]string{"aws": "AKIAIOSFODNN7EXAMPLE"}),
	}
	got := Scan(cms, nil)
	if len(got) != 2 || got[0].Namespace != "a" || got[1].Namespace != "b" {
		t.Fatalf("want sorted by namespace, got %+v", got)
	}
}
