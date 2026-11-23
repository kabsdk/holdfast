# 017: Structured schedules, with no cron strings

Status: accepted, 2026-11-21

## Decision

A schedule has one of three forms:
- every N minutes or hours, from a start time each day;
- daily, at one or more times;
- weekly, on chosen days at one time.

It can also have an optional random start spread, in minutes. The only free-text input is numbers.

These values are derived from the schedule and shown with it: the next 5 runs, the longest gap, the
worst-case data loss (gap + spread), and the stale alert threshold (2 × the longest gap).

## Why

- **The derived values need a schedule Holdfast can reason about.** That includes the worst-case loss, the
  stale threshold and checks R001, S001 and R103. A structured form makes this exact.
- **Cron strings are easy to get wrong** and hard to review.

## Consequences

- The agent and the server use the same schedule evaluator, so the runs Holdfast shows are the runs that
  happen.
- The time zone and the handling of daylight-saving changes need a decision.
