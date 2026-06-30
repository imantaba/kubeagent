# Project Website Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish a multi-page MkDocs + Material website for kubeagent to GitHub Pages via GitHub Actions, served at `k8sproject.top` (apex canonical, `www`→apex) over HTTPS.

**Architecture:** Site source lives in a dedicated `website/` directory (curated markdown adapted from the README, not the internal `docs/`). A GitHub Actions workflow runs `mkdocs build --strict` and deploys to Pages. The custom domain is bound via a `CNAME` file + the Pages API, with Cloudflare DNS (apex A/AAAA + `www` CNAME, DNS-only) created through the user's scoped API token.

**Tech Stack:** MkDocs, mkdocs-material (Python), GitHub Actions (`actions/deploy-pages`), GitHub Pages, Cloudflare DNS API.

## Global Constraints

- **No secrets committed.** The Cloudflare token is read only from `$CF_P_API_TOKEN` (or a file outside the repo / git-ignored) at the DNS step; its value is never printed, logged, or committed. The user revokes it after.
- **No real cluster data on the site.** Any sample scan output is **synthetic** — no real IPs, namespaces, or node names.
- **Separation:** all site source under `website/`. Do **not** modify the Go module, `.github/workflows/ci.yml`, or `.github/workflows/release.yml`. MkDocs build output is git-ignored.
- **Site URL:** `https://k8sproject.top`. **Repo:** `imantaba/kubeagent`. **GitHub Pages host:** `imantaba.github.io`.
- **Strict builds:** the site must build with `mkdocs build --strict` (broken nav/links fail the build) at the end of every buildable task.
- Local builds use a throwaway virtualenv so the repo and system Python stay clean.

---

### Task 1: MkDocs scaffold (config, deps, assets, stubs, build green)

**Files:**
- Create: `website/mkdocs.yml`
- Create: `website/requirements.txt`
- Create: `website/docs/index.md` (stub), `website/docs/quickstart.md` (stub), `website/docs/install.md` (stub), `website/docs/roadmap.md` (stub)
- Create: `website/docs/features/diagnostics.md`, `resource-context.md`, `platform-facts.md`, `service-health.md`, `networkpolicy.md`, `connectivity.md`, `credential-lint.md` (stubs)
- Create: `website/docs/assets/logo.svg`, `website/docs/assets/favicon.svg`, `website/docs/assets/extra.css`
- Create: `website/docs/CNAME`
- Modify: `.gitignore`

**Interfaces:**
- Produces: a complete site skeleton that builds clean with `mkdocs build --strict`; later tasks fill page bodies. The nav in `mkdocs.yml` and the set of `docs/*.md` files must match exactly (strict mode errors on either a nav entry with no file or a docs file absent from nav).

- [ ] **Step 1: Create `website/requirements.txt` with an exact pin**

Create a throwaway venv, install the latest mkdocs-material, and pin the exact installed version:

```bash
python3 -m venv /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
VENV=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
"$VENV/bin/pip" install --quiet --upgrade pip
"$VENV/bin/pip" install --quiet mkdocs-material
VER=$("$VENV/bin/pip" show mkdocs-material | awk '/^Version:/{print $2}')
printf 'mkdocs-material==%s\n' "$VER" > website/requirements.txt
cat website/requirements.txt
```

Expected: `website/requirements.txt` contains `mkdocs-material==<the installed 9.x version>`.

- [ ] **Step 2: Create `website/mkdocs.yml`**

