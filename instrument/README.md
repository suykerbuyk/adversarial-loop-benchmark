# Instruments (public parts)

The three task definitions. Each is a self-contained Go exercise given to the
implementor as `spec` + `public-tests`; Series 2 and 3 also include a `reference`
solution. `C`/`G`/`X` cells all received identical instrument inputs.

| Dir | Task | What it probes |
|---|---|---|
| `series-1-config-resolver/` | Layered config resolver (defaults→file→env→flags), with the "a present zero-value must override a lower layer" trap. | **Correctness** on a small, long-lived deliverable. |
| `series-2-block-cache/` | Bounded, concurrent, single-flight block cache with LRU eviction and clean `Close`. | **Concurrency & resource safety** (graded under `-race` + a leak gate). |
| `series-3-wal-codec/` | Two-phase: build a v1 WAL record codec, then absorb a v2 amendment (nested `Batch` record + format version) without breaking v1. See `spec` and the amendment. | **Discovered extensibility** — does the loop build a seam unprompted? |

## What's withheld (on purpose)

To keep the benchmark **honestly re-runnable**, the grading-only material is *not*
published:

- the **held-out generalization suite** (grades generalization beyond the public tests),
- the **naive / reference-correct** solutions used to calibrate the grader,
- Series 1's reference (its reference *is* the withheld correct solution).

Their pass/fail outcomes per cell are fully reported in `../results/`. The public
instrument here is enough to understand each task and re-implement it; the hidden
graders are what make an independent re-run meaningful.
