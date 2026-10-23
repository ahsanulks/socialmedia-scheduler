# ADR-0001: Architecture baseline — CQRS + DDD on SQLite, modular monolith

**Date:** 2026-10-21
**Status:** accepted (pending product confirmation of §Decisions in PLAN.md)

## Context

We need a backend that schedules social posts (Threads first) and publishes them at a chosen
time. The constraints given: Go, SQLite, CQRS with DDD, backend only, MVP = schedule with a time,
list the schedule plan, edit and remove a schedule.

This is a small, single-tenant service on one host. The main correctness risks are around state
transitions (don't publish canceled posts twice, don't lose a post on restart) and around
concurrency (two edits racing, two workers claiming the same post) — not throughput.

## Decisions

### 1. CQRS with separate read model, without event sourcing

Write side: a `ScheduledPost` aggregate enforcing invariants, persisted to normalized tables.
Read side: a flat `schedules_read` projection queried directly, never through the aggregate.
Domain events are appended to a `domain_events` table in the same transaction as the state change.

**Why not full event sourcing:** reconstructing every aggregate from events adds replay,
snapshotting, and schema-versioning cost that a CRUD-shaped scheduler does not earn back. The
event log is kept as an append-only audit trail, so migrations toward event sourcing (or a
broker-fed async projection) stay open without rewriting handlers.

**Why not a single shared model:** the read side needs filtering, ordering, and pagination on
columns that have nothing to do with aggregate invariants; splitting the model lets both sides
evolve independently and keeps query code free of domain reconstruction.

### 2. SQLite with a single writer

One process, one write connection (`SetMaxOpenConns(1)`), WAL mode, `busy_timeout=5000`,
`BEGIN IMMEDIATE` for claims. Write transactions are short (one aggregate + one projection update).
This makes the claim operation trivially race-free without locks in application code.

**Consequence:** horizontal scaling is off the table until Postgres. Acceptable for MVP; the
repository interfaces are SQL-agnostic enough to swap.

### 3. Modular monolith, in-process scheduler

One binary: HTTP server plus a tick loop that claims due posts. The scheduler lives in its own
package behind an interface so it can be split into `cmd/worker` later.

**Why not a separate worker now:** two processes would need leader election or a lock to avoid
double publishing against a single SQLite file. One process makes the concurrency story simple.

### 4. Threads behind an anti-corruption layer

`internal/scheduling/adapters/threads` speaks Graph API; the domain sees `Publisher` with
`Prepare` / `Publish`. Two-phase because the draft reference must be persisted before
publishing for crash recovery. Meta error codes map to retryable/permanent at the adapter boundary.

### 5. Cancel is a soft delete

`DELETE /schedules/{id}` sets `status=canceled` and keeps the row. Audit value outweighs storage
savings, and it makes the operation idempotent. Purge is a separate future concern.

## Consequences

- Write handlers return `{id, version}` only; clients read the projection for details. This is the
  visible CQRS discipline and keeps command responses cheap and stable.
- Projection and write model must stay in sync in one transaction; a bug there is a data-integrity
  bug. Mitigation: projection is driven by events, and can be rebuilt by replaying `domain_events`.
- Exactly-once publishing is not achievable against Threads (no idempotency key). The system is
  at-least-once with a documented manual reconciliation path.
- Adding media, multi-account, or recurring schedules will grow the aggregate; the split point
  (content vs. timing vs. delivery) is noted in the plan so it is a refactor, not a rewrite.
