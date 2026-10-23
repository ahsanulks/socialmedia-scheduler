# Social Scheduler — MVP Plan

**Status:** draft for approval · **Last updated:** 2026-10-21
**Stack:** Go · SQLite · CQRS + DDD · REST/JSON (backend only)
**Module:** `github.com/ahsanulks/social-scheduler`
**First platform:** Threads (Meta Graph API)

---

## 1. Goal

A backend service that accepts a text post plus a publish time, stores it, and publishes it to
Threads at that time. Clients can inspect the schedule plan, edit content or time, and cancel a
schedule before it runs.

### MVP scope

| In scope | Out of scope (for now) |
|---|---|
| Create schedule: text + `scheduled_at` | Web/mobile UI |
| List / get schedules ("schedule plan") | Images, video, carousels, links cards |
| Edit content and/or publish time | Reply chains / multi-post threads |
| Cancel (remove) a schedule | Recurring schedules, queue slots, drafts |
| Auto-publish at due time to one Threads account | Multi-account OAuth, analytics, approvals |
| Retry transient publish failures | Multi-instance / horizontal scaling |

### MVP user stories

| ID | Story | Acceptance |
|---|---|---|
| US-1 | Schedule a post for a future time | `POST` returns `201` + id; row is visible via `GET`; past times are rejected |
| US-2 | See the schedule plan | `GET /schedules` returns upcoming items ordered by time, filterable by status and date range, paginated |
| US-3 | Edit a scheduled post | `PATCH` changes content and/or time; stale `expected_version` gets `409` |
| US-4 | Remove a schedule | `DELETE` moves it to `canceled`; it disappears from the default list; published items cannot be canceled |
| US-5 | Publish automatically | At `scheduled_at` the worker publishes to Threads once and stores `external_post_id` |
| US-6 | Survive transient failures | Retryable errors are retried with backoff; permanent errors land in `failed` with `last_error` |

### Assumptions

- Single tenant, single Threads account, one service instance.
- All times stored in UTC; clients send RFC 3339 with an offset.
- The backend is not public; a static bearer token is sufficient for MVP auth.
- Threads text limit is 500 characters and the account limit is 250 API posts per rolling 24h.

---

## 2. Domain (DDD)

### Bounded context

**Scheduling** — owns the lifecycle of a planned post. Threads is an external system behind an
anti-corruption layer; the domain never sees HTTP, JSON, or Meta error codes.

### Ubiquitous language

| Term | Meaning |
|---|---|
| **Scheduled post** | Aggregate root: one piece of content planned for one publish time |
| **Content** | The text body, validated to 1–5000 characters (platform limits applied at the edge) |
| **Schedule time** | The instant the post becomes due, stored UTC |
| **Publish attempt** | One try at sending a post to Threads, with outcome and error |
| **Canceled** | Soft-removed by the user; retained for audit, excluded from default queries |

A post and its schedule lifecycle always change together, so MVP uses **one aggregate**, not
separate `Post` and `Schedule` roots. If drafts are introduced later, split content from timing.

### Strict domain types (no primitive obsession)

Every domain concept is its own type with an unexported field and a validating constructor
(smart constructor), in the style of *Functional Programming in Scala*: construct once, valid
forever, never re-check downstream.

| Type | Wraps | Constructor rules | Replaces |
|---|---|---|---|
| `PostID` | string | valid UUIDv7, not zero | raw `string` |
| `PostContent` | string | trimmed, 1–5000 runes (sanity bound) | raw `string` |
| `ScheduleTime` | `time.Time` | non-zero, normalized to UTC | raw `time.Time` |
| `Platform` | string | slug `[a-z][a-z0-9_]{1,31}`, must be a registered adapter | raw `string` |
| `Status` | closed set | only from `ParseStatus`, zero value is invalid | raw `string` |
| `Version` | int | ≥ 1, `Next()` is the only way it grows | raw `int` |
| `Attempt` | int | ≥ 0, `Next()` is the only way it grows | raw `int` |
| `PrepareRef` | string | opaque platform reference, non-empty, bounded length | raw `string` |
| `ExternalPostID` | string | non-empty, bounded length | raw `string` |
| `FailureReason` | string | trimmed, 1–500 runes | raw `string` |

