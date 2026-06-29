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
		"gh":   "ghp_0123456789abcdefghijklmnopqrstuvwxyz",
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
		"DB_PASSWORD":   "hunter2pass",
		"TOKEN_TTL":     "3600",       // numeric → skipped
		"DEBUG":         "true",       // boolean → skipped
		"API_KEY":       "${API_KEY}", // reference → skipped
		"MAX_CONNS":     "50",         // numeric → skipped
		"TOKEN_VERSION": "1.2",        // credential-named but decimal → skipped
		"SECRET_REF":    "$(SECRET)",  // paren reference → skipped
		"PLAIN_TOKEN":   "$PLAIN",     // bare $ reference → skipped
	})}
	got := Scan(cms, nil)
	if len(got) != 1 || got[0].Location != "DB_PASSWORD" || got[0].Pattern != "credential-like name with a literal value" {
		t.Fatalf("want one DB_PASSWORD finding, got %+v", got)
	}
}

func TestScan_FileRefNamesSkipped(t *testing.T) {
	// A *_FILE env var holds a path to a secret file (the secure convention),
	// not the secret itself, so the credential-name heuristic must not fire.
	// But a real secret VALUE in a *_FILE-named var is still a leak — the value
	// patterns win regardless of name.
	pods := []corev1.Pod{podEnv("default", "app", "c", []corev1.EnvVar{
		{Name: "DB_PASSWORD_FILE", Value: "/etc/secrets/db-password"}, // path → skipped
		{Name: "KC_ADMIN_PASSWORD_FILE", Value: "/run/secrets/admin"}, // path → skipped
		{Name: "AWS_SECRET_FILE", Value: "AKIAIOSFODNN7EXAMPLE"},      // real value → still flagged
	})}
	got := Scan(nil, pods)
	if len(got) != 1 || got[0].Location != "c/AWS_SECRET_FILE" || got[0].Pattern != "AWS access key" {
		t.Fatalf("want only the AWS-key finding at c/AWS_SECRET_FILE, got %+v", got)
	}
}

func TestScan_VersionValuesSkipped(t *testing.T) {
	// A credential-named key whose value is a dotted version (1.2.3) is config,
	// not a secret — the numeric skip must catch multi-part versions, not just
	// integers and single decimals.
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{
		"TOKEN_SCHEMA_VERSION": "1.2.3",   // three-part version → skipped
		"SECRET_FORMAT":        "1.2.3.4", // four-part version → skipped
	})}
	got := Scan(cms, nil)
	if len(got) != 0 {
		t.Fatalf("want no findings for version-valued keys, got %+v", got)
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

func TestScan_InitContainerEnv(t *testing.T) {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "job"},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{
			Name: "setup",
			Env:  []corev1.EnvVar{{Name: "AWS", Value: "AKIAIOSFODNN7EXAMPLE"}},
		}}},
	}
	got := Scan(nil, []corev1.Pod{p})
	if len(got) != 1 || got[0].Location != "setup/AWS" || got[0].Pattern != "AWS access key" {
		t.Fatalf("want one init-container finding at setup/AWS, got %+v", got)
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
