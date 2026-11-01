# Sample transcripts

Full raw model transcripts (every plan- and code-loop turn) for three cells chosen
to show the mechanism. Each `turns/` dir holds `p1..p3_{imp,adv}` (plan loop) and
`c1..c3_{imp,adv}` (code loop) as the model's raw responses, plus `brief_*.txt`
(the exact prompts sent). The other 78 cells' transcripts are omitted for size;
their code, reviews, and grades are in `runs/`.

| Sample | Why it's here |
|---|---|
| `series-2-block-cache/CtoG-rep2__shipped-race` | **Convergence ≠ correctness.** Claude implemented, Grok reviewed; the loop converged, yet the delivered cache failed `-race`. Read the adversary's findings and see what got signed off. |
| `series-2-block-cache/CtoC-rep1__diagonal-converge` | A clean same-vendor convergence on the hard concurrency task, for contrast. |
| `series-3-wal-codec/GtoC-rep2__best-extension-attempt` | The single highest additive-change ratio in Series 3 (0.13) — i.e. the closest any cell came to a reusable seam, which is still far from the reference's 1.00. |
