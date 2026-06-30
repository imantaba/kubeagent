# Connectivity diagnostics

When the API server cannot be reached, `scan` prints an actionable diagnosis
instead of a raw transport error.

## Error categories

| Category | What it means |
|----------|---------------|
| **Connection refused / reset** | The control plane is down or unreachable at the configured address |
| **Timeout** | The request timed out — network path may be blocked |
| **TLS / expired certificate** | Certificate verification failed or the server certificate has expired |
| **Authentication / Authorization (401 / 403)** | The credentials in your kubeconfig are invalid or lack permission |
| **DNS / wrong host** | The API server hostname cannot be resolved |

Each category is followed by a `details:` line with the underlying error for
further investigation.

## Example output

```text
Error: cannot reach API server — connection refused
details: dial tcp 192.0.2.1:6443: connect: connection refused
```

```text
Error: cannot reach API server — TLS certificate problem
details: x509: certificate has expired or is not yet valid
```

!!! note
    This is classification only. `kubeagent` issues no extra network calls when
    diagnosing a connectivity failure. It exits non-zero as usual.