Deliberately **not** wrapped: `time.Time` for audit/lease timestamps (already unambiguous),
`time.Duration` for leases (already carries its unit), and primitives inside SQL rows and HTTP
DTOs (adapters convert at their own boundary).

Scala → Go mapping used throughout:

| FP in Scala | Go equivalent here |
|---|---|
| `case class Email private (value: String)` + smart constructor | struct with an unexported field + `NewEmail(string) (Email, error)` |
| `Either[E, A]` | `(A, error)` with typed errors matched by `errors.As` |
| `Option[A]` | a pointer only where absence is real; mostly **eliminated** by the state ADT below |
| `sealed trait Status` + case objects | unexported interface + unexported state structs (sealed by an unexported method) |
| exhaustive `match` | type switch with a `default: panic` plus a test that enumerates every state |
| immutable case class + `copy` | value structs replaced wholesale (`p.state = next`) |
| newtype | named struct wrapper, so `PrepareRef` can never be passed as `ExternalPostID` |

### Aggregate: `ScheduledPost`

Fields are unexported. Common fields that exist in every state:

| Field | Type |
|---|---|
| `id` | `PostID` |
| `content` | `PostContent` |
| `scheduledAt` | `ScheduleTime` |
| `platform` | `Platform` |
| `version` | `Version` |
| `attempts` | `Attempt` |
| `prepareRef` | `*PrepareRef` |
| `createdAt` / `updatedAt` | `time.Time` |

Plus a sealed **state ADT** — one variant per lifecycle stage, each carrying only the data that is
valid in that stage, which is what makes illegal states unrepresentable:

```
scheduled   { retry *retry }             // retry = { at, reason }; nil means due now
publishing  { leaseUntil time.Time }
published   { externalPostID ExternalPostID, publishedAt time.Time }
failed      { reason FailureReason, failedAt time.Time }
canceled    { canceledAt time.Time }
```

`prepareRef` sits on the aggregate rather than in a state: it is an infrastructure artifact that
must survive a lease recovery back into `scheduled`, not a business fact about the lifecycle.

`Status` is therefore **derived** from the state (`post.Status()`), not stored — a status field and
a state variant can no longer disagree. Consequences that fall out for free:

- a canceled post cannot carry a `publishedAt`, because `canceled` has no such field;
- a `prepareRef` cannot exist outside `publishing`;
- `externalPostID` cannot exist before `published`;
- there is no `time?`/`string?` soup on the aggregate.

**Invariants**

1. Content is non-empty after trimming and at most 5000 runes — a service sanity bound enforced
   by `NewPostContent`. Each platform's real limit lives in its adapter's `Capabilities()` and is
   enforced at the API edge.
2. `scheduledAt` must be strictly in the future when created, edited, or rescheduled — creation
   rule, never re-applied by `Rehydrate`.
3. `scheduled` posts can be edited or rescheduled. `failed` posts can be rescheduled (optionally
   with new content); rescheduling resets attempts and returns them to `scheduled`. `publishing`,
   `published`, and `canceled` are immutable from the user's perspective.
4. `scheduled -> publishing` only via the due-post claim, never via the public API.
5. Canceling is allowed from `scheduled` and `failed`; `published` and `publishing` return `409`.
6. Every mutation advances `version` through `Version.Next()`; a command carries `expected_version`
   and fails on mismatch.
7. State transitions are exhaustive and total: every variant in, exactly one variant out, or a
   typed `InvalidTransitionError`.

### State machine

