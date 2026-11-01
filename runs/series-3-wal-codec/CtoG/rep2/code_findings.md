No blocker or major issue remains.

nit: appendBatch grows the output by a child frame and only then rejects a parent len over 16 MiB, so a failed Encode can hold about one extra already-capped child (up to roughly 32 MiB) before returning (nil, err). The leaf path checks the cap before appending; the Batch path does not, and nothing in the tests measures that overshoot.
