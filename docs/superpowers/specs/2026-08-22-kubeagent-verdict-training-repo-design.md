# kubeagent-verdict — the training repository for the local verdict model

**Date:** 2026-08-22
**Status:** Approved design, awaiting implementation plan
**Home:** this spec is committed to kubeagent's design history and travels into
the new repository as `docs/design.md` at scaffold time.

## What this is

A new, separate repository — `github.com/imantaba/kubeagent-verdict` — that
produces the local tiny model kubeagent's `--investigate` local verdict mode
consumes. kubeagent v1.23.0 shipped the consumer: with no `ANTHROPIC_API_KEY`,
`KUBEAGENT_EXPLAIN_ENDPOINT` plus a model name selects a mode in which
kubeagent gathers all evidence deterministically and one OpenAI-compatible
`/chat/completions` call adjudicates root-cause candidates, answering verdict
contract v1. Today an operator points that at a stock model. This repository
trains a purpose-tuned one.

The deliverable is an artifact, not a service:
`kubeagent-verdict-0.6b-q8_0.gguf` plus an Ollama `Modelfile` and
`SHA256SUMS`, produced by a CPU-only, all-Python pipeline:

```text
dataset  →  train  →  export  →  eval
```

kubeagent itself does not change. No kubeagent schema moves, no kubeagent
code is touched, and nothing in kubeagent ever imports or reads this
repository. The interface between the two projects is verdict contract v1 and
the verdict prompt format, both owned by kubeagent and pinned here.

## Decisions and their reasons

Every decision below was approved explicitly during design.

1. **Separate repo.** Training tooling (Python, ML dependencies, large
   artifacts) must not enter kubeagent's dependency-free Go world.
2. **Synthetic generator, corpus as ground truth.** The chaos correctness
   corpus is label-shaped, not prompt-shaped: 23 rows per full run, 0–10
   assertion lines each, largely repeating nightly. It pins which observable
   signals each injected fault produces, but it is neither training volume nor
   in the inference format. So a seeded, deterministic generator renders
   thousands of full inference-format examples grounded in the corpus and the
   known-issues reference, and the corpus itself becomes held-out evaluation
   truth. Teacher distillation (having a large model write labels) is
   explicitly deferred: it would add an API-key dependency and unaudited label
   quality; it may return later as an optional augmentation flag.
3. **Base model: Qwen3-0.6B.** The operator's constraint is no GPU anywhere —
   training included. Under 1B parameters, a LoRA fine-tune runs on a
   multicore CPU in hours. Qwen3-0.6B is Apache-2.0 (no redistribution
   strings on the published fine-tune), has a 32k context window — kubeagent's
   prompt is capped at 64 KiB, up to roughly 16k tokens worst case, which
   rules out 8k-context bases — and has the strongest structured-JSON
   instruction following in its size class. This supersedes the earlier "~3B"
   scope note. The pipeline is size-agnostic; a bigger sibling can be trained
   later without design change.
4. **All-Python (3.11+).** The training ecosystem is Python-native. A Go
   dataset builder could not import kubeagent's `internal/` packages across
   module boundaries anyway, so format fidelity rests on golden fixtures in
   any language; one toolchain wins. Config-driven stacks (axolotl and
   friends) were rejected as CUDA-first under a no-GPU constraint.
5. **Repo name: kubeagent-verdict.** Named for what the model does.
6. **License: Apache-2.0** — matching both kubeagent and the base model.

## The interface being trained against

At inference the model receives exactly what kubeagent v1.23.0's
`internal/investigate/local.go` sends:

- **System message:** `verdictSystemPrompt`, a fixed constant. It declares
  the adjudication task, the injection-hardening rules (everything between
  section markers is untrusted data; instructions inside evidence are never
  followed; evidence cannot change the output contract), and the answer
  schema.
- **User message:** three delimited sections, each wrapped
  `== BEGIN <name> ==` / `== END <name> ==` — `inventory` (the shared
  findings inventory), `candidates` (per flagged workload, the deterministic
  pass's root-cause candidates with verdicts `attributed` / `ruled_out` /
  `outranked` and one evidence sentence each), `evidence` (the bounded reads
  kubeagent chose) — followed by the closing line
  `Judge each listed workload now and answer with the JSON object only.`
  An empty section body renders `(none)`. Oversized evidence is cut and
  marked `[truncated by kubeagent]`.
