# Rebuttal: phase 2, round 2 plan findings

Summary: the major is accepted in full. Minor #1 (encoder cap-check ordering) is accepted. Minor #2
(nil success slice) is rejected on the facts about `make`, but the requested pinning and tests are added
anyway. Nit #1 (allocation ceiling) is accepted, with the arithmetic corrected. Nit #2 (`minPayload`) is
accepted, and the constant is removed. This round's adversary also confirmed that the round-1 depth and
cap-accounting disputes are settled.

---

## major: "decodeBatch does not return on a child error": **Accepted.**

The round-2 sketch left out the `if err != nil` lines after `nextFrame` and `decodeRecord`, in both
`Decode` and `decodeBatch`. Read literally, it does what the finding says: it overwrites `err`, appends a
nil record, and advances to `next = 0`. The intent was v1's behaviour. But a plan that is meant to be
checked should not depend on intent, so the fix goes in the text.

What changed:
- §2.2 now gives complete control flow for `Decode`, `decodeRecord` and `decodeBatch`. After `nextFrame`
  and after `decodeRecord`, each loop returns `nil, wrapped err` **before** `append` and before `off =
  next`. That is the order v1 uses.
- `nextFrame`'s contract says its other results are zero and must not be used on error. On success,
  `next ≥ off + 9`, so the loops terminate.
- A "first error wins, at every depth" bullet explains how an inner error comes up through each
  `decodeBatch` level to `Decode` as `(nil, ErrCorrupt)`. So a nil `Record` can never be inside a returned
  `Batch`.
- Public cases 5 and 7 in §3 are re-traced through the explicit returns.
- §5 adds a table test that would catch this bug. Each bad inner frame (truncated, CRC flip, unknown type,
  `len` 0, `len` over the cap, trailing payload bytes, 9th-level Batch) is placed at depth 1 and at depth 3,
  with valid siblings after it and a valid top-level frame after the Batch. Each case asserts both
  `ErrCorrupt` and `got == nil`. A timeout guard turns a spinning loop into a failure. The fuzz oracle
  also asserts that a successful result has no nil `Record` at any depth.

## minor: "running cap check uses len(out)-body after a child that may return (nil, err)": **Accepted.**

In round 2 the order was already err-check first, then length. But `if out, err = appendRecord(...)`
overwrote `out` before the check, so the safety depended on every error path returning a nil buffer. That
is fragile. Now:
- `Encode` and `appendBatch` write `grown, err := appendRecord(out, r, sub)`, check `err` first, and only
  then do `out = grown`. The cap check `len(out) - body` runs after that.
- The child's return value on error is never read. So a later change that returns a partly grown buffer
  with the error cannot cause a panic or skip the check.
- `len(out) ≥ body` holds because every helper only appends. §2.1 says this.
- §2.1 now writes out `appendRecord` and a new `appendLeaf` in full, with every error return.
- §5 adds a test where the 2nd child of a Batch, at depth 1 and at depth 5, is a nil interface. It
  expects `(nil, err)` and no panic.

## minor: "Decode success does not guarantee a non-nil slice": **Rejected on the facts. Pinning and tests added.**

- The claim "if that capacity expression is 0, builtin `make` returns nil" is incorrect. In Go,
  `make([]T, 0, 0)` always returns a non-nil slice: its data pointer is the runtime's zero-size base, and
  `s == nil` is false. v1 already relied on this. Its `Decode([]byte{1})` goes through
  `make([]Record, 0, min(0/13, 1024))` = capacity 0, and returns a non-nil empty slice.
- `min` is defined. It is the builtin from Go 1.21. This module's `go.mod` says `go 1.25`, and v1's
  `Decode` and `Encode` already use it. The round-2 sketch just did not say so.

What I added anyway, because "the plan says so" is weaker than "a test says so":
- §2.2 notes at both `make` sites that the result is non-nil for every capacity, including 0, and names
  `min` as the builtin.
- §5 asserts `got != nil && len(got) == 0` for `Decode([]byte{2})` and `Decode([]byte{1})`. It asserts
  `Records != nil` for an empty Batch at the top level and nested. The fuzz oracle asserts that a
  successful result is non-nil.

## nit: "one allocation ceiling for two different inputs": **Accepted (ceilings split). The arithmetic is corrected.**

The top-level hint is `min((len(b)-1)/13, 1024)` entries, so it depends on the input length. It is not a
flat 1024. Both round-2 hostile inputs are tiny. The huge-`len` stream is 6 bytes, which gives 0 entries.
The `n = 0xFFFFFFFF` stream is 14 bytes, which gives 1 entry (16 B). So a correct decoder would not have
failed the 4 KiB ceiling. The finding's point still stands, though: one unexplained number for different
inputs is fragile, and it does not test the 1024 cap at all. §5 now has four cases, and each ceiling is
derived from its input:
1. `n = 0xFFFFFFFF`: 1 KiB + 16 B.
2. Huge `len`, 6 bytes: 1 KiB.
3. Huge `len` plus 1 MiB of padding: 1 KiB + 16 KiB. This is the case that checks the hint is a hard cap.
4. A legal Batch of 10,000 empty Deletes: ≤ 4·len(b).

## nit: "minPayload = 4 is documented as a payload floor but is not one": **Accepted. Constant removed.**

`minPayload` is gone. `minFrameWire` is now defined directly as `lenSize + typeSize + u32Size + crcSize`
(= 13). The reason is the real invariant: every payload kind starts with a u32 (`klen` for Put/Delete,
`n` for Batch). Its comment says it is a lower bound on wire size, used only for the count guard and the
capacity hint, and that it is not a payload-validity floor. Per-kind payload validity stays in the
parsers, so an empty Put (8-byte payload) and an empty Batch (4-byte payload) are each checked by their
own code.
