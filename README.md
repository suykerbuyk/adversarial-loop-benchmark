# Cross-Vendor Adversarial Review — a model benchmark

Does pairing a code-writing model with an **adversarial reviewer from a different
vendor** produce better long-lived software? This repository holds the full
experiment that asks that question: three Go instruments, each run as a 3×3 matrix
of **Claude**, **Codex**, and **Grok** playing *implementor × adversary*, three
repeats per cell — **81 isolated runs** in all — with every deliverable, every
reviewer finding, and every grade included.

📊 **[Read the report →](https://claude.ai/artifact/BiY8gLXhNXYqKrx2UNG8Y6)**
 (or open [`report/benchmark-report.html`](report/benchmark-report.html) locally)

🧭 **[Document index →](INDEX.md)** · 📋 **[Full per-cell results →](RESULTS.md)**

---

## The headline

Three instruments, each isolating a different kind of quality. They gave three
different answers — and together they land somewhere more interesting than "review
helps."

| Series | Instrument | Measures | Result | Cross-vendor conv. | Same-vendor conv. |
|---|---|---|---|---|---|
| **1** | config resolver | correctness | 27/27 pass — **saturated** | **15/18 (83%)** | 6/9 (67%) |
| **2** | block cache | concurrency | discriminates; **1 data race shipped** | 11/18 (61%) | **8/9 (89%)** |
| **3** | WAL codec | extensibility | **0/27 built a reusable seam** | **13/18 (72%)** | 4/9 (44%)¹ |

1. **On the easy task, cross-vendor loops *agreed* more often** (converged 83% vs
   67%) — but all 27 cells passed every grade, so this is a process difference, not a
   measured quality gain, and the gap is within noise (Fisher's exact p = 0.37).
2. **On the hard concurrency task, same-vendor converged more** (89% vs 61%) — also
   within noise (p = 0.20), and partly Grok's budget trips. The durable finding here
   is safety, not the rate: **agreement is not safety** — one converged cross-vendor
   loop shipped code with a live data race the reviewer signed off on.
3. **On this extensibility task, no *finished* cell discovered a reusable seam** —
   best additive-change ratio 0.13; most ~0. (The reference's 1.00 was authored with
   the v2 amendment already known, so it is an upper bound, not a peer baseline.)
   Every finished cell met the new requirement by editing existing code, and **3
   cells — all Codex-implementor — broke the v1 API** while extending.

And a measurement finding: computed with *consistent* cross-vendor accounting,
**cache re-reads are ~80% of all token traffic** (Grok's 256K window paging its
working set). Counting *fresh* work, Grok does the most in every series **and** costs
the most — dollars and tokens agree, there is no inversion. Codex reports **$0** (its
API cost isn't captured) but is the *lightest* fresh worker, so "free in dollars"
must not be read as "does the most work."

¹ Series 3's same-vendor figure is confounded: all three Claude-on-Claude cells hit
the per-cell cost ceiling (budget trips, not non-convergence). Excluding them, the
diagonal is 4/6 (67%) vs cross-vendor 72% — indistinguishable (Fisher's exact
p = 1.0). None of the three series' cross-vs-same gaps reach significance.

---

## How it works

Each cell runs one **frozen loop**: a *plan loop* then a *code loop*, each capped at
3 rounds. The implementor drafts; the adversary tags findings
`blocker / major / minor / nit`; the implementor fixes-or-rebuts; repeat until no
`blocker`/`major` is open, or the cap. A **chair** orchestrates and grades but never
breaks a tie inside a cell — an unresolved `blocker`/`major` at the cap is recorded
as "did not converge," never rewritten.

- **Isolation.** Every turn is a fresh headless process from a clean directory with
  all project tooling, plugins, MCP, and parent-session identity stripped. Cells
  communicate only through files. (An early run was discarded when this wasn't
  airtight — see *Transparency* below.)
- **Grading.** Build, vet, public tests, and a **held-out generalization suite the
  cell never sees**; Series 2 adds `-race` + a goroutine-leak gate; Series 3 adds
  v1-regression, v1-API-stability, and an additive-change diff.
- **Tripwires.** A per-cell cost ceiling and a fresh-token cap (counting only new
  work, not cache re-reads).

Models: `claude-opus-5-5`, `gpt-6-luna`, `grok-4.7`. See
[`methodology/driver.sh`](methodology/driver.sh) for the exact orchestration,
including how held-out grading is applied.

### The Grok handicap (disclosed, not hidden)
`grok-4.7`'s context window is **256K**, vs. ~1M for the others, so on review-heavy
cells it pages its working set through many model calls — inflating its cost and
causing some budget-driven non-convergence. Part of the cross-vendor gap in S2/S3
reflects context size, not review skill. We report it as a caveat, not a clean
skill comparison.

---

## Results

Per-cell **code-loop convergence** (converged within the 3-round cap; phase-2 for
Series 3), implementor (rows) × adversary (columns), rendered from
`results/*.jsonl`. Same-vendor (diagonal) cells are **bold**. Full per-cell grades,
rounds, LoC, cost, and additive-change ratios are in **[RESULTS.md](RESULTS.md)**;
the machine rows are in [`results/`](results/).

**Series 1 — config resolver (correctness)** — cross-vendor **15/18** · same-vendor 6/9

| impl ↓ / adv → | Claude | Grok | Codex |
|---|---|---|---|
| **Claude** | **2/3** | 3/3 | 3/3 |
| **Grok** | 3/3 | **3/3** | 2/3 |
| **Codex** | 1/3 | 3/3 | **1/3** |

*All 27 cells passed every grade — correctness saturated — so the only signal is
convergence, where cross-vendor leads.*

**Series 2 — block cache (concurrency)** — cross-vendor 11/18 · same-vendor **8/9**

| impl ↓ / adv → | Claude | Grok | Codex |
|---|---|---|---|
| **Claude** | **3/3** | 3/3 | 0/3 |
| **Grok** | 3/3 | **2/3** | 1/3 |
| **Codex** | 2/3 | 2/3 | **3/3** |

*Same-vendor now leads (within noise, p = 0.20). Codex-as-adversary never converged
on Claude's cache (C→X = 0/3). The durable finding is the one converged cell (C→G
rep2) that shipped a live data race — see its `grade-*` in `runs/` and its full
transcript in `transcripts/`.*

**Series 3 — WAL codec (extensibility)** — cross-vendor 13/18 · same-vendor 4/9¹

| impl ↓ / adv → | Claude | Grok | Codex |
|---|---|---|---|
| **Claude** | **0/3** | 2/3 | 2/3 |
| **Grok** | 3/3 | **3/3** | 1/3 |
| **Codex** | 2/3 | 3/3 | **1/3** |

*Phase-2 convergence is common, but **no finished cell** built a reusable seam (max
additive-change 0.13; the reference's 1.00 was authored with the amendment known —
an upper bound, not a peer). ¹Claude-on-Claude shows 0/3 because all three hit the
per-cell cost ceiling — budget trips, not non-convergence; excluding them the
diagonal is 4/6 (67%), indistinguishable from cross-vendor 72%.*

---

## Repository layout

```
report/            The written report (HTML): exec summary, charts, method, appendix
results/           Authoritative data per series — the result write-ups (.md) + machine rows (.jsonl)
instrument/        The task definitions, PUBLIC parts only (see note below)
runs/              All 81 cells: generated code + reviewer findings + chair grades
transcripts/       Full raw model transcripts for 3 illustrative cells
methodology/       The chair driver (orchestration + grading mechanism)
```

### `runs/<series>/<imp>to<adv>/rep<n>/`
`C`=Claude, `G`=Grok, `X`=Codex; `CtoG` = Claude implemented, Grok reviewed.
- `worktree/` — the delivered Go code (plus the model's own tests)
- `code_findings.md`, `plan/` — the adversary's actual review findings
- `grade-*.{txt,err}` — the chair's grades (build, vet, public, race, leak, api, v1reg, v2, diff)
- `snapshot-v1/`, `snapshot-v2/` *(Series 3 only)* — code after each phase, for the additive-change diff

### What's withheld, and why
To keep the benchmark **honestly re-runnable**, the **held-out generalization
suite** and the **reference/naive solutions** used for grading are *not* published
(their pass/fail outcomes are, in `results/`). The instrument here is enough to
understand and extend each task; the hidden graders are what keep a re-run honest.
Held-out grade stderr was likewise removed from `runs/` — its outcome lives in
`results/*.jsonl`.

---

## Transparency

The first launch of Series 1 was **invalidated and discarded**: non-isolated cells
loaded project tooling that biased the implementor. It was caught by a
session-note guard, the isolation was hardened (bare launch + environment
stripping + a user-slice scope), and the series was re-run clean. That episode is
disclosed here on purpose — the discarded run is *not* in this repository, and the
contamination-control design is in `methodology/driver.sh`.

This is three single runs at three repeats each — directionally clear, **not**
significance-tested. Token counts aren't strictly comparable across vendors
(different tokenizers); read them order-of-magnitude.

---

## Citation

> *Cross-Vendor Adversarial Review: a model benchmark.* J. Suykerbuyk, 2026.
> https://github.com/suykerbuyk/adversarial-loop-benchmark

## License

- **Code** (instrument, generated deliverables, driver): dual-licensed
  **MIT** ([`LICENSE-MIT`](LICENSE-MIT)) **OR Apache-2.0**
  ([`LICENSE-APACHE`](LICENSE-APACHE)), at your option.
- **Report, data, and findings** (`report/`, `results/`, `transcripts/`):
  **CC-BY-4.0** ([`LICENSE-DATA`](LICENSE-DATA)).
