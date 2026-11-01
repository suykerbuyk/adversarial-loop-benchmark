minor — `ErrCorrupt` changes its public error text from the v1 specified `walcodec: corrupt stream` to `walcodec: corrupt data` in `worktree/walcodec.go`; callers that inspect `Error()` or expect the documented text see a regression despite the v1 surface being frozen.
No blocker or major issue remains.
