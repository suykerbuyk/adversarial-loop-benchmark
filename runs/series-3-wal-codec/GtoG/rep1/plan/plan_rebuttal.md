# Rebuttal: `plan_findings.md`

Each finding is accepted. The revised plan in `plan/plan.md` states the contract the finding asked for. No finding is disputed.

## Blocker — `int(n)` truncates a legal Batch count on 32-bit `int`

Accepted. The previous sketch typed the walker count as `int` and converted with `int(n)`. The module does not set `GOARCH`. On a 32-bit `int`, a count in `2^31..2^32-1` truncates, the walker stops early, and the leftover-suffix check reports `ErrCorrupt` for a payload the amendment defines as exactly `n` frames. The encode guard `len(Records) > math.MaxUint32` does not close this: a count that fits in `uint32` still encodes, and that stream must decode.

Pinned: the walker takes the count as `uint64`. The Batch caller passes `uint64(n)` from the `uint32` count word, with no narrowing to `int`. The loop condition compares the number of frames parsed to that `uint64`. A legal count in `2^31..2^32-1` is read in full. Stated in "Shared frame walker" and in "Batch count is `uint64` through the walker".

## Major — key and value length prefixes wrap on encode

Accepted. `encodePayload` still wrote `uint32(len(x.Key))` and `uint32(len(x.Value))` with no range check, and the previous plan said those payload encoders were unchanged. A key or value longer than `MaxUint32` wraps the prefix, so Encode emits a frame whose declared length is shorter than the bytes that follow. Decode then fails exact consumption (`ErrCorrupt`) on a stream the encoder produced, which breaks "the encoder will not emit a stream it would itself reject". The same wrap sits on every nested frame, not only at depth 0.

Pinned: before the `uint32` conversion, `len(key) > math.MaxUint32` and `len(value) > math.MaxUint32` return an encode error and no bytes, the same way the Batch count is refused. The check runs for every `Put` and `Delete`, including one nested inside a Batch. Stated in "Encode lengths must fit in `uint32`".

## Minor — depth check and CRC order disagree on a malformed inner payload

Accepted. "Depth" said an over-deep Batch is `ErrCorrupt` before the inner count is read, even when the payload is truncated or otherwise malformed. The walker said CRC of the inner frame is checked before that frame's payload is parsed, and that depth is not a substitute for CRC. An implementor who puts the depth check inside the payload parser rejects a bad inner CRC first; an implementor who checks depth on entry rejects depth first and never looks at the inner bytes.

Pinned one order, for every frame including an inner frame: outer CRC, then depth, then inner count, then inner frames. A depth-9 Batch whose inner body has a bad CRC is `ErrCorrupt` for depth. The inner body is not parsed. The outer frame's CRC is still checked before the depth check, so a bad outer CRC is `ErrCorrupt` for CRC and the depth check does not run. Stated in "Depth" and in the walker order list.

## Minor — `1+len(payload)` wraps in `int` before the 16 MiB check

Accepted. `appendFrame` returned `errOversize` when `1+len(payload)` exceeds 16 MiB, and the plan said that length is computed before the length word is written. `1+len(payload)` is an `int` add. A payload of length `MaxInt` wraps the sum, skips the cap, and then `uint32(bodyLen)` wraps the length word the same way the Batch count used to. This is reachable from a Batch whose children assemble a payload near the `int` limit, which the 16 MiB check is supposed to refuse.

Pinned: compare with `len(payload) > int(maxFrameLen)-1` (the v1 form) or with `uint64`. Do not add `1` in `int` before the check. The length word is written only after that comparison succeeds. Stated in "Frame body length must not wrap".

## Nit — `Kind()` on a pointer must be the dereferenced value

Accepted. `Kind()` has a value receiver, so a non-nil `*Put` promotes and returns 1. A type switch that checks `Kind()` on the pointer variable and then encodes a copied value is fine today, and drifts if a later edit type-asserts the pointer and calls a different method set.

Pinned: both the `Kind()` check and the payload encoder see the dereferenced struct. A non-nil pointer is dereferenced once, then `Kind()` is called on that value, then the payload is encoded from that same value. Stated in "Concrete types the encoder accepts".

## Nit — top-level decode copies one frame at a time

Accepted. Calling `decodeFrames(..., 1)` once per frame and appending matches the contract, and it also copies the record slice on every frame. One path returns `rest` and the other assumes a single record, so a later edit can desync the top-level caller from the Batch caller.

Pinned: one walker serves both callers. The top-level caller asks for "until empty" (`n == drain`). The Batch caller passes the `uint64` count and requires an empty remainder. Both share the remainder rule. Stated in "Shared frame walker".
