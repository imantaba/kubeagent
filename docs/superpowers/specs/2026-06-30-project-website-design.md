# kubeagent — Design: project website (GitHub Pages + custom domain)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-30

## Goal

Publish a multi-page static website for kubeagent — a landing page plus
documentation pages — built with MkDocs + the Material theme, deployed to GitHub
Pages via GitHub Actions, and served at the custom domain **k8sproject.top**
(apex canonical, `www` redirecting to apex) with HTTPS.

## Decisions (from brainstorming)

- **Scope:** landing page + docs pages (multi-page).
- **Tooling:** MkDocs + Material theme. Source in a dedicated `website/`
  directory with **curated** content adapted from the README + CHANGELOG — not
  the internal `docs/` planning material.
- **Deploy:** a new GitHub Actions workflow builds and publishes to Pages
  (Pages source = GitHub Actions). The existing `ci.yml` / `release.yml` are
  untouched.
- **Domain:** apex `k8sproject.top` is canonical; `www` redirects to apex via
  GitHub Pages' built-in www↔apex 301. Cloudflare DNS configured via the user's
  scoped API token (env var `CF_P_API_TOKEN`).

## Invariants / constraints

- **No secrets committed.** The Cloudflare API token is read only from the
  environment (`$CF_P_API_TOKEN`) or a local untracked file at the DNS step; its
  value is never printed, logged, or committed. Rotate/revoke after use.
- **No real cluster data on the site.** The landing-page sample scan output is
  **synthetic** — no real IPs, namespaces, or node names.
- **Separation:** site source lives under `website/`; the Go module, `ci.yml`,
  and `release.yml` are not modified. MkDocs build output is git-ignored.
- The site is documentation only — it does not change kubeagent's behavior.

## Architecture

```text
website/ (MkDocs source, curated markdown)
   └─ mkdocs build --strict ─► static HTML
        └─ GitHub Actions (pages.yml) ─► upload-pages-artifact ─► deploy-pages
             └─ GitHub Pages (source = Actions)
                  └─ custom domain k8sproject.top (CNAME + Pages setting)
                       └─ Cloudflare DNS: apex A/AAAA + www CNAME (DNS-only)
                            └─ GitHub auto-redirect www → apex, Enforce HTTPS
```

## Component 1 — site source (`website/`)

```text
website/
  mkdocs.yml
  requirements.txt              # pinned mkdocs-material (exact 9.x chosen in the plan)
  docs/
    index.md                    # landing: hero, tagline, install/quickstart CTAs,
                                #   feature grid, synthetic sample scan output
    quickstart.md               # build, scan, core flags, --explain (+ privacy note)
    features/
      diagnostics.md            # CrashLoopBackOff / ImagePullBackOff / OOMKilled / Pending
      resource-context.md
      platform-facts.md
      service-health.md
      networkpolicy.md
      connectivity.md
      credential-lint.md
    install.md                  # release download + checksum verify
    roadmap.md                  # v1/v2 + shipped features; links to Releases + CHANGELOG
    assets/
      logo.svg                  # simple SVG wordmark
      favicon.svg               # small favicon
      extra.css                 # minor accent/hero styling
    CNAME                       # contents: k8sproject.top
```

Content is adapted from the existing README sections (one feature page each) and
written in Material-flavored markdown (admonitions, code blocks with copy,
grids/cards on the landing page). MkDocs copies `docs/CNAME` and `docs/assets/`
verbatim to the site root/output.

## Component 2 — MkDocs config (`website/mkdocs.yml`)

Key settings:

- `site_name: kubeagent`
- `site_url: https://k8sproject.top`
- `repo_url: https://github.com/imantaba/kubeagent`, `repo_name: imantaba/kubeagent`
- `theme: name: material` with:
  - `palette` light/dark toggle
  - `features`: `navigation.sections`, `navigation.top`, `content.code.copy`,
    `search.suggest`, `navigation.footer`
  - `logo: assets/logo.svg`, `favicon: assets/favicon.svg`
  - `extra_css: [assets/extra.css]`
- `markdown_extensions`: `admonition`, `pymdownx.highlight`,
  `pymdownx.superfences`, `pymdownx.tabbed` (alternate style), `toc` with
  permalinks, `attr_list`, `md_in_html` (for the landing feature grid)