```yaml
site_name: kubeagent
site_description: Read-only Kubernetes troubleshooting, explained.
site_url: https://k8sproject.top
repo_url: https://github.com/imantaba/kubeagent
repo_name: imantaba/kubeagent
copyright: kubeagent — read-only Kubernetes troubleshooting

theme:
  name: material
  logo: assets/logo.svg
  favicon: assets/favicon.svg
  features:
    - navigation.sections
    - navigation.top
    - navigation.footer
    - content.code.copy
    - search.suggest
  palette:
    - media: "(prefers-color-scheme: light)"
      scheme: default
      primary: indigo
      accent: indigo
      toggle:
        icon: material/weather-night
        name: Switch to dark mode
    - media: "(prefers-color-scheme: dark)"
      scheme: slate
      primary: indigo
      accent: indigo
      toggle:
        icon: material/weather-sunny
        name: Switch to light mode

extra_css:
  - assets/extra.css

markdown_extensions:
  - admonition
  - attr_list
  - md_in_html
  - toc:
      permalink: true
  - pymdownx.highlight:
      anchor_linenums: true
  - pymdownx.superfences
  - pymdownx.tabbed:
      alternate_style: true
  - pymdownx.emoji:
      emoji_index: !!python/name:material.extensions.emoji.twemoji
      emoji_generator: !!python/name:material.extensions.emoji.to_svg

nav:
  - Home: index.md
  - Quickstart: quickstart.md
  - Features:
      - Failure diagnostics: features/diagnostics.md
      - Resource context: features/resource-context.md
      - Platform facts: features/platform-facts.md
      - Service health: features/service-health.md
      - NetworkPolicy hints: features/networkpolicy.md
      - Connectivity diagnostics: features/connectivity.md
      - Credential lint: features/credential-lint.md
  - Install: install.md
  - Roadmap: roadmap.md
```

- [ ] **Step 3: Create the CNAME and asset files**

`website/docs/CNAME` (exactly one line, no trailing content):

```text
k8sproject.top
```

`website/docs/assets/logo.svg` (simple magnifier-over-hex wordmark mark, white so it shows on the colored header):

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="none">
  <path d="M16 3l11 6v14l-11 6-11-6V9l11-6z" stroke="#fff" stroke-width="1.6" fill="none" opacity="0.9"/>
  <circle cx="14.5" cy="14.5" r="5" stroke="#fff" stroke-width="2" fill="none"/>
  <line x1="18.2" y1="18.2" x2="23" y2="23" stroke="#fff" stroke-width="2.2" stroke-linecap="round"/>
</svg>
```

`website/docs/assets/favicon.svg` (same mark on an indigo tile):

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
  <rect width="32" height="32" rx="6" fill="#3f51b5"/>
  <circle cx="14.5" cy="14.5" r="5" stroke="#fff" stroke-width="2" fill="none"/>
  <line x1="18.2" y1="18.2" x2="23" y2="23" stroke="#fff" stroke-width="2.2" stroke-linecap="round"/>
</svg>
```

`website/docs/assets/extra.css` (minor hero/centering polish for the landing page):

```css
/* Center the landing hero buttons row */
.md-typeset .hero-cta { display: flex; flex-wrap: wrap; gap: .6rem; margin: 1.2rem 0 2rem; }
/* Tighten the feature card icons */
.md-typeset .grid.cards :is(.twemoji, svg) { height: 1.4rem; vertical-align: -0.2rem; }
```

- [ ] **Step 4: Create stub pages so strict build passes**

Each stub is a heading + one sentence (filled in later tasks). Create:

`website/docs/index.md`:

```markdown
# kubeagent

Read-only Kubernetes troubleshooting, explained. (Landing content added in Task 2.)
```

`website/docs/quickstart.md`:

```markdown
# Quickstart

How to build and run kubeagent. (Content added in Task 3.)
```

`website/docs/install.md`:

```markdown
# Install

Download a prebuilt release and verify it. (Content added in Task 3.)
```

`website/docs/roadmap.md`:

```markdown
# Roadmap

What's shipped and what's next. (Content added in Task 3.)
```

`website/docs/features/diagnostics.md`:

```markdown
# Failure diagnostics

CrashLoopBackOff, ImagePullBackOff, OOMKilled, Pending/Unschedulable. (Content added in Task 4.)
```

`website/docs/features/resource-context.md`:

```markdown
# Resource context

Cluster CPU/memory summary and per-OOMKill limits. (Content added in Task 4.)
```

