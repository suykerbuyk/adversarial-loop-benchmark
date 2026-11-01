---
type: benchmark-result
project: vibe-palace
benchmark: model-adversarial
roster: claude-codex-grok
date: 2026-10-30
protocol: isolated-2026-10-30
status: VALID
supersedes: 2026-10-30-claude-codex-grok.md (INVALID-contaminated-launch)
---

# Model-Adversarial Benchmark — 2026-10-30 (roster: claude / codex / grok) — VALID run

First complete 27-cell run under the isolated (bare) protocol. Replaces the contaminated
same-day run recorded in `2026-10-30-claude-codex-grok.md` (which stays as a labeled specimen).
Method: `/vps-model-benchmark`, frozen instrument = layered config resolver. Chair records and
grades; it never adjudicates inside a cell.

## Why this run is valid (the fix)

The earlier same-day run was invalid because `claude -p` loaded the vibe-palace plugin skill
catalog and fired the `vp hook` Stop/SessionEnd capture. This run ran every cell **bare and
isolated**, verified live:

- claude: `--bare --disable-slash-commands --no-session-persistence --strict-mcp-config --mcp-config '{"mcpServers":{}}'`
- grok: isolated `GROK_HOME` (compat.claude/cursor skills·rules·agents·mcps·hooks·sessions = false), `--disable-web-search --no-subagents --verbatim`
- codex: `--ignore-user-config --ignore-rules --ephemeral -c mcp_servers={}`, brief on stdin
- `cell_env()` strips `HERDR_*`/`CLAUDE_CODE_*`/`CLAUDE_VAULT` so no cell inherits the chair session
- driver ran in a `systemd-run --user` scope (`user.slice`), outside the Teleport cgroup
- **Contamination guard held: vault session-note count 7 → 7 across the whole run (zero captures).**

## Frozen parameters

- Models (shipped default flagship, pinned, version-recorded; operator ruling 2026-10-30):
  claude `claude-opus-5-5`, grok `grok-4.7`, codex `gpt-6-luna` (reasoning high).
- Tripwire: **$5/cell cost ceiling (primary)**; token caps 500000/round, 2000000/cell as runaway
  backstops (raised from 200k/1M after the calibration showed grok's agentic review legitimately
  exceeds 200k while costing ~$0.25). A trip = `did_not_finish` (recorded, never rewritten).
- Cost = Σ(imp+adv) `total_cost_usd` per cell. **Codex emits no cost field → $0 in cost; codex is
  token-only.** So any cell with a codex turn undercounts cost (noted per cell).
- Calibration (separate, not a matrix replicate): C→G bare — converged, all grades pass,
  dnf=false, $1.81.

## Per-cell summary (3 repeats each; imp → adv)

All 9 cells × 3 reps = 27: **build / vet / public / generalization(held-out) / race / API = pass for
every cell; did_not_finish = 0 for every cell.** Quality (correctness) is therefore saturated — the
discriminators are convergence, cost, and code size.

| cell | plan_conv | code_conv | mean cost* | mean tokens | mean LOC |
|------|-----------|-----------|-----------|-------------|----------|
| C→C (claude self) | 3/3 | 2/3 | $1.36 | 427,204 | 177 |
| C→G | 3/3 | 3/3 | $0.96 | 510,716 | 164 |
| C→X | 2/3 | 3/3 | $0.68 (claude only) | 491,775 | 152 |
| G→C | 3/3 | 3/3 | $1.01 | 508,925 | 160 |
| G→G (grok self) | 3/3 | 3/3 | $0.74 | 742,298 | 159 |
| G→X | 3/3 | 2/3 | $0.37 (grok only) | 683,611 | 133 |
| X→C | 3/3 | 1/3 | $0.88 (claude only) | 897,428 | 108 |
| X→G | 3/3 | 3/3 | $0.32 (grok only) | 617,280 | 96 |
| X→X (codex self) | 3/3 | 1/3 | $0 (codex, uncaptured) | 719,927 | 66 |

*cost is claude+grok only; codex turns contribute $0. Matrix measurable cost (claude+grok) = **$19.05**;
+ calibration $1.81 = **$20.86**. Codex spend across 6 codex-imp + 6 codex-adv cells is uncaptured.

## Full per-(cell, repeat) data

Self-contained copy (the host-local `results.jsonl` is scratch). plan = rounds/converged,
code = rounds/converged; all gate columns = pass for every row.

