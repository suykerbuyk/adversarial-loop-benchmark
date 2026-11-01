# Response to adversary findings

## Finding: Close cannot promptly wake current Get waiters while joining a slow Loader

**Status: Fixed.** The plan now separates lifecycle notification into `closeStarted` and `closeDone`. The first Close closes `closeStarted` immediately after marking the cache closed and before waiting on the loader wait group. Every Get wait selects on `closeStarted`, then checks the closed state under the mutex and returns `ErrClosed`. `closeDone` is reserved for signaling that all loaders have returned and entries have been dropped; concurrent Close callers wait on it. This addresses the finding while preserving the required join behavior.
