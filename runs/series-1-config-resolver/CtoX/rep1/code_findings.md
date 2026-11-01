No blocker or major issues remain. `go build ./...`, `go vet ./...`, and `go test ./...` all pass.
minor: `Resolve` documents that it is “safe for concurrent use” without stating that callers must not mutate the input maps during resolution. Concurrent mutation of a Go map while it is read can race or panic; qualify the guarantee as concurrent calls over maps that are not being modified.