| From | Trigger | To | Guard |
|---|---|---|---|
| — | `SchedulePost` | `scheduled` | valid content, time in future |
| `scheduled` | `UpdateContent` | `scheduled` | version matches |
| `scheduled` | `Reschedule` | `scheduled` | new time in future, version matches |
| `scheduled` | `Cancel` | `canceled` | version matches |
| `scheduled` | due + claim | `publishing` | `scheduled_at <= now`, lease free, rate budget available |
| `publishing` | publish OK | `published` | — |
| `publishing` | retryable error | `scheduled` | attempts < max, `next_attempt_at` set |
| `publishing` | permanent error | `failed` | — |
| `publishing` | lease expired | `scheduled` | attempts incremented |
| `failed` | `Reschedule` | `scheduled` | new time in future; attempts reset |
| `failed` | `Cancel` | `canceled` | — |
| `published` / `canceled` | — | terminal | — |

### Domain events

`PostScheduled`, `PostContentUpdated`, `PostRescheduled`, `PostCanceled`, `PostPublishStarted`,
`PostPrepared`, `PostPublished`, `PostPublishFailed` (retryable or permanent),
`PostLeaseRecovered`.

Events are appended to `domain_events` in the same transaction as the aggregate write. They feed
the read-model projection, the audit trail, and (later) notifications.

### Tactical patterns used

| Pattern | Where |
|---|---|
| Aggregate root / entity | `ScheduledPost` |
| Value objects (smart constructors) | `PostID`, `PostContent`, `ScheduleTime`, `Platform`, `Status`, `Version`, `Attempt`, `PrepareRef`, `ExternalPostID`, `FailureReason` |
| State ADT (sealed) | `domain/state.go`: `scheduled`, `publishing`, `published`, `failed`, `canceled` |
| Domain events | `internal/scheduling/domain/events.go` |
| Repository (write) | `ScheduledPostRepository` interface, SQLite implementation |
| Read repository | `ScheduleReadRepository`, projection queries |
| Anti-corruption layer | `internal/scheduling/adapters/threads` |
| Domain service | none in MVP; publish orchestration is an application concern |

---

## 3. Architecture — CQRS

```
                  ┌──────────────────────────────┐
   HTTP/JSON      │ adapters/httpapi             │
   ──────────────▶│ auth · routing · DTO mapping │
                  └──────────────┬───────────────┘
                                 │
                  ┌──────────────▼───────────────┐
                  │ application                  │
                  │ command handlers             │
                  │ query handlers               │
                  └───────┬──────────────┬───────┘
                          │              │
              ┌───────────▼────┐   ┌─────▼────────────┐
              │ domain         │   │ read model        │
              │ ScheduledPost  │   │ schedules_read    │
              └───────┬────────┘   └─────▲─────────────┘
                      │ events            │ projection
              ┌───────▼───────────────────┴──────────┐
              │ adapters/sqlite                      │
              │ write tables + domain_events         │
              └───────┬──────────────────────────────┘
                      │
              ┌───────▼──────────┐      ┌─────────────────────┐
              │ scheduler        │─────▶│ adapters/threads    │
              │ claim due posts  │      │ Graph API client    │
              └──────────────────┘      └─────────────────────┘
```

### Rules

- **Commands** load the aggregate, call a domain method, persist the new state, append events, and
  update the projection — all in one SQLite transaction. They return only `{id, version}`.
- **Queries** never load aggregates. They read `schedules_read` directly and return DTOs.
- **Projection is synchronous in MVP.** This gives read-your-writes and removes the need for a
  message broker. The projector takes domain events, so it can become async without changing
  handlers.
- **No event sourcing** (see ADR-0001). State tables are authoritative; `domain_events` is an
  append-only audit/projection log with a unique `(aggregate_id, aggregate_version)` constraint.
- **Dependency rule:** `domain` imports stdlib only; `application` imports `domain`; adapters
  import both; `cmd` wires everything.
