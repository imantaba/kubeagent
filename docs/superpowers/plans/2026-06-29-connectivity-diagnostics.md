# Connectivity Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the API server is unreachable or broken, replace the raw Go transport error with an actionable diagnosis (TLS/cert, auth, connection refused/reset, timeout, DNS) plus a `details:` line.

**Architecture:** A new pure package `internal/connectivity` classifies an API-call error into an actionable message (`Diagnose(err) (string, bool)`). `main.go` runs the first collect call's error through it and returns `"<diagnosis>\ndetails: <raw>"` when recognized. No new requests, no new dependency.

**Tech Stack:** Go 1.26, stdlib `errors`/`net`/`net/url`/`crypto/x509`/`syscall`/`strings`, `k8s.io/apimachinery/pkg/api/errors` (already present).

## Global Constraints

- **READ-ONLY:** only classifies an error from a call that already happens — issues no new requests. No mutation.
- **No new Go module dependency.** stdlib + `k8s.io/apimachinery/pkg/api/errors` (+ `.../runtime/schema` in the test), already in the tree.
- **Sequential**, stdlib `flag`. Exit code stays `1` on a failed scan; successful-scan output unchanged.
- **Classification order:** TLS/cert and auth are checked BEFORE the generic transport branches, so a cert/auth failure isn't mis-labeled as a bare connection error.
- **Host** is interpolated from a `*url.Error` when present; otherwise messages say "the Kubernetes API server" with no host.
- **Unrecognized errors** return `("", false)` — the caller keeps the raw error path.
- **TDD:** failing test first, watch it fail, implement, watch it pass, commit. `export PATH=$PATH:/usr/local/go/bin` before any `go` command. Run `gofmt -l` on touched files; fix with `gofmt -w`.
- **Scope (YAGNI):** no `/readyz` probe; kubeconfig-load errors unchanged.

---

### Task 1: `internal/connectivity` — classify API-call failures (pure)

**Files:**
- Create: `internal/connectivity/connectivity.go`
- Test: `internal/connectivity/connectivity_test.go`

**Interfaces:**
- Produces: `func Diagnose(err error) (string, bool)`

- [ ] **Step 1: Write the failing test**

Create `internal/connectivity/connectivity_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/connectivity/`
Expected: FAIL — package has no non-test files / `undefined: Diagnose`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/connectivity/connectivity.go`:

```go
// Package connectivity turns a failed Kubernetes API call into an actionable,
// human-readable diagnosis. It is pure — it classifies an existing error and
// issues no requests of its own.
package connectivity

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Diagnose inspects an error from a Kubernetes API call and returns an
// actionable, multi-line diagnosis plus true when it recognizes a
// connectivity / transport / authentication failure. It returns ("", false)
// for anything it does not recognize, so the caller falls back to the raw error.
func Diagnose(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	at := "the Kubernetes API server"
	if host := serverHost(err); host != "" {
		at += " " + host
	}
	msg := err.Error()

	// TLS / certificate (checked first so an expired cert isn't read as a bare
	// connection error).
	var unknownAuth x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostnameErr x509.HostnameError
	if errors.As(err, &unknownAuth) || errors.As(err, &certInvalid) || errors.As(err, &hostnameErr) ||
		strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate") {
		return fmt.Sprintf("TLS/certificate problem reaching %s.\n"+
			"The cluster certificates may be expired, or the CA/credentials in your kubeconfig are wrong.\n"+
			"Check: control-plane certificate expiry and that your kubeconfig matches this cluster.", at), true
	}

	// Authentication / authorization.
	if apierrors.IsUnauthorized(err) {
		return fmt.Sprintf("Authentication failed (401) at %s.\n"+
			"Check: the credentials/token in your kubeconfig are valid and not expired.", at), true
	}
	if apierrors.IsForbidden(err) {
		return fmt.Sprintf("Authorization failed (403) at %s.\n"+
			"Check: your user has RBAC permission to read this (kubeagent needs only list/get).", at), true
	}

	// Connection refused.
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(msg, "connection refused") {
		return fmt.Sprintf("%s refused the connection.\n"+
			"The control plane may be down (the API server or etcd is not running).\n"+
			"Check: control-plane pods/processes and the server URL in your kubeconfig.", capitalize(at)), true
	}

	// Connection reset.
	if strings.Contains(msg, "connection reset by peer") {
		return fmt.Sprintf("%s reset the connection.\n"+
			"The control plane may be unhealthy or restarting (e.g. the API server or etcd).\n"+
			"Check: control-plane health and recent restarts.", capitalize(at)), true
	}

	// Timeout.
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) ||
		strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "Client.Timeout") {
		return fmt.Sprintf("Timed out reaching %s.\n"+
			"Check: network/VPN/firewall reachability and the server URL in your kubeconfig.", at), true
	}

	// DNS.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving") {
		return fmt.Sprintf("Cannot resolve %s.\n"+
			"Check: the server URL/hostname in your kubeconfig and your DNS.", at), true
	}

	return "", false
}

