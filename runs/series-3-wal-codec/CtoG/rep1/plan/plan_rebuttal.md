# Rebuttal to plan findings (phase 2, round 2)

## major: "empty Batch adds 0 to used, so empty-node fan-out is exponential": premise REJECTED. Guard and test ACCEPTED.

**The premise is wrong.** r2 §2.3 step 5 sets `used += len(out) - before`, where `before` is taken
**before** `appendFrame` writes the child. That difference is the child's **full** frame:
`len(4) + type(1) + payload + crc(4)`.

An empty `Batch{}` child appends:
- a 4-byte `len` placeholder;
- the type byte;
- `n = 0` (4 bytes);
- its crc (4 bytes).

That is **13 bytes**, so the parent's `used` grows by 13, not 0, and `childLimit` shrinks by 13 for the
next sibling. The budget also nests:
- A child is entered with `childLimit = limit − used − 8`.
- It returns a frame of at most `childLimit + 8` bytes.
- So `used ≤ limit` after every child, at every depth.

So a Batch of empty Batches drains the 16 MiB budget at 13 bytes per node. Encode fails after about
1.29 M visits, whatever the logical size of the aliased tree.

**What was wrong in r2 was the proof, not the bound.** r2 said "each visited record writes at least 13
bytes" without saying which bytes, so the argument read as if it relied on leaf payloads. r3 §2.3
rewrites it as exclusive-byte accounting:
- Every successful visit writes its own `len`, type, crc and first count or length field: 13 bytes that
  no other visit writes. The empty Batch is called out explicitly.
- Failed visits abort the whole Encode, leaving at most 9 unfinished.
- So there are ≤ (16 MiB + 8)/13 + 9 visits per top-level frame.

**Accepted anyway:**
- **Guard.** The child guard is now `childLimit < minRecordBody (5)` instead of `< 1`, checked before
  recursing. No record of any kind fits in less than 5 bytes of `len`, so this never rejects encodable
  input. It just stops the descent one call earlier.
- **Constants.** `minRecordBody = 5` is new, and `minRecordFrame` is defined as
  `frameOverhead + minRecordBody`.
- **Tests (§5):**
  - **Fan-out-16, depth-8, empty-leaf test.** It must error within 2 s and under 64 MiB of allocation.
    A design where empty children added 0 would walk ≈ 2.9 × 10^8 nodes and fail this.
  - **Fan-out-5 success control.** `len(out)` must equal exactly `1 + 13 × nodes`. This pins the
    13-bytes-per-empty-node fact the bound rests on, and shows the guard rejects nothing valid.
- **CRC cost.** §2.3 also states it: each byte is checksummed at most 9 times.

## minor: "frame errors carry no depth": ACCEPTED.

r2 promised depth on every frame error but only wrote it into the truncated-header message. r3 §2.2
fixes this structurally rather than message by message:
- A single helper, `corruptAt(off, depth, format, ...)`, appends `at offset <off> (depth <d>)`.
- `frame`, `payload` and `batch` may raise errors **only** through it. Callee errors pass through
  unchanged. Plain `corrupt` is kept only for the two stream-level version checks.
- `depth` is defined as the depth of the stream holding the failing frame.
- `batch` now takes the Batch frame's own depth and checks `depth+1 > 8`. That is textually the encoder's
  predicate, and it gives each error an unambiguous depth.

§5 replaces the single offset test with a table test. It has one depth-2 corruption of every kind, and
each case asserts that the message contains both the computed absolute offset and `(depth 2)`.

## minor: "vlen = 16 MiB − 22 is not derived": ACCEPTED.

§5 now derives it step by step:
- inner Put `len` = 1 + 4 + len(k) + 4 + vlen
- inner frame = that + 8, inner crc included, since inner crcs are inside the outer `len`
- outer `len` = 1 + 4 + inner frame = 22 + len(k) + vlen

The test computes vlen with a `capVlen(keyLen)` helper that carries this derivation in its comment,
instead of a bare literal. It runs with both an empty and a non-empty key.

The test already asserted that the encoded outer `len` field reads exactly `16<<20`. r3 calls that out
explicitly as the guard against a wrong edit: a constant that drops the inner crc, or forgets the key,
produces a different `len` and fails that assertion even though Encode succeeds.

## nit: "`walcodec:` prefix depends on copying it onto every leaf error": ACCEPTED, resolved the other way.

The finding's suggestion, putting the prefix on every leaf error, would give
`walcodec: record 2: batch record 0: walcodec: unsupported ...` once the nested wraps stack up. Instead,
§2.3 states a single prefix rule:
- Internal helpers return **unprefixed** errors.
- `appendBatch` adds `batch record %d: `.
- `Encode` is the **only** site that adds `walcodec: record %d: `, and it wraps every helper error.

So a nested failure cannot drop the prefix or duplicate it. §5 asserts, for every Encode failure kind:
- `HasPrefix(err, "walcodec: ")`;
- `Count(err, "walcodec:") == 1`;
- `!errors.Is(err, ErrCorrupt)`.

The pseudocode now shows each helper's error constructor (`errors.New` / `fmt.Errorf` without prefix).

## nit: "inner-len-past-payload case is not named, so a split could drop the offset": ACCEPTED.

§2.2 `frame` step 4 is now labelled as the amendment's "inner len past enclosing payload" case, with a
code comment saying so. Its message is "truncated frame: len %d needs %d bytes, %d remain in region".

Because every error in `frame` must go through `corruptAt` (see the depth minor), a later split of the
truncation messages cannot drop the offset or depth without bypassing the only allowed constructor. §5's
inner-len-one-past-payload test now asserts the message contains "truncated frame", the inner frame's
absolute offset and `depth 1`.

**No open blocker or major issue remains from round 2.**