- **No exported fields in the domain.** The aggregate has exactly two ways in
  (`NewScheduledPost` for user intent, `Rehydrate` for storage) and three ways out (behavior
  methods, `Snapshot()` for persistence, `PullEvents()` for the event log). See
  [docs/CODE_STRUCTURE.md](CODE_STRUCTURE.md).

### Why this is CQRS and not just layering

| Concern | Write side | Read side |
|---|---|---|
| Model | `ScheduledPost` aggregate with invariants | Flat `schedules_read` projection |
| Optimized for | Correct transitions, concurrency | Listing/filtering/pagination |
| Handlers | `SchedulePost`, `UpdateSchedule`, `CancelSchedule`, `PublishPost` | `GetSchedule`, `ListSchedules` |
| Returns | identity + version | full read DTO |
| Storage | normalized write tables | denormalized projection |

### Package layout

```
social-scheduler/
├── cmd/
│   └── api/main.go                 # config, wiring, HTTP server, worker, graceful shutdown
├── internal/
│   ├── scheduling/
│   │   ├── domain/                 # aggregate, VOs, events, errors — pure Go
│   │   ├── app/                    # commands, queries, handlers, ports (Clock, Publisher)
│   │   ├── adapters/
│   │   │   ├── sqlite/             # write repo, read repo, projector, claim query
│   │   │   ├── threads/            # Graph API client + error mapping
│   │   │   └── httpapi/            # router, handlers, DTOs, problem+json
│   │   └── scheduler/              # tick loop, lease recovery, rate budget
│   └── platform/                   # config, idgen, logging, migrations
├── migrations/                     # goose SQL files, embedded
├── api/openapi.yaml
├── docs/
│   ├── PLAN.md
│   ├── THREADS_API_NOTES.md
│   └── adr/0001-architecture-baseline.md
├── .gitignore
└── README.md
```

### Dependencies (deliberately few)

| Package | Why |
|---|---|
| `modernc.org/sqlite` | Pure-Go SQLite driver, no cgo/toolchain headaches on the home server |
| `github.com/pressly/goose/v3` | Embedded SQL migrations |
| `github.com/google/uuid` | UUIDv7 ids |
| stdlib `net/http`, `log/slog`, `encoding/json` | Router (Go 1.22 patterns), logging, JSON |

No web framework, no ORM, no message broker.

---

## 4. Data model

SQLite, WAL mode, `foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`.
Timestamps are **INTEGER Unix milliseconds** (index-friendly, unambiguous); IDs are TEXT UUIDs.

### `scheduled_posts` (write model)

```sql
CREATE TABLE scheduled_posts (
    id               TEXT PRIMARY KEY,
    content          TEXT    NOT NULL,
    scheduled_at     INTEGER NOT NULL,
    platform         TEXT    NOT NULL,
    status           TEXT    NOT NULL CHECK (status IN
                       ('scheduled','publishing','published','failed','canceled')),
    version          INTEGER NOT NULL DEFAULT 1,
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  INTEGER,
    prepare_ref     TEXT,
    external_post_id TEXT,
    last_error       TEXT,
    lease_until      INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    published_at     INTEGER,
    failed_at        INTEGER,
    canceled_at      INTEGER
);

CREATE INDEX idx_due     ON scheduled_posts(status, scheduled_at);
CREATE INDEX idx_retry   ON scheduled_posts(status, next_attempt_at);
CREATE INDEX idx_created ON scheduled_posts(created_at DESC);
CREATE INDEX idx_published ON scheduled_posts(published_at) WHERE published_at IS NOT NULL;
```

### `publish_attempts` (audit of each try)

```sql
CREATE TABLE publish_attempts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    schedule_id TEXT NOT NULL REFERENCES scheduled_posts(id),
    attempt_no  INTEGER NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    outcome     TEXT NOT NULL,          -- succeeded | retryable | permanent
    prepare_ref TEXT,
    error_code  TEXT,
    error_msg   TEXT
);
```

