# Point-by-point response to plan findings

1. **Major — nested Batch count allocation.** The plan now establishes that every complete inner frame needs at least 9 bytes and rejects a declared count greater than `remainingPayload/9` before allocation or count-proportional work. It also explicitly forbids preallocating the record slice from the declared count: decoded records are appended only as each bounded inner frame succeeds. Both safeguards apply recursively at every nesting depth.

2. **Minor — CRC checked after parsing.** The plan now requires CRC32-IEEE verification immediately after validating frame length and confirming the body plus CRC fit in the enclosing slice. Type inspection, field parsing, and Batch recursion happen only after CRC success, at every depth.

3. **Nit — decoded keys and values alias the input.** The plan now requires owning copies for all decoded fields, including inside nested Batches: copy key bytes before conversion to string, and copy each Put value into independent storage. Zero-length values remain length-zero slices.

4. **Nit — partial Encode output on failure.** Every Encode failure at any recursion depth must return a nil byte slice with the error. Recursive helpers may build intermediate data, but no partial version-2 stream is returned to the caller.
