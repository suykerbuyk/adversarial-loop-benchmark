minor: `encodeRecords` materializes each `Put`/`Delete` payload (and a `Batch`'s complete inner stream) before checking `bodyLen > maxFrameLen`, so oversized inputs that are guaranteed to fail still incur large allocations and copies; validate lengths before allocation to limit avoidable peak memory (`worktree/walcodec.go:62-105`).
No blocker or major issue remains.