- `nav`: Home → Quickstart → Features (the 7 pages) → Install → Roadmap
- `plugins`: built-in `search` (no social-cards plugin — it needs system cairo;
  out of scope)

`requirements.txt` pins `mkdocs-material` (a specific 9.x) so CI builds are
reproducible.

## Component 3 — deployment (`.github/workflows/pages.yml`)

```yaml
name: Pages
on:
  push:
    branches: [main]
    paths: ['website/**', '.github/workflows/pages.yml']
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
        with: { python-version: '3.x' }
      - run: pip install -r website/requirements.txt
      - run: mkdocs build --strict -f website/mkdocs.yml -d ../_site
        working-directory: website
      - uses: actions/upload-pages-artifact@v3
        with: { path: _site }
      - id: deploy
        uses: actions/deploy-pages@v4
```

(`mkdocs build --strict` fails the build on broken internal links / nav, so a
bad page is caught in CI rather than shipped.) Build output path is git-ignored.

Pages source is set to **GitHub Actions** (`gh api -X POST repos/imantaba/kubeagent/pages`
with `build_type=workflow`, or the repo Settings → Pages UI).

## Component 4 — custom domain

- `website/docs/CNAME` contains `k8sproject.top` (copied to the published site
  root so the domain binding survives each deploy).
- GitHub Pages custom domain set to `k8sproject.top` and **Enforce HTTPS**
  enabled (via `gh api` once DNS resolves and the cert is provisioned).
- **GitHub provisions** a Let's Encrypt cert for both apex and `www`, and serves
  an automatic **301 `www` → apex**, so no Cloudflare redirect rule is needed.
- **Cloudflare DNS** (created via `$CF_P_API_TOKEN` at the DNS step):
  - `A  @`   → `185.199.108.153`, `185.199.109.153`, `185.199.110.153`, `185.199.111.153`
  - `AAAA @` → `2606:50c0:8000::153`, `2606:50c0:8001::153`, `2606:50c0:8002::153`, `2606:50c0:8003::153`
  - `CNAME www` → `imantaba.github.io`
  - All records **DNS-only (grey cloud)**; SSL/TLS mode **Full**. (Cloudflare-
    proxied Pages commonly breaks GitHub's cert provisioning; DNS-only is the
    reliable initial setup. Enabling the orange proxy + SSL Full later is an
    optional, documented follow-up.)

### DNS step prerequisite

The Cloudflare API calls run with network egress and need the token in the
environment the tool actually uses. `CF_P_API_TOKEN` set in `~/.bashrc` is **not
visible** to the non-interactive/login shells the tool runs (confirmed during
brainstorming). At the DNS step the user will make the token available to the
tool — e.g. write it to a local untracked file the step reads, or provide it for
that step — and revoke it afterward. The token value is never printed or
committed.

## Component 5 — repo hygiene

- `.gitignore`: ignore the MkDocs build output (`_site/` and the default
  `website/site/`).
- No change to the Go module, `ci.yml`, or `release.yml`.
- README gains a short line linking to the site (https://k8sproject.top).

## Testing / validation

- **CI build:** `mkdocs build --strict` must pass (broken links/nav fail it).
- **Local preview:** `mkdocs serve` renders the site; visually check nav, the
  landing page, dark/light toggle, and code-copy.
- **Deploy:** the `pages.yml` Actions run succeeds and the `*.github.io` URL
  serves the site before the domain is attached.
- **Domain:** after DNS, `dig k8sproject.top` returns the GitHub Pages IPs;
  GitHub's custom-domain check passes; `https://k8sproject.top` loads with a
  valid cert; `https://www.k8sproject.top` 301-redirects to apex; Enforce HTTPS
  is on.

## Out of scope (explicit non-goals)

- Rendering the internal `docs/` planning material (specs/plans) on the site.
- A blog, versioned docs, analytics, or search beyond Material's built-in.
- Social-card image generation (needs system cairo).
- Cloudflare proxy/CDN tuning beyond the DNS-only initial setup (optional later).
- Any change to kubeagent's Go code or release process.
