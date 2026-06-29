# kubeagent — Design: connectivity diagnostics (gap-feature C)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-29

## Goal

When the Kubernetes API server is unreachable or broken (chaos gaps #1 etcd
quorum loss, #2 expired certificates, plus network/auth/DNS failures), replace
the raw Go transport error with a clear, actionable diagnosis.

Today a failed scan prints, e.g.:

```text
kubeagent: listing pods: Get "https://rancher.nova…/api/v1/pods": read tcp …: connection reset by peer
```

After this feature it prints an actionable diagnosis plus a `details:` line with
the raw error.

## Decisions (from brainstorming)

- **Classify connect errors only.** No proactive `/readyz` probe (most etcd/cert
  outages make the apiserver itself unreachable, which classification already
  covers; a probe adds a round-trip and deprecated-API handling).
- **Diagnosis + raw cause line.** Print the actionable message and suggested
  checks, then a `details: <raw error>` line so power users keep the exact error.

## Invariants preserved

- **READ-ONLY:** the feature only *classifies* the error from the call that
  already happens — it issues no new requests. Never mutates the cluster.
- **No new Go module dependency.** Uses stdlib `errors`/`net`/`net/url`/
  `crypto/x509`/`syscall` and `k8s.io/apimachinery/pkg/api/errors` (already
  present).
- **Sequential**, stdlib `flag`. Exit code stays `1` on a failed scan.
- No change to a successful scan's output.

## Architecture

```text
main: collect.CollectInventory(...) returns err  (first real API call: pods List)
      → connectivity.Diagnose(err) → (actionable message, ok)
      → if ok: return "<message>\ndetails: <raw>"   (main prints "kubeagent: …", exit 1)
        else:  return err unchanged
```

The first network call in `run` is `collect.CollectInventory`; any
unreachable/broken-API failure surfaces there. Later collect calls (Nodes,
NodeMetrics, Services, EndpointSlices, NetworkPolicies) only run once pods
succeed — i.e. the API is reachable — so only `CollectInventory`'s error needs
classification.

## Component 1 — `internal/connectivity` (pure)

```go
// Diagnose inspects an error from a Kubernetes API call and returns an
// actionable, multi-line diagnosis plus true when it recognizes a
// connectivity / transport / authentication failure. It returns ("", false)
// for anything it does not recognize, so the caller falls back to the raw error.
func Diagnose(err error) (string, bool)
```

Classification, first match wins. Each branch produces a one-line problem
statement (including the server host when extractable) followed by a short
"Check:" suggestion. Detection mechanisms:

1. **TLS / certificate** — `errors.As` to `x509.UnknownAuthorityError`,
   `x509.CertificateInvalidError`, `x509.HostnameError`, or a `"x509:"` /
   `"certificate"` substring. (Expired certs — #2 — surface here.)
   Message: TLS/certificate problem reaching the API server `<host>`; the cluster
   certificates may be expired or the CA/credentials in your kubeconfig are wrong;
   Check: control-plane cert expiry and that your kubeconfig matches the cluster.
2. **Authentication** — `apierrors.IsUnauthorized(err)` → 401, check kubeconfig
   credentials/token.
3. **Authorization** — `apierrors.IsForbidden(err)` → 403, your user lacks RBAC
   permission for this read.
4. **Connection refused** — `errors.Is(err, syscall.ECONNREFUSED)` or a
   `"connection refused"` substring → the API server `<host>` refused the
   connection; the control plane may be down (apiserver/etcd not running).
5. **Connection reset** — `"connection reset by peer"` substring → the API server
   `<host>` reset the connection; the control plane may be unhealthy or restarting.
6. **Timeout** — `errors.As` to a `net.Error` with `Timeout() == true`, or a
   `"i/o timeout"` / `"context deadline exceeded"` / `"Client.Timeout"` substring
   → timed out reaching `<host>`; check network/VPN/firewall and the server URL.
7. **DNS** — `errors.As` to `*net.DNSError`, or a `"no such host"` /
   `"server misbehaving"` substring → cannot resolve `<host>`; check the server
   URL in your kubeconfig.
8. **Unrecognized** — return `("", false)`.

Helper `serverHost(err) string`: `errors.As(err, **url.Error)`; if found, parse
`urlErr.URL` and return its host (`scheme://host`), else `""`. The host is
interpolated into messages only when non-empty (otherwise phrased as "the
Kubernetes API server").

Ordering note: TLS and auth are checked before the generic transport branches so a
cert/auth failure isn't mis-classified as a bare connection error.

## Component 2 — wiring (`main.go`)

Wrap the first API call's error:

```go
inputs, err := collect.CollectInventory(context.Background(), client, namespace)
if err != nil {
	if diag, ok := connectivity.Diagnose(err); ok {
		return fmt.Errorf("%s\ndetails: %w", diag, err)
	}
	return err
}
```

`main` already prints `kubeagent: <err>` to stderr and exits 1. Kubeconfig-load
errors from `cluster.NewClient` keep their existing messages (config errors, not
connectivity) and are out of scope.

## Testing (TDD)

- `connectivity.Diagnose` — table tests with synthetic errors:
  - `&url.Error{Op:"Get", URL:"https://api.example:6443/...", Err: syscall.ECONNREFUSED}`
    → connection-refused message containing the host.
  - an error whose message contains `"connection reset by peer"` → reset message.
  - a `net.Error` whose `Timeout()` is true (a small fake type) → timeout message.
  - `x509.UnknownAuthorityError{}` (wrapped) → TLS message.
  - `apierrors.NewUnauthorized("nope")` → auth (401) message;
    `apierrors.NewForbidden(...)` → authorization (403) message.
  - `&net.DNSError{Err:"no such host", Name:"api.bad"}` → DNS message.
  - `errors.New("totally unrelated")` → `("", false)`.
  - host extraction: a `url.Error` with a known URL yields the host in the message;
    an error without a URL still produces a (host-less) diagnosis.
- `main_test.go` — a hermetic integration test: write a temp kubeconfig whose
  server is `https://127.0.0.1:1` (a port nothing listens on → loopback
  `connection refused`, no external network), run `run([]string{"scan",
  "--kubeconfig", tmpPath})`, and assert the returned error contains the
  connection-refused diagnosis and a `details:` line. Deterministic and
  network-free.

## Out of scope (explicit non-goals)

- A proactive `/readyz` / `componentstatuses` control-plane health probe.
- Re-classifying kubeconfig-loading errors (missing file, unknown context) —
  those already have clear messages.
- Retries, backoff, or any change to a successful scan.
- Distinguishing etcd-specific failures from generic apiserver-unreachable
  (kubeagent cannot see etcd directly when the apiserver is down).