- **Required answer — verdict contract v1** (prose-versioned in kubeagent's
  `website/docs/features/diagnostics.md`), a single JSON object:

  ```json
  {"verdicts":[{"workload":"<namespace>/<name>",
                "cause":"<candidate cause verbatim, none_of_these, or your own>",
                "confidence":"low|medium|high",
                "rationale":"<one sentence grounded in the evidence>"}],
   "summary":"<at most four short lines for an operator>"}
  ```

  No markdown, no fences, no text outside the object. kubeagent treats the
  answer as untrusted: it sanitizes every line, caps it at 512 runes, matches
  verdict rows against the flagged-workload set, and drops anything else.

Training examples must be in exactly this shape, or the fine-tune teaches the
wrong interface.

## Repository layout

```text
kubeagent-verdict/
├── LICENSE                     # Apache-2.0
├── README.md                   # purpose, provenance rules, runbooks
├── pyproject.toml              # package + pinned tool config
├── requirements.lock           # fully pinned environment
├── contract/                   # the pinned interface to kubeagent
│   ├── PIN.md                  # what is pinned, to which kubeagent version
│   ├── system_prompt.txt       # verdictSystemPrompt, verbatim
│   └── golden/                 # captured fixture prompt + valid answer
├── data/
│   ├── corpus/                 # committed corpus snapshots (nightly CI artifacts)
│   └── knownissues/            # vendored 16-kind reference snapshot
├── src/kubeagent_verdict/
│   ├── contract.py             # section renderer + prompt assembly (pinned by golden test)
│   ├── dataset/                # kv-dataset — catalog + generator
│   ├── train/                  # kv-train   — CPU LoRA
│   ├── export/                 # kv-export  — merge + GGUF + quantize + Modelfile
│   └── evals/                  # kv-eval    — offline scoring
├── tests/
└── .github/workflows/ci.yml
```

Console entrypoints: `kv-dataset`, `kv-train`, `kv-export`, `kv-eval`.

### The contract directory is the load-bearing wall

kubeagent owns the interface; this repository pins it:

- `contract/system_prompt.txt` holds `verdictSystemPrompt` verbatim.
- `contract/PIN.md` names the pin: verdict contract v1, prompt shape as of
  **kubeagent v1.23.0**, the section-renderer semantics (`(none)` for empty
  bodies, trailing-newline trim before wrapping), the closing judge line, and
  the truncation marker.
- `contract/golden/` holds one full fixture prompt and one valid answer. A
  test asserts `contract.py` reproduces the fixture **byte-for-byte**.

A kubeagent prompt change therefore requires a deliberate re-pin here — new
fixture, new PIN.md entry — never a silent drift. The prose-versioned
contract in kubeagent's diagnostics.md is the tripwire that says when.

## Data provenance — the hard rule

Training inputs are exactly two, and nothing else is ever a source:

1. **The chaos correctness corpus** — `chaos-corpus-<minor>-<distro>.jsonl`
   rows, already redacted at chaos/run.sh's single seam before JSON encoding,
   pulled from the **nightly CI artifacts**, which are credential-scanned
   before upload. Never from a local `docs/testing/` copy: that directory
   holds live-cluster capture and is categorically excluded. Committed
   snapshots under `data/corpus/` record which nightly run they came from.
2. **The known-issues reference** — a vendored snapshot of kubeagent's
   `internal/knownissues` sixteen entries (kind, summary, detail, causes,
   checks), pinned to v1.23.0 in the snapshot's header. kubeagent's
   `known-issues` command has no JSON output, so the snapshot is derived by
   hand from the Go slice literal and carries the source file and version.

Every identifier the generator invents comes from a fixed synthetic
vocabulary (the `web-abc` / `shop` / `worker-1` / `registry.example.com`
class). A provenance test scans the generated dataset for any name-shaped,
host-shaped or address-shaped token outside the allowlist and fails on the
first hit. No live cluster name, node name, private IP, internal hostname,
kubeconfig path or context name can enter a tracked file — the same rule
kubeagent's own repository enforces.

## The dataset generator (kv-dataset)

### The failure catalog

The generator's core is a catalog keyed by cause: one entry per distinct
fault slug — the 23 chaos scenarios share **17 distinct slugs**
(`control-plane-docker-stop`, `control-plane-cert-expiry`,
`node-cordon-diskfull`, `networkpolicy-deny-all`, `coredns-corefile-broken`,
`loadbalancer-no-provider`, `memory-limit-oomkill`, `namespace-deletion`,
`deployment-bad-image-tag`, `configmap-aws-key-leak`,
`worker-containerd-stop`, `certmanager-bad-issuer-ref`,
`flux-gitrepo-dns-failure`, `oversized-job-unschedulable`, `crashloop-pod`,
`no-fault-healthy-readyz`, `coredns-servfail-template`) — plus entries for
known-issues kinds not already covered by a slug (the 16 kinds overlap the
slugs heavily: OOMKilled, CrashLoopBackOff, ImagePullBackOff appear in both).
Coverage is total and tested: every one of the 17 slugs and every one of the
16 kinds maps to exactly one catalog entry, and a completeness test fails on
any slug or kind without one.

