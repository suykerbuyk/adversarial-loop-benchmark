---
type: benchmark-result
project: vibe-palace
benchmark: model-adversarial
series: 2
instrument: blockcache
roster: claude-codex-grok
date: 2026-10-31
protocol: isolated-2026-10-30
status: COMPLETE
---

# Model-Adversarial Benchmark — Series 2 (blockcache / concurrency), 2026-10-31

Second series, new instrument: a bounded concurrent single-flight block cache (race / resource /
lifecycle surface). NOT comparable to series 1 (config-resolver) or series 3 (walcodec). Isolation,
stop rule, roster, 3 repeats carried over unchanged. Tripwires: $5/cell cost ceiling, **$10/cell for
cells involving grok** (operator ruling — grok-4.7's 256K context window drives iteration-heavy,
pricier reviews); token caps count FRESH tokens only (cache-read excluded), 1M/round, 4M/cell;
per-turn timeout 1800s. Instrument: `/home/user/adv-imp-benchmark/blockcache/` (chair-only).

## Calibration & plumbing
C→G calibration cleared ($2.78, converged). Three calibration iterations were needed to find the right
tripwires — the cap mis-fired on grok's cache-read replays (256K window → 26 model calls, 948K
cache-read, fresh work ~100K) until it was changed to count fresh tokens only; then grok reviews hit
the $5 cost ceiling, raised to $10 for grok cells. Contamination guard held (MATRIX DONE, not halted).

## Headline
- **Correctness did NOT saturate** (unlike series 1): convergence varies, and one cell shipped a real
  data race. The harder instrument discriminates.
- **Diagonal (same-vendor) convergence 8/9 (89%) > cross-vendor 11/18 (61%)** — the opposite direction
  to series 1 (cross 15/18 > diagonal 6/9), though **within noise** (Fisher's exact p = 0.20; the
  cross-series interaction p = 0.26). On hard concurrency, a same-vendor reviewer converged more;
  cross-vendor loops (esp. codex-as-adversary, and grok's costly reviews) stayed open or ran out of
  budget.
- **Total measurable cost $74.12** (claude+grok; codex $0). grok-involved cells dominate ($4–10.69 each).

## Per-cell (dedup: C→G r1 rerun under $10 supersedes its $5.66 dnf)

| imp→adv | r1 | r2 | r3 |
|---|---|---|---|
| C→C | conv $1.22 | conv $2.18 | conv $2.98 |
| C→G | conv $4.48 | **conv but race=FAIL** $4.54 | conv $7.19 |
| C→X | not-conv $1.70 | not-conv $1.97 | not-conv $1.08 |
| G→C | conv $3.02 | conv $1.28 | conv $2.46 |
| G→G | **dnf $10.69** | conv $4.29 | conv $7.51 |
| G→X | conv $0.97 | not-conv $1.89 | not-conv $1.57 |
| X→C | conv $0.94 | not-conv $0.99 | conv $1.38 |
| X→G | conv $1.80 | not-conv $4.38 | conv $3.50 |
| X→X | conv $0 | conv $0 | conv $0 |

All cells pass build/vet/public/held-out(gen)/race/API EXCEPT **C→G r2 race=fail** (a converged loop
that shipped a data race — the grok adversary missed it) and **G→G r1 did_not_finish** (cost > $10,
both-grok, genuinely non-converging). objective_quality = partial (stress advisory, recorded-fail
noted but not a gate).

## Observations
- **Codex-as-adversary did not converge** on claude's cache (C→X 0/3) and was mixed elsewhere (G→X
  1/3) — codex kept the review loop open on concurrency.
- **grok cells are expensive and slow** by construction (256K window → heavy iteration): $4–10.69,
  ~1–1.5h each; one still exceeded $10.
- **codex diagonal (X→X) converged 3/3 at $0** (cost-invisible) — terse, fast, self-agreeing.
- A converged loop is NOT a correctness guarantee: C→G r2 converged yet shipped a race.

## Caveats
- One run, 3 repeats — coarse error bar. The cross-vs-diagonal reversal is suggestive, not
  significance-tested, and is entangled with grok's cost-ceiling dnf and codex-adv non-convergence.
- grok's 256K window is a genuine handicap vs claude/codex (1M / larger) on a review-heavy task — the
  convergence/cost gap partly reflects context size, not review skill. Report this explicitly.
- Raw per-cell cost is not cross-vendor comparable (codex $0).
