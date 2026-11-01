---
type: benchmark-result
project: vibe-palace
benchmark: model-adversarial
series: 3
instrument: walcodec
roster: claude-codex-grok
date: 2026-11-01
protocol: isolated-2026-10-30
status: COMPLETE
---

# Model-Adversarial Benchmark — Series 3 (walcodec / extensibility), 2026-11-01

Third series, new instrument: a two-phase versioned WAL record codec. Phase 1 builds v1 (Put/Delete,
framed, crc); phase 2 hands the cell a v2 amendment that adds a nested variable-length `Batch` record +
a format version, and must absorb it without breaking v1. NOT comparable to series 1/2. Isolation, stop
rule, roster, 3 repeats carried over. Tripwires: $5/cell cost ceiling, $10 for grok cells; fresh-token
caps (cache-read excluded) 1M/round, 4M/cell; 1800s/turn. Instrument chair-only under
`/home/user/adv-imp-benchmark/walcodec/`. **The v1 "build it extensibly" hint was deleted** so the
instrument measures *discovered* extensibility, not hint-compliance.

Note: a first launch produced a transient grok hang (0-byte output) from the same context/account
reload that downed the artifacts endpoint; grok was verified healthy (replay: 9 calls, $0.22, findings
written) and the run re-launched. This record is the clean re-run.

## Headline
- **Discovered extensibility is essentially absent.** The reference impl absorbs v2 additively
  (additive_ratio 1.00: new file, zero edits to v1). **No cell reached 0.2; the max was 0.13**, and
  most were ~0.00 — every model pairing extended by editing the v1 files in place. Across all three
  vendors, the imp/adv loop did **not** produce an extensible-by-design codec unprompted.
- **Phase-2 convergence 17/27.** Cross-vendor 13/18, diagonal 4/9 — but the diagonal is dragged by the
  three Claude-self (C→C) cells, which `did_not_finish` on the $5 ceiling (two-phase Claude review is
  expensive); read the diagonal number with that caveat.
- **3/27 broke v1 API stability** when extending to v2 (changed a v1 exported symbol) — all three are
  Codex-implementor cells (X→C r1, X→G r2, X→G r3). A real extensibility defect: the extension wasn't
  backward-compatible. (Three further `api=fail` rows are `loc=0` dnf cells that emitted no v2 code — not
  genuine breaks; see below.)
- Cost **$93.48** (matrix-only; two-phase, grok $10 cells + Claude-self cost trips); fresh tokens
  **16.4M** (consistent cross-vendor accounting — cache re-reads, ~80% of traffic, excluded).

## Per-cell (dedup; p2 = phase-2 code convergence, ratio = additive-change)

| imp→adv | r1 | r2 | r3 |
|---|---|---|---|
| C→C | dnf $5.58 | **dnf** $5.25 | dnf $5.12 |
| C→G | conv $7.61 r0.016 | conv $6.89 | dnf $2.18 |
| C→X | conv $3.08 | conv $2.88 | not-conv $3.84 |
| G→C | conv $6.48 | conv $5.44 r0.13 | conv $6.10 r0.05 |
| G→G | conv $4.62 | conv $3.76 | conv $3.20 |
| G→X | not-conv $1.87 | conv $2.02 | not-conv $1.75 |
| X→C | conv $3.12 **api=fail** | conv $1.61 | not-conv $4.99 |
| X→G | conv $1.47 r0.06 | conv $2.02 **api=fail** | conv $2.46 **api=fail** |
| X→X | conv $0 | not-conv $0 | not-conv $0 |

v1_regression / v2_public / held-out pass on every finished, code-producing cell. The `fail` grades on
the `dnf` cells are artifacts of the cell being killed before emitting v2 code (`loc=0`), not genuine
regressions — with one exception: C→C r2 is a dnf that *did* produce code (`loc=462`, passing every
grade) before tripping the cost ceiling. Of the 6 `api=fail` rows, only **3 are genuine** API breaks (the
Codex-implementor cells above, all `loc>0`); the other 3 (C→C r1, C→C r3, C→G r3) are `loc=0` dnf cells.

## Observations
- **The additive-ratio finding is the point of this series.** With the hint deleted, no finished model
  built the type-registry seam the reference uses; they all hardcoded v1 and edited it for v2. "Design for
  extension" is not a default behavior the adversarial loop surfaces on its own — on this one instrument.
- **C→C (Claude self, two-phase) is expensive** — all three hit the $5 ceiling. Claude reviewing Claude
  across two phases spends more than the budget.
- **Codex implementations broke the v1 API** 3× when extending — terse but not backward-careful.
- **codex diagonal (X→X) $0** (cost-invisible); converged 1/3 at phase 2.

## Caveats
- Additive-ratio is advisory and gameable (file-splitting / comment churn), but the *uniform* near-zero
  across finished cells is a robust directional signal, not a single-cell artifact. The reference's 1.00
  was authored with the v2 amendment already known, so treat it as an upper bound, not a peer baseline.
- Diagonal phase-2 convergence (4/9) is depressed by the three C→C cost-`dnf` cells; those are
  budget trips, not non-convergence.
- One run, 3 repeats — coarse error bar. Cross-vs-diagonal here (cross 13/18 > diagonal 4/9) resembles
  series 1 and is opposite series 2, but the C→C cost dnf confound makes it the weakest of the three
  cross-vs-diagonal reads.
- grok's 256K-window handicap applies as in series 2.
