minor: The implementation plan describes malformed Batch counts/trailing bytes, version-1 rejection of Kind 3, and the exact depth boundary (eight legal, nine rejected by Encode and Decode), but no planned tests exercise those cases; the shipped suite only checks nested round-trip and a 9-Batch Decode rejection, leaving these key invariants easy to regress.

No blocker or major issue remains.
