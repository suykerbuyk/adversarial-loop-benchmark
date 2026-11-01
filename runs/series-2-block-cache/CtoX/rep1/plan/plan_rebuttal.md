# Plan rebuttal — round 3

## Round-2 findings: "No blocker or major issues remain" — **acknowledged, no change required**

The round-2 review contains no blocker, major, minor, or nit findings; it explicitly confirms that
the waiter-settlement design (plan §3 I6, §4.1, §4.2, §4.4) fixes the round-1 major finding
(post-resolution caller-ctx cancellation overriding the load result) and that the plan covers the
stated API rules and public acceptance cases.

Changes this round (clarifications only; no design change):
- Plan header updated to round 3 with revision history.
- §4.4 / §7: documented explicitly that Close settles and wakes waiters with `ErrClosed` before
  joining in-flight Loaders (the spec prose lists "waits" before "wakes"); argued that every
  observable guarantee of rule 8 is preserved and liveness is improved.

## Round-1 Finding 1 (major) — remains **FIXED** (see round-2 rebuttal content, carried forward)

Each Get registers a `*waiter`; its outcome is settled once under `c.mu` by whichever happens
first: resolution (ctx already Done ⇒ `ctx.Err()`, else load result), Close (`ErrClosed`), or the
waiter itself observing its ctx Done while unsettled (`ctx.Err()`). After wake-up the waiter only
reads its settled outcome, so a later cancellation cannot override a resolved result. Regression
tests: plan §6 #5 and #6.
