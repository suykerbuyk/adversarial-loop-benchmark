# Implementation Plan — `blockcache` (bounded, concurrent, single-flight block cache)

Round 2. This revision answers `plan_findings.md` (round 1). The point-by-point response is in
`plan_rebuttal.md`. Changes from round 1, in summary:
- The Loader join no longer uses a `sync.WaitGroup`. It uses an `active` counter guarded by `mu` plus
  a `drained` channel that is closed exactly once (§2, §3.2, §3.5). (Finding 1)
- The discard decision is gated on `c.closed`, not on `c.ctx.Err()` (§3.2). (Finding 2)
- The atomicity of check-closed / lookup / insert / count / spawn in `Get` is stated explicitly (§3.1). (Finding 3)
- `admit` no longer has the defensive overwrite branch. A double admit panics (§3.3). (Finding 6)
- Loader panics are recovered only around the Loader call itself. The error carries the panic value
  and a stack trace. Completion runs in a `defer`, so it also survives `runtime.Goexit` (§3.2). (Finding 7)
- Findings 4 and 5 are rebutted. The reasoning is now written into §3.1 and §3.2 as well.

## 0. Files and environment

- `worktree/blockcache.go`: the whole package (public API, data model, Get/Len/Close, the load
  goroutine, LRU helpers). One file is enough for something this small.
- `worktree/blockcache_internal_test.go`: extra tests for rules the public suite does not cover
  (§6). The public suite `blockcache_public_test.go` is used exactly as shipped.
- `go.mod` stays `module blockcache`, `go 1.25`. The installed toolchain is go1.27.2. Go 1.25 is
  enough for `testing/synctest.Test`. Standard library only: `context`, `errors`, `fmt`, `bytes`,
  `sync`, `container/list`, `runtime/debug`.

## 1. Public API (exactly as the spec gives it)

```go
type Loader func(ctx context.Context, key string) ([]byte, error)

type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Len() int
    Close() error
}

var ErrClosed = errors.New("blockcache: closed")

func New(capacity int, load Loader) Cache
```

- `New` panics if `capacity < 1` (`"blockcache: capacity must be >= 1"`) and if `load == nil`
  (`"blockcache: nil Loader"`).
- `New` returns the unexported `*cache`. Nothing else is exported.
- `ErrClosed` is returned unwrapped, so both `==` and `errors.Is` work.
- Caller preconditions, written down in doc comments. These are the standard single-flight caveats
  and the spec does not contradict them:
  - The `ctx` passed to `Get` must be non-nil.
  - A Loader must not call `Close` on its own cache. Close waits for that same Loader to return, so
    it would deadlock.
  - A Loader must not `Get` its own key. It would wait on its own result.
  - A Cache must not be shared across `testing/synctest` bubble boundaries. This is the same rule
    that applies to any channel-based type: the cache's channels belong to the bubble they were
    created in.

## 2. Internal data model

```go
type cache struct {
    capacity int
    load     Loader

    // Cache-scoped context handed to every Loader call. Derived from context.Background();
    // cancel is called only by Close, after closed=true.
    ctx    context.Context
    cancel context.CancelFunc

    closing chan struct{} // closed in the critical section that sets closed=true (wakes waiters)
    drained chan struct{} // closed exactly once, when closed && active == 0 (the join)

    mu       sync.Mutex               // guards everything below
    closed   bool
    active   int                      // number of running load goroutines (Loader not yet completed)
    lru      *list.List               // resident entries; Front = MRU, Back = LRU; Value = *entry
    items    map[string]*list.Element // resident key -> element of lru
    inflight map[string]*call         // key -> in-flight load (NOT resident, not counted, not evictable)
}

type entry struct {
    key string
    val []byte // cache-owned copy; never mutated after insertion
}

type call struct {
    done chan struct{} // closed exactly once, after val/err are final
    val  []byte        // cache-owned copy on success; nil otherwise
    err  error         // Loader error, or ErrClosed when the result was discarded by this cache's Close
}
```

