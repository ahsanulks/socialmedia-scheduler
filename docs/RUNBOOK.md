# Ops runbook (M6)

Everything needed to run, upgrade, back up, and restore the scheduler API. The
service is one Go binary over one SQLite file: the whole operational surface is
the process, the database file, and the environment.

## Run locally

```bash
export API_TOKEN=...            # required
go run ./cmd/api                # serves HTTP_ADDR, default :8080
```

The schema migrations in `migrations/` are applied automatically on every
start-up (`sqlite.Migrate`), so there is no separate migrate step to forget.
Every migration is a plain SQL file `000N_name.sql` applied in filename order
exactly once.

## Health probes

- `GET /healthz` — process is alive: `{"status":"ok"}` (200).
- `GET /readyz` — database answers: 200 `up`, else 503.

Point the load balancer or watchdog at `/readyz`; alert on `healthz` failing
from outside.

## Run in Docker

```bash
docker build -t social-scheduler .
docker run --rm -p 8080:8080 \
  -v /srv/scheduler:/data \
  -e API_TOKEN=... \
  -e DATABASE_PATH=/data/scheduler.db \
  social-scheduler
```

Rules the deployment must respect:

- **One process per database file.** SQLite takes one writer; two containers
  pointed at the same `DATABASE_PATH` corrupt the lease and claim semantics.
- The database must live on a **persistent volume**. The container itself is
  stateless.
- Configure the platform adapters with env vars only (see README's env
  table); every adapter fails fast on incomplete credentials.

## Back up

SQLite holds everything; take a consistent file-level copy with `VACUUM INTO`
(works while the service is up and writes a defragmented copy):

```bash
sqlite3 /srv/scheduler/scheduler.db "VACUUM INTO '/backups/scheduler-$(date -u +%Y%m%d).db'"
```

Run it from cron, keep (7, 30) or whatever your retention policy is. The
upgrade path when the budget allows stream-based replication is Litestream —
deliberately not wired up for MVP.

Suggested crontab line:

```cron
30 3 * * * sqlite3 /srv/scheduler/scheduler.db "VACUUM INTO '/backups/scheduler-$(date -u +\%Y\%m\%d).db'"
```

## Restore

1. Stop the service (it is allowed to be a short outage; discarding the WAL on
   a running process is not).
2. Copy the backup over the live file (remove the companion `*.db-wal`,
   `*.db-shm` files of the live database).
3. Start the service. Migrations run on start-up and no-op if already applied.

Note: `VACUUM INTO` copies are ordinary SQLite files; verify with
`sqlite3 /backups/FILE 'PRAGMA integrity_check'` after writing one.

## Database layout

- `scheduled_posts` + `domain_events` — the write side (aggregate rows and
  their event log, one transaction apart).
- `schedules_read` — the read projection, rebuilt by the projector from
  events; never queried for writes.
- `schedule_attempts` — per-attempt diagnostics (outcome, error, duration).
- `idempotency_keys` — reservations and stored POST `/schedules` responses
  behind the `Idempotency-Key` header. Rows with `response IS NULL` are
  pending reservations: fresh ones belong to an in-flight request (retries
  get 409), an old one is the wake of a crashed process (`pending` after
  `create` failed) and can be deleted manually. The worker deletes rows —
  pending and completed alike — older than `IDEMPOTENCY_RETENTION` (default
  30d) once per hour, so keep retention far above the create duration;
  pruning an in-flight reservation frees the key for re-claiming, which is
    exactly the documented double-create cost — never a database problem. A
    pruned key only means that retry could create the schedule twice.
  twice.

## Common failures

| Symptom | Cause / action |
|---|---|
| Startup: `API_TOKEN is required` | missing var; set it and restart |
| Startup: `THREADS_USER_ID... are required when THREADS_ENABLED=true` | adapter enabled with partial credentials; fix env or disable the adapter |
| Lots of `lease expired` in logs | publish flow hung (network) — recovery is working as designed; check `schedule_attempts` for the slow attempt |
| `last_error` contains `429` | platform rate limit/backoff; the post retries automatically up to `MAX_PUBLISH_ATTEMPTS` |
| `version_conflict` on writes | concurrent writers on one row; clients must refetch and retry (optimistic concurrency is the contract) |