### `domain_events` (append-only log)

```sql
CREATE TABLE domain_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    aggregate_id      TEXT NOT NULL,
    aggregate_type    TEXT NOT NULL,
    aggregate_version INTEGER NOT NULL,
    event_type        TEXT NOT NULL,
    payload           TEXT NOT NULL,     -- JSON
    occurred_at       INTEGER NOT NULL,
    UNIQUE (aggregate_id, aggregate_version)
);
```

### `schedules_read` (query model)

```sql
CREATE TABLE schedules_read (
    id               TEXT PRIMARY KEY,
    content          TEXT NOT NULL,
    status           TEXT NOT NULL,
    scheduled_at     INTEGER NOT NULL,
    platform         TEXT NOT NULL,
    attempts         INTEGER NOT NULL,
    last_error       TEXT,
    external_post_id TEXT,
    version          INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    published_at     INTEGER
);

CREATE INDEX idx_read_status_time ON schedules_read(status, scheduled_at);
CREATE INDEX idx_read_time_id     ON schedules_read(scheduled_at, id);
```

### `idempotency_keys` (optional, M6)

`key TEXT PRIMARY KEY, request_hash TEXT, status_code INTEGER, response TEXT, created_at INTEGER`.
A repeated `Idempotency-Key` with the same body replays the stored response instead of creating a
second schedule.

### `schema_migrations`

Managed by goose. Migrations run on startup behind a flag or via `goose up` in the runbook.

### SQLite operating rules

- One process, one writer: `db.SetMaxOpenConns(1)` for the write pool.
- A separate read pool may use more connections (`mode=ro`), or the same pool in MVP.
- Wrap claim operations in `BEGIN IMMEDIATE`.
- On shutdown: stop worker, `http.Server.Shutdown`, checkpoint WAL, close DB.
- Backup: `VACUUM INTO '/backups/scheduler-YYYYMMDD.db'` on a cron; Litestream is the upgrade path.

---

## 5. Publish pipeline

### Claiming due posts

Every tick (default 5s), the worker claims a batch atomically:

```sql
BEGIN IMMEDIATE;

UPDATE scheduled_posts
SET status = 'publishing',
    lease_until = :now + 120000,
    updated_at = :now,
    version = version + 1
WHERE id IN (
    SELECT id
    FROM scheduled_posts
    WHERE status = 'scheduled'
      AND scheduled_at <= :now
      AND (next_attempt_at IS NULL OR next_attempt_at <= :now)
    ORDER BY scheduled_at
    LIMIT :batch
)
AND status = 'scheduled'
RETURNING id, content, attempts;

COMMIT;
```

`UPDATE ... LIMIT` is not portable SQLite, so the id subquery + `RETURNING` is used. Because the
process is single-writer and the claim is one transaction, two ticks cannot claim the same row.

### Publishing one post

1. Insert `publish_attempts` row (`started_at`, `attempt_no`).
2. `Prepare(content)` → platform draft reference (nil for single-call adapters). **Persist `prepare_ref` immediately.**
3. `Publish(content, prepare_ref)` → external post id.
4. On success: `status=published`, `external_post_id`, `published_at`, clear lease/error.
5. On retryable failure: `attempts++`, `status=scheduled`, `next_attempt_at=now+backoff`.
6. On permanent failure or attempts exhausted: `status=failed`, `last_error` set.

### Retry policy

| Error class | Examples | Action |
|---|---|---|
| Retryable | network/timeout, HTTP 429, HTTP 5xx | backoff 1m, 5m, 15m, 60m (+20% jitter), max 5 attempts |
| Permanent | HTTP 400/403 with parameter or permission error, invalid content | `failed`, no retry |
| Token invalid | HTTP 401 / code 190 | `failed` + loud log; requires operator action |

Late posts still publish (a schedule that was missed due to downtime is sent as soon as possible).
The API never promises second-level precision; expect the tick interval as worst-case lag.

