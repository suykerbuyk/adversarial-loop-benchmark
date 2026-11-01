# Document index

A map of everything in this repository. Start at the **[README](README.md)** for the
story and headline; use this page to jump straight to any document or artifact.

## Read first
| Document | What it is |
|---|---|
| [README.md](README.md) | Overview, headline findings, method, results grids, licensing. |
| 📊 [report/benchmark-report.html](report/benchmark-report.html) | **The full report** — executive summary, charts, three-series synthesis, methodology, token analysis, and the task appendix. Open locally, or read the hosted copy: [claude.ai artifact](https://claude.ai/artifact/BiY8gLXhNXYqKrx2UNG8Y6). |
| 📋 [RESULTS.md](RESULTS.md) | Full per-cell table for all 81 runs (grades, rounds, LoC, cost, additive ratios), rendered from the data. |

## Results data
| Document | What it is |
|---|---|
| [results/series-1-config-resolver.md](results/series-1-config-resolver.md) | Series 1 result write-up (correctness). |
| [results/series-2-block-cache.md](results/series-2-block-cache.md) | Series 2 result write-up (concurrency). |
| [results/series-3-wal-codec.md](results/series-3-wal-codec.md) | Series 3 result write-up (extensibility). |
| [results/series-1.jsonl](results/series-1.jsonl) · [series-2.jsonl](results/series-2.jsonl) · [series-3.jsonl](results/series-3.jsonl) | Machine-readable per-cell rows (one JSON object per cell). |

## Instruments (the tasks)
| Document | What it is |
|---|---|
| [instrument/README.md](instrument/README.md) | What each task probes; what grading material is withheld and why. |
| [instrument/series-1-config-resolver/](instrument/series-1-config-resolver/) | Config-resolver spec + public tests. |
| [instrument/series-2-block-cache/](instrument/series-2-block-cache/) | Block-cache spec + public tests + reference. |
| [instrument/series-3-wal-codec/](instrument/series-3-wal-codec/) | WAL-codec v1 spec, v2 amendment, public tests + reference. |

## The 81 runs
Each cell: `runs/<series>/<imp>to<adv>/rep<n>/` — `worktree/` (delivered code),
`code_findings.md` + `plan/` (reviewer findings), `grade-*` (chair grades),
and `snapshot-v1/`,`snapshot-v2/` for Series 3.
| Path | What it is |
|---|---|
| [runs/series-1-config-resolver/](runs/series-1-config-resolver/) | 27 correctness cells. |
| [runs/series-2-block-cache/](runs/series-2-block-cache/) | 27 concurrency cells. |
| [runs/series-3-wal-codec/](runs/series-3-wal-codec/) | 27 extensibility cells (two-phase). |
| [runs/series-2-block-cache/CtoG/rep2/](runs/series-2-block-cache/CtoG/rep2/) | ⚑ The cell that **converged but shipped a data race**. |

## Sample transcripts
| Document | What it is |
|---|---|
| [transcripts/README.md](transcripts/README.md) | Why these three cells were chosen. |
| [transcripts/series-2-block-cache/CtoG-rep2__shipped-race/](transcripts/series-2-block-cache/CtoG-rep2__shipped-race/) | Full turns for the shipped-race cell. |
| [transcripts/series-2-block-cache/CtoC-rep1__diagonal-converge/](transcripts/series-2-block-cache/CtoC-rep1__diagonal-converge/) | Full turns for a clean same-vendor convergence. |
| [transcripts/series-3-wal-codec/GtoC-rep2__best-extension-attempt/](transcripts/series-3-wal-codec/GtoC-rep2__best-extension-attempt/) | Full turns for the best extensibility attempt (0.13). |

## Methodology & licensing
| Document | What it is |
|---|---|
| [methodology/driver.sh](methodology/driver.sh) | The chair's orchestration + grading driver (shows the frozen loop, isolation, and how held-out grading is applied). |
| [LICENSE-MIT](LICENSE-MIT) · [LICENSE-APACHE](LICENSE-APACHE) | Code: dual MIT OR Apache-2.0. |
| [LICENSE-DATA](LICENSE-DATA) | Report, data, transcripts: CC-BY-4.0. |
