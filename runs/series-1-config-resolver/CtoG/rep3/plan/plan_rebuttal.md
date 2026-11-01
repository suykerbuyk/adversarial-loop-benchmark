# Response to plan findings (round 1 → round 2)

1. **blocker: `map[string]*field` pointing into `fields`. FIXED (but the stated hazard is rebutted).**
   - In Go a pointer to a slice element cannot point to "freed" memory. The GC keeps the backing
     array alive while it is referenced. After an `append` that reallocates, the old pointer still
     sees a valid copy, only a stale one. Also, `fields` is built once at init and never appended.
     So the original plan had no actual memory-safety or correctness bug.
   - The stale-copy concern is still a real maintainability trap, and avoiding it costs nothing.
     §2.2 now uses `fieldIndex map[string]int`, built by `indexFields`, and never takes the address
     of a slice element. `checkKeys` only needs membership.

2. **major: validation hook not real. FIXED.**
   - §2.3 adds `type validator func(Config) error` and a `validators` slice whose only entry is
     `validateLogLevel`. `validateLogLevel` wraps `ErrInvalidLogLevel` with `%w`.
   - `validate` runs every validator and returns `errors.Join(errs...)`. `resolve` (§3) calls only
     `validate`.
   - A new validated field is now one sentinel, one function, and one slice entry, and the error
     path never changes.
   - A new §6 test injects a stub validator with its own sentinel. It shows that both sentinels
     satisfy `errors.Is` together.

3. **minor: nil as wrong type should be a stated contract. FIXED.**
   - §4.2 now states the contract explicitly: presence is solely key existence, and a present
     `nil` panics. That covers both untyped nil and typed nils such as `(*int)(nil)`.
   - The plan also says "nil means absent" will not be adopted even for future pointer or interface
     fields.
   - The rule is documented on `Resolve` (§1). §6 tests untyped nil and typed nil for each relevant key.

4. **minor: `Config{}` on error is an extra contract. FIXED.**
   - The `Resolve` doc comment (§1) now states: when err != nil the returned Config is the zero
     `Config{}`, and callers must not rely on partial values.
   - Any future partial-result API would be a separate function. §6 asserts the zero result in the
     error cases.

5. **nit: duplicate keys in `fields`. FIXED.**
   - `indexFields` panics at package init on a duplicate key, so any duplicate breaks every test run
     immediately.
   - §6 adds `len(fieldIndex) == len(fields)` and a direct `indexFields` duplicate-panic test.

6. **nit: panic message wording locked by tests. FIXED.**
   - §4.2/§4.3 give the example messages as illustrations only.
   - §6 now asserts substrings only: layer name, key, type names, and sorted order of unknown keys
     via relative positions. The surrounding wording is never asserted.

7. **nit: adding a layer by changing `Resolve`'s signature. FIXED.**
   - §1 now says `Resolve`'s signature is frozen. A new layer gets a new exported entry point that
     forwards to the unexported variadic `resolve`. §5 matches.