| imp→adv | rep | plan | code | LOC | tok_imp | tok_adv | cost$ |
|---|---|---|---|---|---|---|---|
| C→C | 1 | 1/✓ | 1/✓ | 144 | 96568 | 153731 | 0.857 |
| C→C | 2 | 1/✓ | 1/✓ | 161 | 120365 | 173412 | 1.004 |
| C→C | 3 | 2/✓ | 3/✗ | 226 | 386441 | 351097 | 2.241 |
| C→G | 1 | 1/✓ | 1/✓ | 164 | 87464 | 316028 | 0.732 |
| C→G | 2 | 1/✓ | 1/✓ | 153 | 100016 | 263224 | 0.855 |
| C→G | 3 | 2/✓ | 1/✓ | 175 | 198296 | 567122 | 1.293 |
| C→X | 1 | 3/✗ | 1/✓ | 152 | 278601 | 327141 | 0.904 |
| C→X | 2 | 1/✓ | 2/✓ | 142 | 164236 | 319539 | 0.552 |
| C→X | 3 | 2/✓ | 1/✓ | 164 | 159238 | 226570 | 0.592 |
| G→C | 1 | 1/✓ | 1/✓ | 103 | 151331 | 110165 | 0.574 |
| G→C | 2 | 2/✓ | 1/✓ | 167 | 271668 | 190009 | 0.984 |
| G→C | 3 | 1/✓ | 2/✓ | 212 | 489110 | 314493 | 1.485 |
| G→G | 1 | 1/✓ | 1/✓ | 154 | 162764 | 320861 | 0.489 |
| G→G | 2 | 1/✓ | 1/✓ | 160 | 217992 | 391286 | 0.582 |
| G→G | 3 | 3/✓ | 1/✓ | 164 | 359739 | 774253 | 1.179 |
| G→X | 1 | 1/✓ | 3/✓ | 138 | 348275 | 366784 | 0.318 |
| G→X | 2 | 1/✓ | 1/✓ | 131 | 219763 | 227434 | 0.297 |
| G→X | 3 | 1/✓ | 3/✗ | 132 | 444980 | 443597 | 0.495 |
| X→C | 1 | 2/✓ | 2/✓ | 98 | 492115 | 220212 | 0.738 |
| X→C | 2 | 2/✓ | 3/✗ | 105 | 548597 | 302840 | 0.982 |
| X→C | 3 | 2/✓ | 3/✗ | 121 | 842590 | 285930 | 0.927 |
| X→G | 1 | 2/✓ | 1/✓ | 133 | 383288 | 343553 | 0.343 |
| X→G | 2 | 1/✓ | 1/✓ | 63 | 226770 | 265315 | 0.239 |
| X→G | 3 | 2/✓ | 1/✓ | 92 | 284477 | 348437 | 0.383 |
| X→X | 1 | 1/✓ | 3/✗ | 70 | 463109 | 395322 | 0.000 |
| X→X | 2 | 1/✓ | 3/✗ | 65 | 447694 | 424093 | 0.000 |
| X→X | 3 | 1/✓ | 1/✓ | 64 | 215525 | 214038 | 0.000 |

## Findings

1. **Correctness did not discriminate.** Every pairing produced a config resolver that passes the
   public suite, the held-out generalization suite, `-race`, and the exported-API check. All three
   frontier models solve this task as implementor, and no reviewer configuration produced a wrong
   deliverable. The instrument separates the models on *process*, not on *pass/fail*.
2. **Cross-vendor adversary → higher loop convergence than same-vendor.** Code-loop convergence:
   **cross-vendor 15/18 (83%)** vs **same-vendor diagonal 6/9 (67%)**. The effect is concentrated on
   codex-authored code: codex-self (X→X) converged 1/3 and claude-reviewing-codex (X→C) 1/3, but
   **grok-reviewing-codex (X→G) converged 3/3** — a different-vendor reviewer settled codex's code
   where codex's own and claude's review kept raising majors through the 3-round cap. Note:
   non-convergence here means the adversary kept finding major issues across 3 rounds; the final
   code still passed every objective grade.
3. **Cost / efficiency:** grok is the cheapest reviewer and implementer on the comparable
   (claude+grok) cells; X→G and G→X are the cheapest measurable cells. claude self-review (C→C) is
   the most expensive comparable cell ($1.36) and the only claude-imp cell that failed to converge
   (rep 3).
4. **Code size (style proxy):** codex writes markedly terser solutions (mean LOC 66–108) than grok
   (133–164) or claude (152–177). Whether terser is better is a panel question (not scored here).

## Caveats / what this run does NOT establish

- **objective_quality = partial.** The frozen battery's goroutine-leak check and extensibility probe
  do not exist under the chair-only instrument tree; they were not run. No cases were invented into a
  cell. Only build/vet/public/held-out/race/API were applied.
- **Panel score (naming/structure, authorship-stripped) = deferred** (secondary; not run).
- **Codex cost is uncaptured** (no cost field), so cross-vendor cost comparisons involving codex are
  one-sided; only token counts are comparable there.
- Three true repeats give only a coarse error bar; the convergence differences (esp. X→G 3/3 vs X→X
  1/3) are suggestive, not significance-tested.

## Recommendation

Because correctness saturated, the config-resolver instrument has reached its ceiling for separating
these three models on *quality*. To answer the headline quality question, run the **generality task**
(retry-with-backoff wrapper) and/or add the missing goroutine-leak + extensibility probes to the
chair-only battery, then re-run. The convergence signal (cross-vendor > diagonal, driven by codex
code) is the strongest result from this pass and is worth confirming with more repeats.