### Crash recovery

- Rows stuck in `publishing` with `lease_until < now` are reset to `scheduled` (attempts++) by a
  sweep at the start of each tick.
- Duplicate-publish window: if the process dies after Threads accepted the post but before the DB
  commit, a retry can post twice. Threads has no idempotency key. Mitigations: persist
  `prepare_ref` before publishing (a retry reuses the same platform draft where possible), keep the
  window small, log every attempt with `prepare_ref`, and reconcile manually if it happens. This
  is documented as at-least-once, not exactly-once.

### Rate budget

Before claiming, count `published` posts with `published_at > now - 24h`. If the count is at the
configured safety cap (`THREADS_DAILY_LIMIT`, default 240 of 250), skip the tick and log.
This prevents a backlog from burning the platform quota in one burst.

---

## 6. Publisher adapters

Publishing sits behind a platform-agnostic port, so the same schedule flow serves any platform.

```go
type Publisher interface {
    Platform() domain.Platform
    Capabilities() Capabilities // platform's own format rules: MaxTextLength, RequiresMedia, DailyLimit…
    Prepare(ctx context.Context, pub Publication) (*domain.PrepareRef, error)
    Publish(ctx context.Context, pub Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error)
}
```

- `Prepare` creates a platform-side draft when the platform needs one (a Threads container) and
  returns an opaque reference. Adapters that publish in one call return `nil`.
- `Publish` commits the prepared post and returns the platform's post id.
- `Capabilities` is how a platform declares its own format rules (max text length, media
  required, rolling-24h allowance); the API validates against it before a schedule is accepted.
- Adapters translate platform errors into `Retryable` / `Permanent`; the worker never parses HTTP
  status codes.

Threads is the first adapter (`adapters/publishers/threads`). The fake adapter is used in tests and
local dev. Adding Instagram/X/LinkedIn/Mastodon is a new package plus one registration line — no
change to the domain, worker, or API.

Details, limits, and error codes: `docs/THREADS_API_NOTES.md`.

---

## 7. HTTP API

Base path `/api/v1`. Auth: `Authorization: Bearer $API_TOKEN` (health endpoints open).
Errors use `application/problem+json` (RFC 9457) with a stable `code`.

| Method | Path | Purpose | Success |
|---|---|---|---|
| `POST` | `/schedules` | Create a schedule | `201` + `{id, version}`, `Location` header |
| `GET` | `/schedules` | List the schedule plan | `200` `{items, next_cursor}` |
| `GET` | `/schedules/{id}` | Read one schedule | `200` `Schedule` |
| `PATCH` | `/schedules/{id}` | Edit content and/or time | `200` `{id, version}` |
| `DELETE` | `/schedules/{id}` | Cancel a schedule | `204` |
| `GET` | `/healthz` | Liveness | `200` |
| `GET` | `/readyz` | DB readiness | `200` / `503` |

### Create

```http
POST /api/v1/schedules
Content-Type: application/json
Idempotency-Key: 6f1c2a9e-...

{
  "content": "Shipping the scheduler MVP today.",
  "scheduled_at": "2026-10-22T02:00:00Z",
  "platform": "threads"
}
```

```http
HTTP/1.1 201 Created
Location: /api/v1/schedules/0192f3c4-...

{ "id": "0192f3c4-...", "version": 1 }
```

### List

```http
GET /api/v1/schedules?status=scheduled,failed&from=2026-10-22T00:00:00Z&to=2026-10-29T00:00:00Z&limit=20&order=asc
```

```json
{
  "items": [
    {
      "id": "0192f3c4-...",
      "content": "Shipping the scheduler MVP today.",
      "status": "scheduled",
      "scheduled_at": "2026-10-22T02:00:00Z",
      "platform": "threads",
      "attempts": 0,
      "last_error": null,
      "external_post_id": null,
      "version": 1,
      "created_at": "2026-10-21T18:00:00Z",
      "updated_at": "2026-10-21T18:00:00Z",
      "published_at": null
    }
  ],
  "next_cursor": null
}
```

