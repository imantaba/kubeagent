package connectivity

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// timeoutErr is a net.Error whose Timeout() is true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func urlErr(rawURL string, inner error) *url.Error {
	return &url.Error{Op: "Get", URL: rawURL, Err: inner}
}

func TestDiagnose_ConnectionRefused_WithHost(t *testing.T) {
	err := urlErr("https://api.example:6443/api/v1/pods", syscall.ECONNREFUSED)
	got, ok := Diagnose(err)
	if !ok {
		t.Fatal("expected a recognized diagnosis")
	}
	if !strings.Contains(got, "refused") {
		t.Errorf("expected a connection-refused message, got: %s", got)
	}
	if !strings.Contains(got, "api.example:6443") {
		t.Errorf("expected the host in the message, got: %s", got)
	}
}

func TestDiagnose_ConnectionReset(t *testing.T) {
	err := urlErr("https://h:6443/x", errors.New("read tcp 1.2.3.4:5: connection reset by peer"))
	got, ok := Diagnose(err)
	if !ok || !strings.Contains(got, "reset") {
		t.Fatalf("expected a connection-reset message, got %q ok=%v", got, ok)
	}
}

func TestDiagnose_Timeout(t *testing.T) {
	got, ok := Diagnose(urlErr("https://h/x", timeoutErr{}))
	if !ok || !strings.Contains(strings.ToLower(got), "timed out") {
		t.Fatalf("expected a timeout message, got %q ok=%v", got, ok)
	}
}

func TestDiagnose_TLSCertificate(t *testing.T) {
	err := urlErr("https://h/x", errors.New("x509: certificate has expired or is not yet valid"))
	got, ok := Diagnose(err)
	if !ok || !strings.Contains(strings.ToLower(got), "certificate") {
		t.Fatalf("expected a TLS/certificate message, got %q ok=%v", got, ok)
	}
}

func TestDiagnose_Unauthorized(t *testing.T) {
	got, ok := Diagnose(apierrors.NewUnauthorized("nope"))
	if !ok || !strings.Contains(got, "Authentication") {
		t.Fatalf("expected an auth (401) message, got %q ok=%v", got, ok)
	}
}

func TestDiagnose_Forbidden(t *testing.T) {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("forbidden"))
	got, ok := Diagnose(err)
	if !ok || !strings.Contains(got, "Authorization") {
		t.Fatalf("expected an authz (403) message, got %q ok=%v", got, ok)
	}
}

func TestDiagnose_DNS(t *testing.T) {
	err := urlErr("https://api.bad/x", &net.DNSError{Err: "no such host", Name: "api.bad"})
	got, ok := Diagnose(err)
	if !ok || !strings.Contains(strings.ToLower(got), "resolve") {
		t.Fatalf("expected a DNS message, got %q ok=%v", got, ok)
	}
}

func TestDiagnose_Unrecognized(t *testing.T) {
	if got, ok := Diagnose(errors.New("totally unrelated boom")); ok {
		t.Errorf("expected no diagnosis for an unrelated error, got %q", got)
	}
}

func TestDiagnose_HostlessStillDiagnoses(t *testing.T) {
	// An auth error with no url.Error → still diagnosed, just without a host.
	got, ok := Diagnose(apierrors.NewUnauthorized("nope"))
	if !ok || strings.Contains(got, "://") {
		t.Fatalf("expected a host-less diagnosis, got %q ok=%v", got, ok)
	}
}