All channels (`closing`, `drained`, `call.done`) are created by `New`/`Get`, so they belong to the
caller's goroutine tree. Nothing in the cache holds a `sync.WaitGroup` or a `sync.Cond`.

Invariants (each holds whenever `mu` is not held):
- I1: `lru.Len() == len(items) <= capacity`.
- I2: no key is in both `items` and `inflight`. A key enters `inflight` only while it is absent from
  `items`. The completion critical section removes it from `inflight` and, on success, adds it to
  `items` in the same step.
- I3: `active == len(inflight)`. Each running load goroutine owns exactly one `inflight` entry, and
  both are removed in the same critical section.
- I4: if `closed` is true, then `closing` is closed, `items`/`lru` are empty, and `active` never
  increases again. `drained` is closed if and only if `closed && active == 0`.
- I5: `call.val`/`call.err` are written only before `close(call.done)` and read only after
  `<-call.done`. The channel close provides the happens-before edge. `entry.val` and `call.val` are
  never written after publication. Callers only ever receive a fresh `bytes.Clone`, so an entry and a
  call can safely share one backing array.

## 3. Algorithms

### 3.1 `Get(ctx, key)`

The whole decision is made in **one** critical section: closed check, hit, join, or start. So no
Close can slip in between "closed check passed" and "inflight insert / active++ / spawn".

```
c.mu.Lock()
if c.closed { c.mu.Unlock(); return nil, ErrClosed }             // rule 8: no Loader call
if el, ok := c.items[key]; ok {                                  // rule 1: hit
    c.lru.MoveToFront(el)                                        // rule 6
    out := bytes.Clone(el.Value.(*entry).val)                    // rule 4
    c.mu.Unlock(); return out, nil
}
cl, ok := c.inflight[key]
if !ok {                                                         // rule 2: the first miss starts the one load
    cl = &call{done: make(chan struct{})}
    c.inflight[key] = cl
    c.active++                                                   // still under mu, closed == false
    go c.runLoad(key, cl)
}
c.mu.Unlock()
return c.wait(ctx, cl)
```

Since Close sets `closed` under the same `mu`, there are only two possible orders. Either Close's
critical section comes first, and this Get returns `ErrClosed` without touching the Loader. Or this
Get's critical section comes first, and the load it starts is counted in `active` before Close looks
at it, so Close joins it.

`wait(ctx, cl)` implements the rule-7 priority:

```
select {
case <-c.closing:
case <-cl.done:
case <-ctx.Done():
}
// select picks randomly among ready cases, so priority is re-checked explicitly:
select { case <-c.closing: return nil, ErrClosed; default: }     // (a) closed
select { case <-cl.done:                                         // (c) resolved; beats a late ctx
           if cl.err != nil { return nil, cl.err }
           return bytes.Clone(cl.val), nil
         default: }
return nil, ctx.Err()                                            // (b) ctx Done while still in flight
```