Pagination is keyset on `(scheduled_at, id)`, encoded as an opaque base64url cursor. `limit`
defaults to 20, max 100. `status` accepts a comma-separated list; `platform` filters by target. Default sort is `scheduled_at ASC`
(imminent first); `order=desc` reverses it.

### Edit / reschedule

```http
PATCH /api/v1/schedules/0192f3c4-...
Content-Type: application/json

{
  "content": "Shipping the scheduler MVP — updated copy.",
  "scheduled_at": "2026-10-22T03:30:00Z",
  "expected_version": 1
}
```

At least one of `content` / `scheduled_at` is required; `expected_version` is always required.
On a `failed` post, `scheduled_at` is required (a reschedule is what revives it); content may be
included in the same request. Returns `{id, version}` with the incremented version. A stale version
returns `409 version_conflict`; an invalid transition (e.g. already publishing) returns `409
invalid_state`.

### Cancel

```http
DELETE /api/v1/schedules/0192f3c4-...?expected_version=2
HTTP/1.1 204 No Content
```

Cancellation is a soft delete: `status=canceled`, excluded from the default list, retained for
audit. Repeating the call returns `204` (idempotent). Published posts return `409 invalid_state`.
`expected_version` is optional; when supplied it is checked.

### Error codes

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | Malformed JSON, bad cursor |
| 401 | `unauthorized` | Missing/wrong bearer token |
| 404 | `not_found` | Unknown schedule id |
| 409 | `version_conflict` | `expected_version` mismatch |
| 409 | `invalid_state` | Transition not allowed from current status |
| 422 | `validation_failed` | Empty/too-long content, past time, non-UTC timestamp |
| 422 | `unsupported_platform` | `platform` is not a registered adapter |
| 500 | `internal_error` | Unexpected failure (logged with `request_id`) |

Full contract: `api/openapi.yaml`.

---

## 8. Configuration

| Env var | Default | Purpose |
|---|---|---|
| `APP_ENV` | `development` | Log format / debug behavior |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_PATH` | `./data/scheduler.db` | SQLite file |
| `API_TOKEN` | — (required) | Static bearer token for `/api/v1` |
| `THREADS_ENABLED` | `false` | Use the real adapter vs `FakePublisher` |
| `THREADS_USER_ID` | — | Threads account id |
| `THREADS_ACCESS_TOKEN` | — | Long-lived token (60 days) |
| `THREADS_API_BASE` | `https://graph.threads.net/v1.0` | Override for tests |
| `THREADS_DAILY_LIMIT` | `240` | Safety cap under the 250/24h quota |
| `SCHEDULER_TICK` | `5s` | Poll interval |
| `SCHEDULER_BATCH` | `10` | Max posts claimed per tick |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Startup fails fast if `API_TOKEN` is empty or `THREADS_ENABLED=true` without credentials.

---

## 9. Testing

| Layer | What | Examples |
|---|---|---|
| Domain (pure) | invariants, transitions, versioning | past time rejected; cannot edit while publishing; cancel published fails |
| Application | handlers with fake repo/clock/publisher | command emits expected events; retryable error requeues with backoff |
| SQLite integration | real temp DB + migrations | repo round-trip; two concurrent claimers, each row claimed once; lease recovery |
| HTTP | `httptest` + real SQLite + fake publisher | create → list → patch → delete end-to-end; problem+json shapes |
| Publisher adapters | `httptest` fake Graph API | prepare→publish flow, ref propagation, 429 → retryable, 400 → permanent |
| Race | `go test -race ./...` in CI | claim loop + concurrent HTTP writes |

Determinism: inject a `Clock` interface everywhere (`Now()`), never call `time.Now()` in domain or
application code. IDs come from an injected generator.

