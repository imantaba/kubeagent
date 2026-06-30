# Credential lint

`scan --lint-secrets` flags credentials stored in the clear in your cluster
configuration.

!!! warning
    Credential lint reports the **location and pattern only — never the
    credential value itself**. These findings are **never sent to `--explain`**,
    even when both flags are used together. The raw value never leaves your
    machine.

## What is scanned

- **ConfigMap values** that match a known credential pattern.
- **Pod `env[].value` literals** — inline environment variable values where a
  `secretKeyRef` should have been used instead.

## Patterns detected

| Pattern | Examples |
|---------|---------|
| AWS access key | `AKIA…` prefixed values |
| Private key | PEM-encoded private keys |
| GitHub token | `ghp_…`, `github_pat_…` prefixed tokens |
| JWT | `eyJ…` prefixed JSON Web Tokens |
| Credential-like key name with a literal value | keys named `password`, `api_key`, `token`, etc. with a non-empty literal |

## Example output

```text
Credential lint findings

  NAMESPACE   RESOURCE                         LOCATION              PATTERN
  staging     ConfigMap/app-config             data.DB_PASSWORD      credential-like key name
  staging     Pod/api-server env               env[AWS_ACCESS_KEY]   AWS access key
```

## Off by default

`--lint-secrets` is opt-in. Without the flag, `kubeagent` does not read
ConfigMaps.

```bash
# opt in to credential scanning
./kubeagent scan --lint-secrets

# combine with namespace scope
./kubeagent scan --lint-secrets -n staging
```

Checks are read-only and honor the scan's `-n` (namespace) scope.