Close can race with the gap between the closing check and the done check (finding 4). That case is
still correct, because `cl.done` can only have been closed in one of two ways:
- **Before** Close's critical section. The load resolved before Close began, so the Get linearizes
  before Close and returning the value (or the Loader's error) is correct. A concurrent operation
  can always be ordered either way. No amount of re-checking removes the window between the last
  check and `return`.
- **After** Close's critical section. Then `runLoad` saw `closed == true` and took the discard
  branch, which sets `cl.err = ErrClosed` (§3.2). So the waiter returns `ErrClosed` from the done
  path too. This is the reason the discard branch stamps `ErrClosed` on the call.

Further notes:
- A waiter blocks only in a channel `select`, never on a mutex. Under `synctest` the waiters are
  therefore durably blocked, which `TestSingleFlight`'s `synctest.Wait()` relies on.
- If `ctx` is already Done on a miss, the Get still joins or starts the load and then returns
  `ctx.Err()` (literal 7(b)). The load continues and populates the cache. A hit returns data
  whatever the ctx state, because a hit never waits.
- Each waiter calls `bytes.Clone` itself, so every waiter gets its own backing array (rules 2 and 4).
- On error, Get returns `nil` bytes. `TestErrorNotCached` asserts `b == nil`.

### 3.2 `runLoad(key, cl)`: the single Loader call (runs in its own goroutine)

```go
func (c *cache) runLoad(key string, cl *call) {
    val, err := []byte(nil), errLoaderExited   // unexported sentinel; replaced on normal return/panic
    defer func() { c.complete(key, cl, val, err) }() // runs on return, recovered panic, or runtime.Goexit
    val, err = c.callLoader(key)
}

func (c *cache) callLoader(key string) (val []byte, err error) {
    defer func() {
        if r := recover(); r != nil {
            val, err = nil, fmt.Errorf("blockcache: loader panicked for key %q: %v\n%s", key, r, debug.Stack())
        }
    }()
    return c.load(c.ctx, key)                  // cache-scoped ctx only, never a caller's (rule 7)
}

func (c *cache) complete(key string, cl *call, val []byte, err error) {
    var own []byte
    if err == nil { own = bytes.Clone(val) }   // rule 4: copy BEFORE taking mu; Loader's slice not retained
                                               // rule 3: (bytes, err) -> err wins, bytes dropped
    c.mu.Lock()
    delete(c.inflight, key)
    c.active--
    switch {
    case c.closed:                             // rule 8: result after Close/cancel is discarded, not stored
        cl.err = ErrClosed
    case err != nil:                           // rule 3: not stored; key absent from both maps -> next Get reloads
        cl.err = err
    default:
        cl.val = own
        c.admit(key, own)                      // rules 5/6
    }
    close(cl.done)
    if c.closed && c.active == 0 {
        close(c.drained)                       // last joined load: release Close (and any 2nd Close)
    }
    c.mu.Unlock()
}
```

- **Discard gate is `c.closed`** (finding 2). The cache context is derived from
  `context.Background()` and only Close cancels it, after setting `closed = true` under `mu`. So
  "the cache-scoped ctx is cancelled" implies `closed`, and the gate covers every result returned
  after cancellation (rule 8). That includes the zero-waiter case and the close-window case. It also
  discards a result that completes in the short window after `closed` is set but before `cancel()`
  runs. That is required, since Close "drops all entries".
- **Why `ErrClosed` is stored on the call** (finding 5): this branch is reached only when *this*
  cache's `closed` flag is set. The stamp is what makes the finding-4 race in §3.1 return
  `ErrClosed`. It never outlives the closing signal in a way that matters: by then `closing` is
  already closed, and the call has been removed from `inflight`, so no new Get can ever reach it.
- The load goroutine is independent of its waiters. It keeps running and populates the cache even
  when every waiter has left (orphaned load, rule 7).
- `close(cl.done)` runs in the same critical section as the map updates. A Get that comes later
  sees either the resident entry or no entry, never a stale call. Len reflects the completion within
  that critical section.
- Panic and Goexit safety (finding 7): `recover` covers only the user's Loader call. Cache code
  (`complete`, `admit`) is outside it, so a cache bug still crashes loudly. A Loader panic becomes an
  error that includes the panic value (`%v`) and the stack, and it goes to every waiter, uncached.
  This is deliberate: re-panicking in a background goroutine would kill the process, and skipping
  `complete` would deadlock every waiter and Close (rule 9). If the Loader calls `runtime.Goexit`
  (for example `t.FailNow` inside a Loader), the deferred `complete` still runs with
  `errLoaderExited`.

### 3.3 `admit(key, val)` (caller holds mu): capacity and LRU

```
if _, ok := c.items[key]; ok {
    panic("blockcache: internal error: admit of resident key")   // impossible under I2; never mutate a published entry
}
for c.lru.Len() >= c.capacity {                                  // evict LRU first (rule 5)
    back := c.lru.Back(); c.lru.Remove(back); delete(c.items, back.Value.(*entry).key)
}
c.items[key] = c.lru.PushFront(&entry{key: key, val: val})       // successful load = MRU (rule 6)
```

- Eviction and insertion happen in one critical section, and Len takes `mu`. So no poll can see
  more than `capacity` entries (rule 5).
- In-flight loads exist only in `inflight`. They are not counted and not evictable until they
  resolve successfully (rule 5).

### 3.4 `Len()`

`c.mu.Lock(); n := c.lru.Len(); c.mu.Unlock(); return n`. Resident entries only.

### 3.5 `Close()`

```
c.mu.Lock()
if c.closed { c.mu.Unlock(); <-c.drained; return nil }   // idempotent; a 2nd Close waits for the same join
c.closed = true
close(c.closing)                                         // wake every current waiter -> ErrClosed (priority (a))
c.items = make(map[string]*list.Element); c.lru.Init()   // drop all entries
if c.active == 0 { close(c.drained) }                    // nothing to join
c.mu.Unlock()
c.cancel()                                               // Loaders observe ctx.Done and return promptly
<-c.drained                                              // wait for every in-flight Loader call to return
return nil
```

- **Join without a WaitGroup** (finding 1). `active` changes only under `mu`. It is incremented
  only while `!closed`, so after Close's critical section it can only decrease. `drained` is closed
  exactly once: by Close itself if `active == 0` at that moment (in which case no `complete` can run
  later), or otherwise by the `complete` that brings `active` to 0. Because `complete` runs after
  the Loader call has returned, `<-drained` means "every in-flight Loader call has returned".
  `drained` is a plain channel created in `New`. Waiting on it is durably blocking inside the bubble
  that created the cache, and there is no cross-bubble state such as WaitGroup bubble association.
- Ordering: `closed` and `closing` are set before `cancel()`. Any Loader that sees cancellation
  completes after `closed == true` and hits the discard branch. Dropped entries stay dropped, since
  `admit` is never reached once `closed` is set.
- When Close returns, every Loader call has returned and every `call.done` is closed. `inflight` is
  empty (I3, with `active == 0`), and `items`/`lru` are empty, so `Len() == 0`.
- Waiters are woken at the start of Close, not after the join. Close itself still returns only
  after the join. Waking early is required by 7(a): a waiter whose ctx expires during the join must
  get `ErrClosed`, not `ctx.Err()`. The value the waiter receives is the same either way, because the
  in-flight result is discarded.
- `TestSingleFlight`'s cleanup Close runs inside the bubble, on a cache created inside that bubble.
  Its only blocking operation is `<-c.drained`, which is durably blocking there. In that test the
  Loader has already returned, so `active == 0` and Close does not block at all.

## 4. How each rule is satisfied

| Rule | Mechanism |
|---|---|
| 1 Hit | `items` lookup under `mu`, `MoveToFront`, return `bytes.Clone`; Loader not touched. |
| 2 Single-flight | `inflight` checked and filled in the same critical section as the `items` check; only the creator spawns `runLoad`; all joiners wait on the same `call.done`; each clones. |
| 3 Error | `err != nil` → `cl.err = err`, bytes dropped (error wins), nothing admitted, key removed from `inflight` → next Get starts a new load. |
| 4 Ownership | `bytes.Clone` of the Loader's slice before storing; `bytes.Clone` on every return path (hit and waiter); stored slices never mutated (no overwrite path in `admit`). |
| 5 Capacity/LRU | `admit` evicts `lru.Back()` until below capacity, then pushes, all in one critical section; in-flight loads only in `inflight`; `Len` reads `lru.Len()` under `mu`. |
| 6 Recency | `MoveToFront` on hit; `PushFront` on successful load. |
| 7 Context/orphans | Loader gets `c.ctx` only; load goroutine independent of waiters; `wait` enforces closed > resolved > ctx Done. |
| 8 Close | One critical section sets closed, closes `closing`, drops entries; `cancel`; `<-drained` join via `active` counter; second Close waits on the same `drained`; discard gated on `closed`; Get's closed check is atomic with start-load. |
| 9 Concurrency | All shared state under one mutex; cross-goroutine values published via channel close; no lock held while calling the Loader, cloning Loader output, or blocking; single mutex so lock order is trivial; Loader panics/Goexit cannot skip `complete`. |

Deadlock check: `mu` is never held while blocking, while calling the Loader, or while waiting on a
channel. Close waits only for Loaders, which must be prompt on ctx Done by contract. `drained` is
always closed once `closed && active == 0`, and `active` reaches 0 because every load goroutine runs
`complete` exactly once.

## 5. Edge cases (decided)

- Loader returns `(nil, nil)`: success, stored value `nil`, Gets return `nil, nil`. Loader returns
  `([]byte{}, nil)`: Gets return a non-nil empty slice. `bytes.Clone` keeps nil and empty distinct.
- `capacity == 1`: each new successful load evicts the single resident entry. An in-flight load
  does not displace it until that load succeeds.
- A Get after Close has started returns `ErrClosed` with no Loader call, hits included.
- Close while waiters are parked: they return `ErrClosed` immediately, and the Loader result is
  discarded.
- A Loader that itself returns `ErrClosed` is just a Loader error (rule 3). Waiters receive it and
  nothing is cached. The cache does not reinterpret it.

## 6. Test approach

1. Public suite as shipped: `go test ./...`, `go test -race -count=20 ./...`, `go vet ./...`,
   `gofmt -l .` (must print nothing).
2. Internal tests in `blockcache_internal_test.go`. Overlap is made deterministic with
   `testing/synctest`. Every test is bounded by a ctx deadline. Inside bubbles, `t.Errorf` is used
   before draining.
   - Per-waiter copies: N single-flight waiters; mutate result[0]; the others and a later hit are
     unchanged, and the backing arrays are distinct.
   - Loader slice not retained: the Loader mutates its returned slice after returning; a later hit
     is unchanged.
   - Error wins: `([]byte("x"), err)` gives `(nil, err)`, `Len()==0`, and the next Get reloads.
   - Error under single-flight: all waiters get the same error; exactly one Loader call.
   - Orphaned load populates the cache: the sole waiter's ctx is cancelled → `context.Canceled`.
     Release the Loader, `synctest.Wait()`; then `Len()==1` and the next Get hits (calls == 1).
   - Priority: ctx cancelled while in flight → `ctx.Err()`. Close while waiting → `ErrClosed`, even
     when the waiter's ctx is also Done.
   - Close waits for the Loader: the Loader blocks on `<-ctx.Done()` then sets a flag. After Close
     returns, the flag is set, `Len()==0`, and the waiter got `ErrClosed`.
   - Close discards a late success: the Loader ignores cancel and returns success after it. After
     Close, `Len()==0`.
   - Close with zero in-flight loads returns immediately (inside synctest: no durable block).
   - Concurrent Close: two Closes while a Loader is joining. Inside synctest, after
     `synctest.Wait()` both are still blocked. Release the Loader; both return nil, and neither
     returned before the Loader did.
   - Get after Close: `ErrClosed`, Loader never called; a pre-Close resident key also gets `ErrClosed`.
   - Get racing Close (many iterations, `-race`): every Get returns either a value or `ErrClosed`.
     After Close returns, the Loader-entry count stays constant and `Len()==0`.
   - Pending load not counted and not evictable: capacity 1, `b` resident, `a` in flight →
     `Len()==1` and Get `b` hits; when `a` resolves, it evicts `b`.
   - A load marks MRU: capacity 2, load a, b, then c → a is evicted.
   - Len bound under concurrency: many goroutines Get random keys while a poller checks
     `Len() <= capacity`, under `-race`.
   - Loader panic: waiters get an error that contains the panic value; the next Get reloads; Close
     does not hang.
   - Loader `runtime.Goexit`: waiters get an error (not a hang); Close returns.
   - `New` panics for a negative capacity.