`website/docs/features/platform-facts.md`:

```markdown
# Platform facts

The detected stack: CNI, ingress, storage, distro, runtime, cloud. (Content added in Task 4.)
```

`website/docs/features/service-health.md`:

```markdown
# Service health

Services with no ready endpoints; backing-aware annotations. (Content added in Task 4.)
```

`website/docs/features/networkpolicy.md`:

```markdown
# NetworkPolicy hints

Which policies select a stuck pod. (Content added in Task 4.)
```

`website/docs/features/connectivity.md`:

```markdown
# Connectivity diagnostics

Actionable API-server-unreachable messages. (Content added in Task 4.)
```

`website/docs/features/credential-lint.md`:

```markdown
# Credential lint

Opt-in scan for credentials stored in the clear. (Content added in Task 4.)
```

- [ ] **Step 5: Update `.gitignore`**

Append to `.gitignore`:

```text
# MkDocs site build output + local build venv + DNS token file
/_site/
website/site/
website/.venv/
*.cf_token
```

- [ ] **Step 6: Build strict to verify the skeleton**

```bash
VENV=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
"$VENV/bin/mkdocs" build --strict -f website/mkdocs.yml -d /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-out
```

Expected: `INFO - Documentation built in ... seconds` with **no WARNING/ERROR** lines (strict mode fails on any). If `pymdownx.emoji`'s `!!python/name:` lines cause a build error, that means the mkdocs-material version differs — keep the lines (they are the documented Material syntax) and re-run; do not remove icons.

- [ ] **Step 7: Commit**

```bash
git add website/ .gitignore
git commit -m "feat(site): scaffold MkDocs Material site (config, assets, stubs)"
```

---

### Task 2: Landing page (`index.md`)

**Files:**
- Modify: `website/docs/index.md`

**Interfaces:**
- Consumes: the Material theme + extensions configured in Task 1 (`grid cards`, `.md-button`, `pymdownx.emoji` icons).

- [ ] **Step 1: Write the landing page**

Replace `website/docs/index.md` entirely with (note: the sample scan output is **synthetic** — keep it that way):

````markdown
---
hide:
  - navigation
  - toc
---

# kubeagent

**Read-only Kubernetes troubleshooting, explained.**

kubeagent scans a cluster, finds unhealthy pods, and explains *why* they're
failing — talking to the cluster through the official Kubernetes Go client
(`client-go`), strictly read-only.