Key regression tests:

- `TestClaim_ConcurrentWorkers_ClaimEachRowOnce`
- `TestPublish_RetryableError_RequeuesWithBackoff`
- `TestPublish_PermanentError_MarksFailed`
- `TestPublish_LeaseExpiry_Recovers`
- `TestUpdate_StaleVersion_Conflict`
- `TestCancel_Published_Rejected`
- `TestList_KeysetPagination_NoDuplicates`

---

## 10. Milestones

Each milestone is independently demoable. Sequence matters; time estimates are intentionally omitted.

| # | Deliverable | Done when |
|---|---|---|
| **M0** | Repo scaffold: `go.mod`, config, slog, health endpoints, goose migrations, Makefile, README | `make run` serves `/healthz`, creates the DB, migrations apply cleanly |
| **M1** | Write side: aggregate, repository, `SchedulePost` handler, `POST /schedules` | curl creates a row + `domain_events` entry + `schedules_read` row |
| **M2** | Read side: `GetSchedule`, `ListSchedules`, projection queries | list filters, ordering, and keyset pagination covered by tests |
| **M3** | Mutations: `PATCH`, `DELETE`, optimistic concurrency | edit/reschedule/cancel work; stale version and bad transitions return 409 |
| **M4** | Scheduler worker + `FakePublisher` + retries + lease recovery | integration test publishes exactly once; restart mid-publish recovers; attempts logged |
| **M5** | Real Threads adapter, error mapping, rate budget | one real text post published to a Threads test account (requires token) |
| **M6** | Hardening: idempotency keys, OpenAPI conformance tests, Dockerfile, runbook, backup cron | `docker run` works; runbook covers config, migrate, backup, restore; `go test -race` green |

Suggested first vertical slice if you want to see something running fast: **M0 + M1 + M2** — create
and list schedules through HTTP with real SQLite, no worker yet.

---

## 11. Risks and open questions

| Risk | Impact | Mitigation |
|---|---|---|
| Threads app review / token not available | Blocks M5, not M0–M4 | Build against `FakePublisher`; get a long-lived token for a test account |
| Duplicate post in crash window | User-visible duplicate | Persist prepare ref before publish, log attempts, reconcile manually; documented as at-least-once |
| Character counting differs (emoji/links) | API rejects otherwise valid content | Validate rune count ≤ 500; surface Meta's error; verify with emoji/link test posts |
| Token expires after 60 days | Publishing stops | Startup check + `/readyz` warning; refresh runbook; later a refresh job |
| SQLite single writer | Write contention under load | Single instance, WAL, one write connection, short transactions; Postgres is the scale-out path |
| Clock/UTC mistakes | Posts at wrong time | UTC only everywhere; offsets rejected at the edge; clock injection in tests |
| No UI | Manual curl only for MVP | Out of scope by request; OpenAPI enables a generated client later |

### Decisions to confirm before build

1. **One Threads account in MVP**, credentials via env — no OAuth flow. (Recommended.)
2. **Real publishing included** in MVP behind `THREADS_ENABLED`, with fake adapter for dev. (Recommended.)
3. **Cancel = soft delete** (`canceled`, retained for audit) rather than hard `DELETE`. (Recommended.)
4. **Static bearer token** for API auth, not user accounts. (Recommended.)
5. **Go module path** `github.com/ahsanulks/social-scheduler` — change if your GitHub handle differs.

Override any of these and the plan adjusts; otherwise M0 starts with the defaults above.

---

## 12. Future extensions (not MVP)

- Media posts (image/video containers + status polling), carousels, reply chains.
- Multiple accounts with OAuth and encrypted token storage.
- Recurring schedules (RRULE) and queue slots ("next free 09:00").
- Webhook/event notifications on publish success/failure.
- Prometheus metrics and tracing.
- Postgres + outbox + separate worker for horizontal scale.
- Web UI or CLI on top of the same API.
