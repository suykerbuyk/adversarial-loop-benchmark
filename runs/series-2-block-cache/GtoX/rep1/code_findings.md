minor: `cache.go` includes test-only synchronization hooks (`testWaitHook` and `closedForTest`) in the production implementation; this couples production state and `wait` behavior to a specific test and should be moved behind a test seam or removed.
No blocker or major issue remains.