<div class="hero-cta" markdown>
[Get started](quickstart.md){ .md-button .md-button--primary }
[Install](install.md){ .md-button }
[GitHub](https://github.com/imantaba/kubeagent){ .md-button }
</div>

## What it catches

<div class="grid cards" markdown>

- :material-restart: __CrashLoopBackOff__ — containers stuck restarting
- :material-image-broken-variant: __ImagePullBackOff__ — bad image or registry auth
- :material-memory: __OOMKilled__ — hit the memory limit (shown with the container's requests/limits)
- :material-timer-sand: __Pending / Unschedulable__ — no node can place the pod

</div>

## Beyond pods

<div class="grid cards" markdown>

- :material-server-network: __Service health__ — Services with no ready endpoints and LoadBalancers with no address, backing-aware
- :material-shield-lock-outline: __NetworkPolicy hints__ — which policies select a stuck pod
- :material-lan-disconnect: __Connectivity diagnostics__ — actionable "API server unreachable" messages
- :material-key-alert-outline: __Credential lint__ — opt-in scan for secrets stored in the clear
- :material-chart-box-outline: __Resource context__ — cluster CPU/memory plus per-OOMKill limits
- :material-layers-outline: __Platform facts__ — CNI, ingress, storage, distro, runtime, cloud

</div>

## See it

```text
$ kubeagent scan
Cluster: Healthy — 3/3 nodes Ready
Platform: Cilium CNI · Traefik ingress · Kubernetes v1.30 · containerd

Resources (cluster):
  CPU     24.0 cores · req 6.2 (25%) · lim 18.0 (75%) · used 3.1 (12%)
  Memory  96Gi · req 22Gi (22%) · lim 70Gi (72%) · used 18Gi (18%)

⚠ shop/checkout  Deployment  1/3 Degraded  · 12 restarts, last 2m ago
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
Service issues:
  ⚠ shop/checkout  ClusterIP  no ready endpoints
```

Optional `--explain` makes a single Claude API call to summarize findings in
plain English — the deterministic core still works fully offline.

---

Open source on [GitHub](https://github.com/imantaba/kubeagent) ·
[Releases](https://github.com/imantaba/kubeagent/releases)
````

- [ ] **Step 2: Build strict**

```bash
VENV=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
"$VENV/bin/mkdocs" build --strict -f website/mkdocs.yml -d /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-out
```

Expected: builds with no warnings/errors. (Visual check optional: `mkdocs serve -f website/mkdocs.yml` and open the local URL.)

- [ ] **Step 3: Commit**

```bash
git add website/docs/index.md
git commit -m "feat(site): landing page"
```

---

### Task 3: Top-level pages (quickstart, install, roadmap)

**Files:**
- Modify: `website/docs/quickstart.md`, `website/docs/install.md`, `website/docs/roadmap.md`

**Interfaces:**
- Consumes: source material in the repo `README.md` (read it; adapt the relevant sections). Do not invent flags — mirror the README exactly.

- [ ] **Step 1: Write `quickstart.md`**

Read `README.md` lines 27–67 (Usage + flags + the `--explain` privacy note) and `README.md` lines 1–25 (intro + Status). Write `website/docs/quickstart.md` covering, in this order, with fenced `bash` code blocks copied faithfully from the README:

- a one-line intro (what `scan` does: prioritized problem report, P1 cluster health then P2 workloads; healthy/restart-only/CronJob hidden by default)
- build (`go build -o kubeagent .`) and the basic `./kubeagent scan`
- the flag examples: `--include-restarts`, `--include-cron`, `--context`/`-n`/`--output json`, `--kubeconfig`, `--explain` (+ `ANTHROPIC_API_KEY`), `--model`, and `--lint-secrets` (cross-link to the Credential lint feature page)
- a `!!! note` admonition reproducing the README privacy note: `--explain` sends only a structured summary (verdict, node counts, notable workloads' ns/name/kind/ready-desired/status/restarts/issue), never raw specs/IPs/env/secrets; and the model precedence (`--model` → `KUBEAGENT_MODEL` → `claude-opus-4-8`).

Use relative links for cross-page links (e.g. `[Credential lint](features/credential-lint.md)`).

- [ ] **Step 2: Write `install.md`**

Read `README.md` lines 135–169 (Install + Cutting a release). Write `website/docs/install.md`:

- the prebuilt linux/amd64 download + checksum-verify block (the `VERSION=...`, `curl`, `sha256sum -c`, `tar`, `./kubeagent version` sequence), copied faithfully
- a short "Build from source" note (`go build -o kubeagent .`)
- a `!!! tip` linking to the [Releases page](https://github.com/imantaba/kubeagent/releases)

Do **not** include the maintainer "Cutting a release" workflow details (that's internal; out of scope for the public site).

- [ ] **Step 3: Write `roadmap.md`**

Read `README.md` lines 17–25 (Status) and 171–178 (Roadmap). Write `website/docs/roadmap.md`:

- **Shipped:** v1 (deterministic scan + the four detectors), v2 (`--explain`), and the post-v2 features (resource context, platform facts, service health + backing awareness, NetworkPolicy hints, connectivity diagnostics, credential lint) — each one line, linking to its feature page where one exists.
- a `!!! info` admonition linking to the [GitHub Releases](https://github.com/imantaba/kubeagent/releases) and the changelog (`https://github.com/imantaba/kubeagent/blob/main/CHANGELOG.md`) as the source of truth for versions.

- [ ] **Step 4: Build strict**

```bash
VENV=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
"$VENV/bin/mkdocs" build --strict -f website/mkdocs.yml -d /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-out
```

Expected: no warnings/errors (strict catches any broken relative link).

- [ ] **Step 5: Commit**

```bash
git add website/docs/quickstart.md website/docs/install.md website/docs/roadmap.md
git commit -m "feat(site): quickstart, install, roadmap pages"
```

---

### Task 4: Feature pages (`features/*.md`)

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `resource-context.md`, `platform-facts.md`, `service-health.md`, `networkpolicy.md`, `connectivity.md`, `credential-lint.md`

**Interfaces:**
- Consumes: the corresponding `README.md` sections. Each feature page adapts one section faithfully (no new claims). Use `!!! note`/`!!! warning` admonitions where the README has a caveat, and fenced code for examples. Any example output must be **synthetic**.

- [ ] **Step 1: Write each feature page from its README section**

Read `README.md` and adapt each section into its page (keep the wording faithful; expand only with synthetic examples and admonitions):

- `features/diagnostics.md` ← README lines 3–25 (the four failure modes + read-only intro + Status). List CrashLoopBackOff / ImagePullBackOff-ErrImagePull / OOMKilled / Pending-Unschedulable, each with one line on what it means.
- `features/resource-context.md` ← README lines 69–77.
- `features/platform-facts.md` ← README lines 79–91 (include the example `Platform:` line, but replace it with a **synthetic** stack, e.g. `Cilium CNI · Traefik ingress · Kubernetes v1.30 · containerd`).
- `features/service-health.md` ← README lines 93–104 (Service health, including the backing-awareness paragraph). Add a synthetic example showing both a primary `no ready endpoints` and an annotated `(backs CronJob — expected between runs)`.
- `features/networkpolicy.md` ← README lines 106–114 (include the kindnet caveat as a `!!! note`).
- `features/connectivity.md` ← README lines 116–123.
- `features/credential-lint.md` ← README lines 125–133. Add a `!!! warning` emphasizing it reports **location and pattern only, never the value**, and is **never sent to `--explain`**; off by default.

- [ ] **Step 2: Build strict**

```bash
VENV=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
"$VENV/bin/mkdocs" build --strict -f website/mkdocs.yml -d /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-out
```

Expected: no warnings/errors.

- [ ] **Step 3: Commit**

```bash
git add website/docs/features/
git commit -m "feat(site): feature documentation pages"
```

---

### Task 5: Deploy workflow + README link

**Files:**
- Create: `.github/workflows/pages.yml`
- Modify: `README.md` (add a website link near the top)

**Interfaces:**
- Consumes: `website/mkdocs.yml`, `website/requirements.txt` from Task 1.
- Produces: a Pages deploy on every push to `main` that touches `website/**`.

- [ ] **Step 1: Create `.github/workflows/pages.yml`**

```yaml
name: Pages
on:
  push:
    branches: [main]
    paths:
      - 'website/**'
      - '.github/workflows/pages.yml'
  workflow_dispatch:

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: pages
  cancel-in-progress: true

jobs:
  build-deploy:
    runs-on: ubuntu-latest
    environment:
      name: github-pages
      url: ${{ steps.deploy.outputs.page_url }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: '3.x'
      - run: pip install -r website/requirements.txt
      - run: mkdocs build --strict -f website/mkdocs.yml -d ../_site
        working-directory: website
      - uses: actions/upload-pages-artifact@v3
        with:
          path: _site
      - id: deploy
        uses: actions/deploy-pages@v4
```

- [ ] **Step 2: Validate the workflow YAML parses**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/pages.yml')); print('pages.yml: valid YAML')"
```

Expected: `pages.yml: valid YAML`.

- [ ] **Step 3: Confirm the CI build command matches locally**

Run the exact command the workflow runs (output to scratchpad, not the repo) to prove parity:

```bash
VENV=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/site-venv
( cd website && "$VENV/bin/mkdocs" build --strict -f mkdocs.yml -d /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/ci-parity )
```

Expected: builds clean (no warnings/errors).

- [ ] **Step 4: Add a website link to `README.md`**

Under the H1 intro (after `README.md` line 3, the "A Kubernetes troubleshooting agent, written in Go." line), insert:

```markdown

📖 **Docs & site:** [k8sproject.top](https://k8sproject.top)
```

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/pages.yml README.md
git commit -m "ci(site): GitHub Pages deploy workflow; link site from README"
```

---

### Task 6: Go live — enable Pages, custom domain, Cloudflare DNS (post-merge, operational)

> This task performs **live, external** actions and is run after the branch is merged to `main` (so the deploy workflow exists on the default branch). It needs network egress and the Cloudflare token. It has no unit tests; each step has an explicit verification command. The controller (not a sandboxed subagent) runs this, because it needs `$CF_P_API_TOKEN` and `gh`.

**Prerequisite — make the token available (never commit/print it):**
`CF_P_API_TOKEN` in `~/.bashrc` is not visible to the non-interactive shell. Before this task, the user provides the token to the shell for this step only — e.g. writes it to a file OUTSIDE the repo and the steps read it:

```bash
# user places the token here (outside the repo); value never printed
TOKENFILE=/tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/cf_token
export CF_API_TOKEN="$(cat "$TOKENFILE")"
test -n "$CF_API_TOKEN" && echo "token loaded (${#CF_API_TOKEN} chars)" || echo "NO TOKEN"
```

- [ ] **Step 1: Enable GitHub Pages with the Actions build type**

```bash
gh api -X POST repos/imantaba/kubeagent/pages -f build_type=workflow \
  || gh api -X PUT repos/imantaba/kubeagent/pages -f build_type=workflow
gh api repos/imantaba/kubeagent/pages --jq '{status:.status, build_type:.build_type, html_url:.html_url}'
```

Expected: a JSON object with `"build_type":"workflow"`. (If the deploy workflow has already run after merge, `status` will be `built`.)

- [ ] **Step 2: Confirm the site is live on the github.io URL (pre-domain)**

```bash
curl -sSI https://imantaba.github.io/ -o /dev/null -w '%{http_code}\n' || true
gh run list --workflow=pages.yml -L 1
```

Expected: the latest `pages.yml` run is `completed/success`. (The github.io URL may 404 only if the custom domain is already bound — that's fine; Step 5 verifies the domain.)

- [ ] **Step 3: Create Cloudflare DNS records (apex A/AAAA + www CNAME, DNS-only)**

```bash
ZONE=$(curl -s -H "Authorization: Bearer $CF_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones?name=k8sproject.top" | python3 -c "import sys,json;print(json.load(sys.stdin)['result'][0]['id'])")
echo "zone id length: ${#ZONE}"

add() { # type name content
  curl -s -X POST -H "Authorization: Bearer $CF_API_TOKEN" -H "Content-Type: application/json" \
    "https://api.cloudflare.com/client/v4/zones/$ZONE/dns_records" \
    --data "{\"type\":\"$1\",\"name\":\"$2\",\"content\":\"$3\",\"proxied\":false,\"ttl\":1}" \
    | python3 -c "import sys,json;d=json.load(sys.stdin);print('ok' if d.get('success') else d.get('errors'))"
}
for ip in 185.199.108.153 185.199.109.153 185.199.110.153 185.199.111.153; do add A k8sproject.top "$ip"; done
for ip in 2606:50c0:8000::153 2606:50c0:8001::153 2606:50c0:8002::153 2606:50c0:8003::153; do add AAAA k8sproject.top "$ip"; done
add CNAME www imantaba.github.io
```

Expected: each call prints `ok`. (If a record already exists, Cloudflare returns an error like "record already exists" — that is acceptable; do not duplicate.)

- [ ] **Step 4: Set SSL mode to Full and bind the custom domain on GitHub**

```bash
curl -s -X PATCH -H "Authorization: Bearer $CF_API_TOKEN" -H "Content-Type: application/json" \
  "https://api.cloudflare.com/client/v4/zones/$ZONE/settings/ssl" --data '{"value":"full"}' \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print('ssl:',d.get('result',{}).get('value') or d.get('errors'))"

gh api -X PUT repos/imantaba/kubeagent/pages -f cname=k8sproject.top
```

Expected: `ssl: full`; the Pages API call succeeds. (The `CNAME` file from Task 1 also binds the domain on each deploy.)

- [ ] **Step 5: Verify DNS, HTTPS, and the www→apex redirect**

DNS can take a few minutes to propagate; re-run until green.

```bash
dig +short k8sproject.top
curl -sSI https://k8sproject.top/ -o /dev/null -w 'apex: %{http_code}\n'
curl -sSI https://www.k8sproject.top/ -o /dev/null -w 'www:  %{http_code} -> %{redirect_url}\n'
```

Expected: `dig` returns the four `185.199.*` IPs; `apex: 200`; `www:` a 301 redirecting to `https://k8sproject.top/`.

- [ ] **Step 6: Enforce HTTPS once the certificate is provisioned**

GitHub provisions the Let's Encrypt cert after the domain resolves (can take a few minutes). Once `gh api repos/imantaba/kubeagent/pages --jq .https_certificate.state` reads `approved`:

```bash
gh api -X PUT repos/imantaba/kubeagent/pages -f cname=k8sproject.top -F https_enforced=true
gh api repos/imantaba/kubeagent/pages --jq '{cname:.cname, https_enforced:.https_enforced, cert:.https_certificate.state}'
```

Expected: `{"cname":"k8sproject.top","https_enforced":true,"cert":"approved"}`.

- [ ] **Step 7: Reminder — revoke the token**

Tell the user to **revoke/rotate** the Cloudflare API token now that DNS is configured, and remove the scratchpad token file.

---

## Self-Review

**Spec coverage:**
- Site source under `website/`, curated, MkDocs Material → Task 1 (scaffold) + Tasks 2–4 (content). ✓
- Landing page + docs pages → Task 2 (landing), Task 3 (top-level), Task 4 (features). ✓
- Synthetic sample output only → Task 2 + Task 4 (explicit). ✓
- GitHub Actions deploy, Pages source = Actions, strict build → Task 5 (workflow) + Task 6 Step 1. ✓
- Custom domain apex + www→apex, CNAME file, Pages cname, Enforce HTTPS → Task 1 (CNAME) + Task 6 Steps 4–6. ✓
- Cloudflare apex A/AAAA + www CNAME, DNS-only, SSL Full → Task 6 Steps 3–4. ✓
- Token never committed/printed; revoke after → Global Constraints + Task 6 prerequisite + Step 7; `.gitignore` `*.cf_token` (Task 1). ✓
- ci.yml/release.yml/Go module untouched; build output git-ignored → Global Constraints + Task 1 `.gitignore`. ✓
- README link to site → Task 5 Step 4. ✓

**Placeholder scan:** the only intentional stubs are Task 1's page bodies, which Tasks 2–4 explicitly replace; the requirements.txt version is resolved by `pip show` at build time (a real value, not a literal placeholder). No "TBD"/"handle edge cases" steps remain.

**Type/name consistency:** `website/` paths, `mkdocs.yml` nav entries, and the `docs/*.md` file set match exactly across Tasks 1–4 (strict build enforces this). The Pages host (`imantaba.github.io`), site URL (`https://k8sproject.top`), the four A IPs / four AAAA IPs, and `$CF_API_TOKEN` (loaded from the token file) are used identically across Task 6. The workflow's `-d ../_site` (run from `website/`) outputs to repo-root `_site`, which `upload-pages-artifact path: _site` consumes and `.gitignore` ignores.
