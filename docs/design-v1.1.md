# kubeagent v1.1 — Design: `--context` and `-n/--namespace`

Two small, independent CLI flags that came directly from running v1 against a
live multi-cluster setup:

- `--context <name>` — select which kubeconfig context to use, instead of being
  stuck with the kubeconfig's current-context.
- `-n, --namespace <ns>` — scope the scan to a single namespace, instead of
  always scanning the whole cluster (noisy on a real cluster).

Both default to today's behavior when omitted, so v1.1 is fully backward
compatible.

## Goals / non-goals

**Goals:** add the two flags; preserve existing behavior when they're absent;
keep the read-only, stdlib-`flag`, sequential constraints from v1.

**Non-goals (YAGNI):** no `--all-namespaces` flag (omitting `-n` already means
all namespaces); no context-listing subcommand; no namespace globbing or
multi-namespace lists.

## Changes by package

### `internal/cluster/client.go`

Signature change: `NewClient(kubeconfigPath string)` → `NewClient(kubeconfigPath, contextName string)`.

Use client-go's idiomatic config override rather than `BuildConfigFromFlags`:

```go
loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
loadingRules.ExplicitPath = path // from the existing resolveKubeconfig()
overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
    loadingRules, overrides).ClientConfig()
```

When `contextName == ""`, the override is empty and the kubeconfig's
current-context is used — identical to v1. The existing `resolveKubeconfig`
(explicit path → `$KUBECONFIG` → `~/.kube/config`) is kept and feeds
`ExplicitPath`, preserving its tested precedence.

**Tests:**
- Existing `resolveKubeconfig` tests and `NewClient("/nonexistent", "")` error
  test stay green.
- New: write a temp kubeconfig fixture containing two named contexts (`alpha`,
  `beta`) with dummy servers. `NewClient(path, "alpha")` succeeds (builds a
  clientset; no network). `NewClient(path, "ghost")` returns an error
  mentioning the missing context.

### `internal/collect/collect.go`

Signature change: `Cluster(ctx, client)` → `Cluster(ctx, client, namespace string)`.

Body changes one line: `client.CoreV1().Pods(namespace).List(...)`. Empty string
lists all namespaces (v1 behavior); a specific namespace lists only that one.

**Tests:**
- Existing all-namespaces test calls `Cluster(ctx, client, "")` and still
  expects both pods.
- New: fake clientset with pods in namespaces `a` and `b`;
  `Cluster(ctx, client, "a")` returns only the `a` pod.

### `main.go`

Add two flags to the `scan` flagset:

```go
contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
var namespace string
fs.StringVar(&namespace, "namespace", "", "namespace to scan (default: all namespaces)")
fs.StringVar(&namespace, "n", "", "namespace to scan (shorthand)")
```

Binding `--namespace` and `-n` to the same variable is stdlib `flag`'s standard
way to provide a shorthand. Wire the values through:

```go
client, err := cluster.NewClient(*kubeconfig, *contextName)
...
facts, err := collect.Cluster(context.Background(), client, namespace)
```

Updated usage string:
`kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json]`.

**Tests:** existing `run()` arg tests (no args, unknown subcommand, bad output
format) stay green — the validation order is unchanged.

## Docs

- `README.md` usage block updated with the two new flags.
- `docs/go-concepts.md` gains one short entry: binding two flag names to one
  variable for a shorthand (`StringVar` called twice), the stdlib `flag` idiom.

## Constraints (unchanged from v1)

- Read-only; stdlib `flag` only; sequential; module `github.com/imantaba/kubeagent`;
  exit codes `0` ran / `1` tool failed.