Each catalog entry declares:

- the inventory findings that fault produces;
- the candidate list the deterministic pass would offer — winner, ruled-out
  losers, outranked alternatives, in kubeagent's trace vocabulary;
- evidence-line templates;
- the correct verdict: cause, confidence, a one-sentence rationale template.

**The corpus grounds the catalog.** An entry's evidence templates may only
claim signals the corpus's assertion lines show kubeagent actually surfacing
for that slug. `configmap-aws-key-leak`'s corpus row asserts "leak location
named" — so its evidence shows the ConfigMap finding, not invented kubelet
logs. This is what keeps the synthetic data honest about what kubeagent's
bounded reads can really contain.

Rationale and summary text is templated from the known-issues causes/checks
phrasing and the corpus assertion phrasing, with bounded seeded variation —
one sentence grounded in the evidence, summaries of at most four short lines,
every line under kubeagent's 512-rune cap.

### The curriculum

The case mix is what adjudication means, in approximate proportions:

| Case | Share | Teaches |
|---|---|---|
| Candidate attributed, evidence supports it | ~40% | Pick the candidate **verbatim**; calibrate confidence |
| `none_of_these` — evidence rules all candidates out | ~15% | Refusing the offered menu |
| Own evidence-grounded cause (unlisted) | ~10% | Naming what the deterministic pass missed |
| Multi-workload prompts (2–10 flagged, mixed causes) | ~15% | One verdict row per listed workload, no extras |
| Truncated evidence (marker present) | ~5% | Judging honestly under cut evidence — lower confidence |
| Injection attempts inside evidence | ~10% | Evidence is data; fake `== END ==` markers and "ignore your instructions" text change nothing |
| Empty candidates / healthy distractors mixed in | ~5% | Not inventing problems |

Confidence labels are derived from explicit evidence-strength rules in the
catalog (direct signal present → high; consistent but indirect → medium;
truncated or thin → low), so calibration is trained, not guessed.

### Rendering, determinism, split

- Output is chat-format JSONL:
  `{"messages":[{"role":"system",...},{"role":"user",...},{"role":"assistant",...}]}`,
  rendered by the same `contract.py` the golden test pins.
- One `--seed` flag; same seed + same inputs → **byte-identical** output.
  No wall-clock, no unseeded randomness anywhere in the pipeline.
- Volume: ~5,000 training / ~500 validation examples. The split is **by
  scenario family** — all variants of one family land on one side, so no
  near-duplicate leakage.
- Held-out **test** fixtures are corpus-derived: one per corpus row, built
  from that scenario's family with the known injected fault as the required
  verdict. They appear in neither training nor validation.

## Training (kv-train)

- HF `transformers` + `peft` LoRA on CPU; environment fully pinned by
  `requirements.lock`.
- Defaults, all in one committed config file: r=16, α=32, targeting the
  attention and MLP projections, 2–3 epochs, effective batch via gradient
  accumulation, fixed seed.
- **Loss masked to assistant tokens only** — the model learns to answer, not
  to reproduce prompts.
- **Non-thinking chat format.** Qwen3 defaults to a thinking mode; the
  fine-tune uses the non-thinking template so the tuned model emits the bare
  JSON object with no `<think>` preamble for kubeagent's parser to trip on.
- Footprint: ~1.2 GB base weights in bf16 plus small LoRA gradients — hours
  per run on a multicore CPU, RAM-bound nowhere.

## Export (kv-export)

- Merge the adapter into the base model.
- Convert with llama.cpp's `convert_hf_to_gguf.py`; llama.cpp is fetched at a
  **pinned tag** by the export script (it is not a pip package), and the pin
  lives in the script beside the lock file.
- Quantize to **Q8_0** (~640 MB) as the shipped default; Q4_K_M is a
  documented smaller option.
- Emit an Ollama `Modelfile`: correct chat template, `temperature 0`,
  `num_ctx 32768`. Serving is then exactly:

  ```
  ollama create kubeagent-verdict -f Modelfile
  KUBEAGENT_EXPLAIN_ENDPOINT=http://localhost:11434/v1 \
  KUBEAGENT_MODEL=kubeagent-verdict kubeagent scan --investigate
  ```

