# Task: `walcodec` — a versioned write-ahead-log record codec (Go), v1

> Complete, PUBLIC task specification handed verbatim to the implementor. Everything required to
> produce a correct v1 implementation is stated here. There is NO forward-looking hint: implement
> exactly what v1 asks for.

## Goal
Package `walcodec`. Encode a sequence of records to a framed byte stream and decode it back.

```go
package walcodec

import "errors"

type Record interface { Kind() uint8 }   // exported; concrete types return their tag

type Put    struct { Key string; Value []byte } // Kind 1
type Delete struct { Key string }               // Kind 2

func Encode(recs []Record) ([]byte, error)      // writes a version-1 stream
func Decode(b []byte) ([]Record, error)

var ErrCorrupt = errors.New("walcodec: corrupt stream")
```

`Put` and `Delete` each implement `Record`; `Put{}.Kind()` is `1`, `Delete{}.Kind()` is `2`.

## Wire format (pinned, all integers big-endian)
A stream is `[version uint8]` then zero or more frames.

```
stream = version(1) frame*
frame  = len(4) type(1) payload(len-1) crc(4)
```

- `len` is a `uint32` that counts `type`+`payload` only (the trailing `crc` is **outside** `len`).
- `crc` is a `uint32` CRC32-IEEE (`hash/crc32.ChecksumIEEE`) computed over `type`+`payload`.
- v1 writes `version = 1`.
- `len` is capped at **16 MiB** (`16 << 20` = `16777216`). A `len` larger than the cap is `ErrCorrupt`
  — rejected **before** any allocation, not read.

### Payloads
- `Put`    payload = `[klen uint32][key bytes][vlen uint32][value bytes]`.
- `Delete` payload = `[klen uint32][key bytes]`.

Strings and values are length-prefixed as `[uint32 len][bytes]`. A `nil` `Value` and an empty `Value`
both encode as `vlen = 0`, and both decode to a length-0 `[]byte`; they are treated as **equal**.

## Rules
- `Encode` writes the version byte, then each record's frame **in stream order**.
- The `version` byte is validated **before** any frame; a version the decoder does not know is
  `ErrCorrupt`. (A v1 decoder knows only version 1.)
- `Decode` validates each frame's `crc` (mismatch → `ErrCorrupt`), rejects an unknown `type` tag
  (→ `ErrCorrupt`), a truncated frame (→ `ErrCorrupt`), and an oversize `len` (→ `ErrCorrupt`).
- `Decode` round-trips `Encode` output exactly; records decode in stream order.
- Duplicate keys are kept, in order (the codec is a log, not a map).
- Empty `recs` → a one-byte, version-only stream; decoding it → an empty slice, no error.
- `Decode(nil)` and `Decode([]byte{})` → `ErrCorrupt` (missing version byte).

## Deliverables
- The `walcodec` package implementing `Record`, `Put`, `Delete`, `Encode`, `Decode`, `ErrCorrupt`.
- Builds with `go build`; clean under `go vet`.
- Passes the shipped public test suite (`go test ./...`).
- Idiomatic, maintainable Go.

## v1 public test cases (ship with the task; part of acceptance)
1. Round-trip a `Put` then a `Delete`, decoded back in order.
2. A flipped `crc` byte → `ErrCorrupt`.
3. An unknown `type` tag (with an otherwise valid frame) → `ErrCorrupt`.
4. A truncated frame → `ErrCorrupt`.
5. An oversize `len` (> 16 MiB) → `ErrCorrupt`.
6. Empty `recs` → a version-only (one-byte) stream that decodes to an empty slice, no error.
7. `Decode(nil)` → `ErrCorrupt`.
8. Duplicate keys kept, in order.
9. An empty `Value` and a `nil` `Value` decode equal.