// serverHost returns "scheme://host" from a *url.Error in the chain, or "".
func serverHost(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

// capitalize upper-cases the first byte (ASCII) of s.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/connectivity/ -v && go vet ./internal/connectivity/ && gofmt -l internal/connectivity/`
Expected: all tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/connectivity/
git commit -m "feat(connectivity): classify unreachable/broken API-server errors"
```

---

### Task 2: wire `main.go`, integration test, document, CHANGELOG

**Files:**
- Modify: `main.go`
- Test: `main_test.go`
- Modify: `README.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: `connectivity.Diagnose`.

- [ ] **Step 1: Write the failing integration test**

Append to `main_test.go` (add `"os"` and `"path/filepath"` to its imports; `strings` and `testing` are already imported):

```go
func TestRun_DiagnosesUnreachableAPI(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	// A kubeconfig pointing at a port nothing listens on → loopback connection
	// refused (no external network). Exercises the connectivity diagnosis path.
	cfg := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: dead
contexts:
- context:
    cluster: dead
    user: dead
  name: dead
current-context: dead
users:
- name: dead
  user: {}
`
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"scan", "--kubeconfig", kc})
	if err == nil {
		t.Fatal("expected an error for an unreachable API server")
	}
	out := err.Error()
	if !strings.Contains(out, "refused") || !strings.Contains(out, "details:") {
		t.Errorf("expected a connection-refused diagnosis with a details line, got: %v", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_DiagnosesUnreachableAPI`
Expected: FAIL — the returned error is the raw `listing pods: … connection refused` (no friendly diagnosis, no `details:` line) until the wiring is added.

- [ ] **Step 3: Wire main.go**

Add `"github.com/imantaba/kubeagent/internal/connectivity"` to the imports (alphabetically, after the `collect` import). Replace the existing CollectInventory error handling:

```go
	inputs, err := collect.CollectInventory(context.Background(), client, namespace)
	if err != nil {
		return err
	}
```

with:

```go
	inputs, err := collect.CollectInventory(context.Background(), client, namespace)
	if err != nil {
		if diag, ok := connectivity.Diagnose(err); ok {
			return fmt.Errorf("%s\ndetails: %w", diag, err)
		}
		return err
	}
```

(`main.go` already imports `fmt`.)

- [ ] **Step 4: Run the test + full suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_DiagnosesUnreachableAPI -v && go vet ./... && go test ./... && gofmt -l main.go main_test.go && go build -o /tmp/kubeagent .`
Expected: the new test PASSES, all packages `ok`, gofmt prints nothing, build succeeds.

- [ ] **Step 5: Document in README.md**

Add a `### Connectivity diagnostics` subsection in the scan/usage area (before `## Install`):

```markdown
### Connectivity diagnostics

When the API server can't be reached, `scan` prints an actionable diagnosis
instead of a raw transport error — distinguishing a down control plane
(connection refused/reset), a timeout, a TLS/expired-certificate problem,
authentication/authorization (401/403), and DNS/wrong-host — followed by a
`details:` line with the underlying error. This is classification only: kubeagent
issues no extra calls and exits non-zero as before.
```

- [ ] **Step 6: Update CHANGELOG.md**

Under `## [Unreleased]` → `### Added`, add a bullet, and REMOVE the "Connectivity / control-plane diagnostics" bullet from `### Planned` (leave the remaining Planned bullet intact):

```markdown
- **Connectivity diagnostics.** An unreachable or broken API server now yields an
  actionable diagnosis (down / timeout / TLS-cert / auth / DNS) with a `details:`
  line, instead of a raw transport error.
```

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go README.md CHANGELOG.md
git commit -m "feat: diagnose unreachable API server in scan; document + changelog"
```

---

## Self-Review

**Spec coverage:**
- `connectivity.Diagnose` — all categories (TLS/cert, 401, 403, refused, reset, timeout, DNS), order (TLS/auth before transport), host extraction, unrecognized → false → Task 1. ✓
- main wiring (wrap the first collect call's error; `details:` line) → Task 2 Step 3. ✓
- hermetic integration test (loopback refused) → Task 2 Step 1. ✓
- README + CHANGELOG → Task 2 Steps 5–6. ✓

**Placeholder scan:** none — every step has concrete code/commands.

**Type consistency:** `Diagnose(err error) (string, bool)` is produced in Task 1 and consumed in Task 2 with the exact `if diag, ok := connectivity.Diagnose(err); ok` shape. The test asserts the same `refused` / `details:` substrings the wiring produces.