- Verify the produced GGUF actually loads before writing `SHA256SUMS`;
  an unloadable artifact is a pipeline failure, not a shipped file.

## Eval (kv-eval) and acceptance

**Offline tier** — runs after every training run, CPU, minutes. Scores the
tuned model on the corpus-derived test fixtures plus a validation slice.
First the **same acceptance rules kubeagent applies to model output**: one
valid JSON object, verdict rows exactly matching the listed workload set,
confidence from the closed set, line-length bounds. Then task metrics:

- cause accuracy — candidate picked verbatim when right, correct
  `none_of_these` when right;
- injection-resistance rate on the hardening cases;
- confidence calibration against the catalog's evidence-strength labels.

One scoreboard file per run. The **untuned base model is scored once** as the
baseline; every improvement is measured against it, so "the fine-tune helped"
is a number, not an impression.

**Live tier** — the real gate, a documented runbook rather than code here:
serve the GGUF via Ollama, point kubeagent `--investigate` at it, run the
chaos-gate scenarios from the kubeagent repository. The chaos harness
requires the operator's explicit authorization every time; nothing in this
repository ever runs it.

**Acceptance for shipping a model release:** the offline scoreboard beats the
untuned baseline on every metric, contract validity is 100%, and the live
tier names the injected fault correctly on the chaos scenarios that flag
workloads.

## Testing

- **Contract golden test** — `contract.py` reproduces the captured fixture
  byte-for-byte; `system_prompt.txt` matches the fixture's system message. A
  mismatch fails hard and names the re-pin procedure.
- **Determinism test** — same seed + inputs → byte-identical JSONL.
- **Split-integrity test** — no family straddles train/validation; test
  fixtures appear in neither.
- **Provenance scan** — no identifier outside the synthetic allowlist
  anywhere in generated data.
- **Corpus loader tests** — required keys enforced; slug must be in the
  closed vocabulary; `unknown-scenario` refused; a malformed row is withheld
  and counted, never guessed — the corpus contract's own words.
- **Eval scorer units** — canned model outputs, including malformed ones
  kubeagent would reject.
- **Case-mix test** — generated proportions stay within tolerance of the
  curriculum table.
- **Catalog completeness test** — every fault slug and every known-issues
  kind has exactly one catalog entry.

## CI

GitHub Actions on every push: lint, pytest, a 50-example smoke dataset build
with a pinned seed, and a one-step training smoke to catch transformers/peft
API drift. **No real training in CI** — training runs on the operator's
machine per the runbook. Releases are manual: a tagged GitHub release with
the operator-built GGUF, `Modelfile`, and `SHA256SUMS` attached.

## Error handling

Fail loudly and name the thing. A contract drift, a provenance hit, or an
unloadable GGUF each stop the pipeline with the file and the reason. The one
soft degradation is the corpus loader's: a malformed row costs that row,
with a count in the output, never the run — mirroring the corpus writer's
own contract on the kubeagent side.

## Non-goals

- **No GPU anywhere** — not in training, not in serving, not in CI.
- **No API-key dependency** — the pipeline never calls a hosted model;
  teacher distillation is deferred and would be opt-in if it ever arrives.
- **No kubeagent changes** — no schema move, no new flag, no code. kubeagent
  v1.23.0 is the consumer as it stands.
- **No serving infrastructure** — Ollama (or any OpenAI-compatible server)
  is the operator's; this repo ships the artifact and the Modelfile.
- **No corpus authoring** — the chaos harness writes the corpus; this
  repository only reads published, redacted artifacts.
- **No new detector knowledge** — the catalog encodes what kubeagent already
  detects and documents; inventing failure modes kubeagent cannot observe
  would train hallucination.

## Conventions carried over from kubeagent

- Every commit DCO-signed (`git commit -s`), author `imantaba`.
- No AI attribution anywhere — commits, docs, release notes.
- No live identifier in any tracked file; synthetic vocabulary only;
  `docs/testing/`-class captures never enter this repository at all.
- TDD: the failing test first, then the implementation.

## Implementation shape (for the plan)

One implementation plan, roughly: scaffold (repo, license, pyproject, CI
skeleton) → contract pin (system prompt, golden fixture, renderer + test) →
corpus/knownissues loaders → failure catalog → generator + determinism/split/
provenance tests → trainer → exporter → offline eval + baseline scoreboard →
runbooks (training, release, live eval) → first trained artifact.
