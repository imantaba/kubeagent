# Shell completion

`kubeagent completion <shell>` prints a completion script, generated straight
from the command tree — every subcommand, every flag — so it can never drift
from what the binary actually accepts the way a hand-written script would.

```bash
kubeagent completion bash
kubeagent completion zsh
kubeagent completion fish
kubeagent completion powershell
```

It is the one kubeagent command that touches nothing at all: no cluster
client, no kubeconfig, no model call. It works with `KUBECONFIG` unset,
pointed at a nonexistent file, or with no network reachable — the script is
built from the command tree in memory, not from anything kubeagent reads.

## Installing it

### Bash

Requires the `bash-completion` package. Load it for the current session:

```bash
source <(kubeagent completion bash)
```

Or install it for every new session:

```bash
kubeagent completion bash > /etc/bash_completion.d/kubeagent   # Linux
kubeagent completion bash > $(brew --prefix)/etc/bash_completion.d/kubeagent   # macOS
```

### Zsh

If shell completion is not already enabled, turn it on once:

```bash
echo "autoload -U compinit; compinit" >> ~/.zshrc
```

Load it for the current session:

```bash
source <(kubeagent completion zsh)
```

Or install it for every new session:

```bash
kubeagent completion zsh > "${fpath[1]}/_kubeagent"   # Linux
kubeagent completion zsh > $(brew --prefix)/share/zsh/site-functions/_kubeagent   # macOS
```

### Fish

Load it for the current session:

```bash
kubeagent completion fish | source
```

Or install it for every new session:

```bash
kubeagent completion fish > ~/.config/fish/completions/kubeagent.fish
```

### PowerShell

Load it for the current session:

```powershell
kubeagent completion powershell | Out-String | Invoke-Expression
```

To load it for every new session, add that same line to your PowerShell
profile (`$PROFILE`).

Every shell needs a new session (or a re-sourced profile) before the change
takes effect.

## Under krew: a script alone is not enough

If kubeagent is installed as the `kubectl` plugin `kubectl kubeagent` (see
[Install](../install.md#as-a-kubectl-plugin-krew)), the sections above are
necessary but **not sufficient** — read this before assuming `source`-ing the
bash or zsh script is enough.

Cobra's completion generators — the code that produces the scripts above —
read the command's plain name, never the display-name annotation that makes
`kubectl kubeagent` the spelling kubeagent uses in its own usage and error
text. That was verified directly: building the binary under both names and
diffing the two generated scripts shows they are byte-identical, and both
register completion for the bare word `kubeagent`. So sourcing the script
gives you tab completion when you invoke the binary directly as `kubeagent`.
It gives you **nothing** for `kubectl kubeagent <TAB>` — `kubectl` does not
consult a plugin's own completion script at all.

`kubectl` instead looks for a separate executable named
`kubectl_complete-<plugin>` on your `PATH`, and runs *that* to answer a
completion request. This is a different mechanism from the script above, not
another way of installing it — kubeagent does not generate this file for you;
it is a one-line shim you create once, that forwards the request back into
kubeagent's own `__complete` command:

```bash
cat > ~/.krew/bin/kubectl_complete-kubeagent <<'EOF'
#!/usr/bin/env sh
kubectl kubeagent __complete "$@"
EOF
chmod +x ~/.krew/bin/kubectl_complete-kubeagent
```

`~/.krew/bin` is already on `PATH` if you followed krew's own install
instructions, so no further setup is needed once the shim is in place and
executable. With it, `kubectl kubeagent <TAB>` completes; without it, krew
users get no completion at all regardless of whether they also sourced the
bash or zsh script above.
