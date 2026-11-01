# Social Scheduler — Implementation Spec

**Version:** 0.3 (MVP) · **Date:** 2026-10-21
**Stack:** Go · SQLite · CQRS + DDD · REST/JSON · backend only
**First platform:** Threads (via a pluggable publisher adapter)

This is the build contract. If the code and this document disagree, fix one of them before merging.

**Changes in 0.2:** all timestamps are UTC only (no timezone field anywhere); publishing is behind a
platform-agnostic adapter port.
**Changes in 0.3:** each platform's format, limits, and error codes are owned by its adapter and
declared through `Capabilities()`; the domain no longer encodes any platform's rules.
**Changes in 0.4 (media):** schedules carry an optional, immutable media attachment
(https URL + `image`/`video` kind); each adapter declares which kinds it accepts through
`Capabilities().MediaKinds`.

---

## 1. What we are building

A backend service that stores text posts with a publish time and publishes them to a social
platform when due. Clients create, list, edit, and cancel schedules over HTTP. One service
instance, SQLite for storage, no UI. The platform is chosen per schedule; Threads is the first
adapter.

---

## 2. Scope

**In scope**

- Create a schedule: text + publish time (+ target platform), optionally with a media attachment.
- List the schedule plan (filter, sort, paginate).
- Get one schedule.
- Edit content and/or publish time.
- Cancel a schedule.
- Publish due posts automatically through the platform adapter.
- Retry transient publish failures.

**Out of scope**

- Web or mobile UI.
- Multiple attachments per post (carousels).
- Reply chains / multi-post threads.
- Recurring schedules.
- Multiple accounts / OAuth per platform.
- Analytics, approvals, teams.
- Horizontal scaling (one process, one SQLite writer).

---

## 3. Domain

### 3.1 Types

Every type below has unexported fields and a validating constructor. The zero value is invalid.
No raw `string`/`int` crosses the domain API for these concepts.

| Type | Rule |
|---|---|
| `PostID` | canonical lowercase UUID |
| `PostContent` | trimmed, 1–5000 characters (service sanity bound, **not** a platform rule) |
| `ScheduleTime` | non-zero UTC instant, millisecond precision |
| `Platform` | slug `^[a-z][a-z0-9_]{1,31}$` (e.g. `threads`); must be a registered platform at the API edge |
| `Status` | `scheduled`, `publishing`, `published`, `failed`, `canceled` (derived, not stored) |
| `Version` | ≥ 1; grows by one per domain change |
| `Attempt` | ≥ 0; grows by one per publish try |
| `PrepareRef` | opaque provider-scoped reference, non-empty, ≤ 128 chars |
| `Media` | optional attachment: valid https URL (≤ 2048 chars) + kind in `image`, `video`; immutable once set |
| `ExternalPostID` | the platform's post id, non-empty, ≤ 128 chars |
| `FailureReason` | 1–500 chars, truncated above that |

`time.Time` and `time.Duration` are used directly — they already carry their meaning and unit.

### 3.2 Times are UTC, always

- Every instant in the system is UTC. There is no timezone concept anywhere in the code.
- API input: `scheduled_at` must be RFC 3339 **with a `Z` suffix** (UTC). A value with an offset
  (`+07:00`) or a missing zone is rejected with `422 validation_failed`.
- API output: every timestamp is UTC with `Z`.
- Storage: `INTEGER` Unix milliseconds (UTC by definition).
- Clients convert to UTC before calling. The service never converts.

### 3.3 Aggregate

`ScheduledPost` has these fields in every state:

`id`, `content`, `scheduledAt`, `platform`, `version`, `attempts`, `prepareRef`, `createdAt`,
`updatedAt`.

It also has exactly one state variant:

| State | Carries |
|---|---|
| `scheduled` | optional `retry{at, reason}`; no retry means "due as soon as possible" |
| `publishing` | `leaseUntil` |
| `published` | `externalPostID`, `publishedAt` |
| `failed` | `reason`, `failedAt` |
| `canceled` | `canceledAt` |

`Status` is derived from the variant. There is no `status` field on the aggregate.

`prepareRef` lives on the aggregate, not in the state: it must survive a lease recovery back into
`scheduled`. It is an infrastructure artifact (a platform-side draft reference), not a business
fact.

`platform` is set at creation and never changes. Targeting a different platform means creating a
new schedule.

### 3.4 Invariants

1. Content is 1–5000 characters after trimming. Platform limits (500 for Threads, 280 for X, …)
   are enforced at the API edge from the target adapter's `Capabilities()`, never in the domain.
2. `scheduled_at` must be strictly in the future when created or rescheduled.
3. Content can be edited only in `scheduled`.
4. Rescheduling is allowed from `scheduled` and `failed`. From `failed`, it resets attempts and
   returns the post to `scheduled`.
5. Cancel is allowed from `scheduled` and `failed`. Canceling an already-canceled post is a no-op.
   `publishing` and `published` cannot be canceled.
6. Every domain change increments `version`, sets `updated_at`, and appends exactly one event.
7. Only the worker may move a post `scheduled → publishing`.
8. `published` and `canceled` are terminal.
9. `platform` never changes after creation.

### 3.5 Implementation rules

- Domain structs have **no exported fields**. Construct only via `NewScheduledPost` (creation rules)
  or `Rehydrate` (structural rules only).
- `Status` is derived from the state. Never add a `status` field.
- Transitions replace the whole state value; never mutate a shared one.
- `NewScheduledPost` rejects past times. `Rehydrate` does **not** — overdue posts are legitimate.
- Persistence uses a primitives-only `Snapshot`; `Rehydrate` rejects impossible combinations
  (e.g. `published` without an external id).
- The `domain` package imports stdlib only. No JSON tags, no SQL, no HTTP.
- The domain knows nothing about any platform. `Platform` is just a validated slug.

---

## 4. Storage

SQLite, one file. PRAGMAs: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`,
`synchronous=NORMAL`. One write connection; short transactions.

Timestamps are `INTEGER` Unix milliseconds (UTC). IDs are `TEXT`.

### 4.1 `scheduled_posts` (write model)

```sql
CREATE TABLE scheduled_posts (
    id               TEXT PRIMARY KEY,
    content          TEXT    NOT NULL,
    scheduled_at     INTEGER NOT NULL,
    platform         TEXT    NOT NULL,
    status           TEXT    NOT NULL CHECK (status IN
                       ('scheduled','publishing','published','failed','canceled')),
    version          INTEGER NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    prepare_ref      TEXT,
    next_attempt_at  INTEGER,
    last_error       TEXT,
    lease_until      INTEGER,
    external_post_id TEXT,
    published_at     INTEGER,
    failed_at        INTEGER,
    canceled_at      INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX idx_due      ON scheduled_posts(status, scheduled_at);
CREATE INDEX idx_retry    ON scheduled_posts(status, next_attempt_at);
CREATE INDEX idx_created  ON scheduled_posts(created_at DESC);
CREATE INDEX idx_platform ON scheduled_posts(platform, published_at);
```

### 4.2 Supporting tables

- `domain_events` — append-only: `id`, `aggregate_id`, `aggregate_version`, `event_type`,
  `payload` (JSON), `occurred_at`. Unique `(aggregate_id, aggregate_version)`.
- `schedules_read` — projection for queries: `id`, `content`, `status`, `scheduled_at`, `platform`,
  `attempts`, `last_error`, `external_post_id`, `version`, `created_at`, `updated_at`,
  `published_at`. Indexes on `(status, scheduled_at)` and `(scheduled_at, id)`.
- `publish_attempts` — one row per try: `schedule_id`, `platform`, `attempt_no`, `started_at`,
  `finished_at`, `outcome`, `prepare_ref`, `error_code`, `error_msg`.

`Save` writes state + events + projection in **one transaction**, guarded by `version`.

Migrations: goose, embedded in the binary, applied on startup.

---

## 5. HTTP API

Base path `/api/v1`. Auth: `Authorization: Bearer <API_TOKEN>` on every `/api/v1` route.
`/healthz` and `/readyz` are open. Errors are `application/problem+json` with a stable `code`.

### 5.1 Endpoints

| Method | Path | Success | Body |
|---|---|---|---|
| `POST` | `/schedules` | `201` | `{id, version}`, `Location` header |
| `GET` | `/schedules` | `200` | `{items, next_cursor}` |
| `GET` | `/schedules/{id}` | `200` | `Schedule` |
| `PATCH` | `/schedules/{id}` | `200` | `{id, version}` |
| `DELETE` | `/schedules/{id}` | `204` | — |
| `GET` | `/healthz` | `200` | `{status:"ok"}` |
| `GET` | `/readyz` | `200` / `503` | `{status, database}` |
| `GET` | `/platforms` | `200` | `{platforms:[…]}` |

### 5.2 Create

```http
POST /api/v1/schedules
Content-Type: application/json

{
  "content": "Shipping the scheduler MVP today.",
  "scheduled_at": "2026-10-22T02:00:00Z",
  "platform": "threads"
}
```

- `content` required, 1–5000 characters, and must fit the target platform's `MaxTextLength`
  (e.g. 500 for Threads). Exceeding it returns `422 platform_limit_exceeded`.
- `scheduled_at` required, RFC 3339 **UTC with `Z`**, must be in the future.
- `platform` optional; defaults to `threads`. Must be a registered platform, else
  `422 unsupported_platform`.
- Response `201 { "id": "0192…", "version": 1 }`.

### 5.3 List

```http
GET /api/v1/schedules?status=scheduled,failed&platform=threads&from=2026-10-22T00:00:00Z&to=2026-10-29T00:00:00Z&limit=20&order=asc
```

- `status`: comma-separated; default is all statuses **except canceled**.
- `platform`: optional exact match.
- `from` / `to`: `scheduled_at` bounds, `from` inclusive, `to` exclusive.
- `limit`: default 20, max 100.
- `order`: `asc` (default) or `desc` by `scheduled_at`.
- `cursor`: opaque value from `next_cursor`.
- Response `200 { "items": [Schedule…], "next_cursor": null }`.

### 5.4 Read one

`GET /api/v1/schedules/{id}` → `200 Schedule`, or `404 not_found`.

### 5.5 Edit / reschedule

```http
PATCH /api/v1/schedules/{id}
Content-Type: application/json

{
  "content": "Updated copy.",
  "scheduled_at": "2026-10-22T03:30:00Z",
  "expected_version": 1
}
```

- `expected_version` required.
- At least one of `content` / `scheduled_at` required.
- `platform` cannot be changed here.
- On a `failed` post, `scheduled_at` is required; content may be included.
- Response `200 { "id": "0192…", "version": 3 }`.
- Each changed field is one domain change, so version advances by one per change (both fields sent
  ⇒ version +2). The response always returns the final version.

### 5.6 Cancel

```http
DELETE /api/v1/schedules/{id}?expected_version=2
```

- Soft delete: status becomes `canceled`, row is retained.
- `204` on success, and also when the post is already canceled.
- `409 invalid_state` if `publishing` or `published`.
- `409 version_conflict` if `expected_version` is supplied and stale.
- `404` if the id has never existed.

### 5.7 Schedule object

```json
{
  "id": "0192f3c4-…",
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
```

All timestamps in responses are UTC with `Z`.

### 5.8 Errors

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | malformed JSON, bad cursor, unknown status filter |
| 401 | `unauthorized` | missing or wrong bearer token |
| 404 | `not_found` | unknown schedule id |
| 409 | `version_conflict` | `expected_version` mismatch |
| 409 | `invalid_state` | transition not allowed |
| 422 | `validation_failed` | bad content, past time, non-UTC `scheduled_at`, limit out of range |
| 422 | `unsupported_platform` | `platform` is not a registered adapter |
| 422 | `platform_limit_exceeded` | content or payload does not fit the platform's declared capabilities |
| 500 | `internal_error` | unexpected; logged with `request_id` |

---

## 6. Worker

One goroutine inside the API process, ticking every 5 seconds. The flow is identical for every
platform — only the adapter differs.

1. **Recover leases.** Any `publishing` row with `lease_until <= now` goes back to `scheduled` with
   a retry reason "lease expired"; attempts are unchanged.
2. **Claim.** Select up to `SCHEDULER_BATCH` rows where `status='scheduled'`, `scheduled_at <= now`,
   and `next_attempt_at` is null or ≤ now, ordered by `scheduled_at`. For each row: load the
   aggregate, call `StartPublishing` (sets `status='publishing'`, takes the lease, bumps the
   version), and save it. The save is guarded by the version the aggregate was loaded with, so when
   two workers race, one wins and the loser gets a version conflict and skips the post. Claiming
   through the domain keeps every row loadable: a SQL-only claim writes a `scheduled` row carrying a
   lease, which `Rehydrate` refuses by design.
3. **Rate budget per platform.** Count posts published for that platform in the last 24h. If the
   adapter's `Capabilities().DailyLimit` is exceeded, skip that platform's posts this tick.
4. **Resolve the adapter.** Look up the platform in the registry. If it is not registered (e.g. it
   was disabled), the post fails permanently with reason "platform not configured".
5. **Publish each claimed post:**
   1. `StartPublishing` (attempt + 1).
   2. If the post has no `prepare_ref`: call `Prepare(content)` and persist the returned reference
      immediately.
   3. Call `Publish(content, prepare_ref)` → external post id.
   4. `MarkPublished` (status `published`, `published_at`, `external_post_id`).
6. **On failure:**
   - Retryable (adapter says so): attempts < 5 → back to `scheduled`, `next_attempt_at` =
     now + 1m / 5m / 15m / 60m, `last_error` set. Otherwise `failed`.
   - Permanent → `failed` immediately.
7. **Record** one `publish_attempts` row per try, with the platform, outcome, and error.

Late posts still publish after downtime. Precision is the tick interval, not seconds.

Publishing is **at-least-once**: a crash between the platform accepting the post and the DB commit
can duplicate a post. The prepare reference is persisted before publishing to narrow the window;
this is documented, not solved.

---

## 7. Publisher adapters (the platform seam)

### 7.1 The port

Defined in `app`, implemented in `adapters/publishers/<name>`:

```go
// app/publisher.go
package app

// Publication is what an adapter is asked to publish. It grows as the domain
// grows (media, replies, …) without changing the shape for existing adapters.
type Publication struct {
	Platform  domain.Platform
	Content   string
	MediaURL  string // empty for text-only posts
	MediaKind string // domain media kind; adapters map it to their own payload key
}

type Publisher interface {
	// Platform reports which platform this adapter serves.
	Platform() domain.Platform

	// Capabilities declares what this platform accepts, so the API can reject
	// impossible schedules up front instead of failing later at publish time.
	Capabilities() Capabilities

	// Prepare validates the publication for this platform and creates whatever
	// platform-side draft is needed, returning an opaque reference. Returning
	// nil means "nothing to prepare" — valid for single-call adapters.
	// A publication this platform cannot express is a Permanent error.
	Prepare(ctx context.Context, pub Publication) (*domain.PrepareRef, error)

	// Publish commits the prepared post and returns the platform's post id.
	// ref is nil when the adapter had nothing to prepare.
	Publish(ctx context.Context, pub Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error)
}

// Capabilities is each adapter's declaration of its own format's rules.
type Capabilities struct {
	MaxTextLength int  // 0 = unknown; threads 500, x 280, linkedin 3000
	RequiresMedia bool // true for platforms that cannot post text alone
	SupportsText  bool
	PreparesDraft bool // true when Prepare creates a platform-side draft
	DailyLimit    int  // posts allowed per rolling 24h; 0 = unknown

	TextLengthInBytes bool // threads counts the 500 limit in UTF-8 bytes (emoji are multi-byte)
	MediaKinds        []string // domain media kinds the adapter accepts; empty = text-only
}
```

### 7.2 Registry

```go
type Registry struct{ byPlatform map[domain.Platform]Publisher }

func NewRegistry(publishers ...Publisher) (*Registry, error) // duplicate platform → error
func (r *Registry) Resolve(p domain.Platform) (Publisher, bool)
func (r *Registry) Platforms() []domain.Platform
```

Wired in `cmd/api/main.go`. An adapter is registered only when its credentials are configured; an
unregistered platform is rejected at the API edge with `unsupported_platform`.

### 7.3 Error classification (the adapter's job)

```go
type PublishError struct {
	Retryable bool
	Reason    string
	Err       error
}

func Retryable(err error) error  // wraps as retryable
func Permanent(err error) error  // wraps as permanent
func IsRetryable(err error) bool // unknown errors default to retryable
```

The worker never inspects HTTP status codes — adapters translate platform errors into
`Retryable` / `Permanent`.

### 7.4 Threads adapter (first implementation)

```http
POST https://graph.threads.net/v1.0/{user-id}/threads
     media_type=TEXT, text=…, access_token=…
     → {"id": "<container-id>"}          ← Prepare

POST https://graph.threads.net/v1.0/{user-id}/threads_publish
     creation_id=<container-id>, access_token=…
     → {"id": "<post-id>"}               ← Publish
```

- Text limit 500 characters, counted in UTF-8 bytes — emoji count as their byte length
  (`Capabilities().TextLengthInBytes = true`); account limit 250 posts per rolling 24h
  (`Capabilities().DailyLimit` = 240 as a safety margin).
- Base URL default `https://graph.threads.net/v1.0`.
- Retryable: network errors, HTTP 429, HTTP 5xx, "container not ready".
- Permanent: HTTP 400/403, invalid parameters, HTTP 401 / code 190 (token).
- Log Meta's `fbtrace_id` from error responses.
- Details: `docs/THREADS_API_NOTES.md`.

### 7.5 Facebook Pages adapter

```http
POST https://graph.facebook.com/{page-id}/feed
     message=…, access_token=<page access token>
     → {"id": "<post-id>"}                ← Publish (single call; Prepare does nothing)
```

- Text only for v1: no `link=`, no media (`Capabilities().MediaKinds` is empty, so the API edge
  rejects attachments with `unsupported_media_type`).
- No documented Graph limit on a feed message; `MaxTextLength` = 63,206 (the composer ceiling,
  defense in depth behind the domain's 5000-character bound). No post quota → `DailyLimit` = 0.
- Requires a Page access token with `pages_manage_posts` + `pages_read_engagement`.
- Errors share the Graph envelope with Threads; classification lives in the `graphapi` package.

### 7.6 Instagram adapter

```http
POST https://graph.facebook.com/{ig-user-id}/media
     image_url=…, caption=…, access_token=<user access token>
     → {"id": "<container-id>"}          ← Prepare, step 1

GET  https://graph.facebook.com/{ig-user-id}/{container-id}?fields=status_code
     → {"status_code": "FINISHED"}       ← Prepare, step 2 (bounded poll)

POST https://graph.facebook.com/{ig-user-id}/media_publish
     creation_id=<container-id>, access_token=…
     → {"id": "<media-id>"}              ← Publish
```

- Media is required (`Capabilities().RequiresMedia = true`); v1 is images only —
  `MediaKinds` = `["image"]` (`domain.MediaKindImage`), and the adapter rejects any other kind
  at prepare time as defense in depth.
- Captions max 2,200 characters, counted in characters (`MaxTextLength` = 2200); rune-counted
  until a live test proves emoji counting is byte-based like Threads
  (`TextLengthInBytes` stays false).
- Account limit 100 API-published posts per rolling 24h, enforced on `media_publish`; the
  adapter caps itself at `DailyLimit` = 90 (safety margin).
- The prepare phase waits for `FINISHED` with a bounded poll (~15s first poll, ~15s interval,
  ≤90s total, inside the default 2-minute publish lease). Still `IN_PROGRESS` at the deadline
  is retryable: the worker requeues with backoff, and an unpublished container is valid for
  24h. Container `ERROR`/`EXPIRED` is permanent.
- Requires a Facebook *user* access token with `instagram_basic` + `instagram_content_publish`
  (a Page token does not work), a professional account linked to a Page, and Page Publishing
  Authorization.
- Errors share the Graph envelope and classification with Threads/Facebook (`graphapi`).

### 7.7 Other adapters

- `fake` — in-memory publisher used by tests and local dev; always succeeds, records what it was
  asked to publish.
- Future: X, LinkedIn, Mastodon, or any HTTP API that can create-then-publish or publish
  in one call.

**Adding a platform must not require changes to the domain, the worker, or the HTTP API.** Only: a
new package under `adapters/publishers/`, a registration line in `main`, and its own env vars.

### 7.8 Where platform differences live

Every platform has a different payload shape, limits, and error codes. All of it lives inside that
platform's adapter package — nothing platform-specific reaches the domain, the worker, or the API.

```
adapters/publishers/threads/
  publisher.go   # Platform, Capabilities, Prepare, Publish
  client.go      # the platform's own HTTP calls and request/response structs
  errors.go      # platform error codes → Retryable / Permanent
  publisher_test.go
```

How the formats differ, and therefore what each adapter owns:

| Platform | Its own format | The adapter owns |
|---|---|---|
| Threads | text only, 500 chars, two-step container → publish | container creation, the 500-char rule, Meta error codes |
| Facebook | text only for v1, single-call publish | the endpoint param shape, the 63,206 composer bound |
| Instagram | media required, caption ≤ 2200 chars, image containers | media container + status polling, caption rule, "no text-only post" |
| X | text ≤ 280 chars, optional media, its own buckets | payload shape, the 280-char rule, rate buckets |
| LinkedIn / Mastodon | different payloads and limits again | ditto |

Two rules follow from that split:

1. **Anything checkable from the neutral `Publication` is declared in `Capabilities()`** and
   validated at the API edge. A 900-character post for Threads is rejected with
   `422 platform_limit_exceeded` when it is scheduled — not silently queued to fail later.
2. **Everything else the platform rejects is discovered in `Prepare`**, which returns a `Permanent`
   error; the post ends up `failed` with the platform's message in `last_error`.

The domain keeps one service-wide sanity bound on content length (5000) and knows nothing about any
platform's rules.

Adding a platform = those files + one registration line. No changes to the domain, the worker, or
the HTTP API.

---

## 8. Configuration

Core:

| Env var | Default | Required |
|---|---|---|
| `HTTP_ADDR` | `:8080` | no |
| `DATABASE_PATH` | `./data/scheduler.db` | no |
| `API_TOKEN` | — | **yes** |
| `SCHEDULER_TICK` | `5s` | no |
| `SCHEDULER_BATCH` | `10` | no |
| `LOG_LEVEL` | `info` | no |
| `DEFAULT_PLATFORM` | `threads` | no |
| `PUBLISH_LEASE` | `2m` | no |
| `MAX_PUBLISH_ATTEMPTS` | `5` | no |
| `FAKE_PLATFORM_ENABLED` | `false` | no |
| `FAKE_PLATFORM_NAME` | `fake` | no |

Per adapter (each adapter owns its namespace; an adapter with missing credentials is not
registered):

| Env var | Default | Required |
|---|---|---|
| `THREADS_ENABLED` | `false` | no |
| `THREADS_USER_ID` | — | when enabled |
| `THREADS_ACCESS_TOKEN` | — | when enabled |
| `THREADS_API_BASE` | `https://graph.threads.net/v1.0` | no |
| `THREADS_DAILY_LIMIT` | `240` | no |
| `FACEBOOK_ENABLED` | `false` | no |
| `FACEBOOK_PAGE_ID` | — | when enabled |
| `FACEBOOK_PAGE_TOKEN` | — | when enabled (Page access token) |
| `FACEBOOK_API_BASE` | `https://graph.facebook.com` | no |
| `INSTAGRAM_ENABLED` | `false` | no |
| `INSTAGRAM_USER_ID` | — | when enabled (the IG account id, not the Page id) |
| `INSTAGRAM_ACCESS_TOKEN` | — | when enabled (user token with `instagram_basic` + `instagram_content_publish`) |
| `INSTAGRAM_API_BASE` | `https://graph.facebook.com` | no |
| `INSTAGRAM_DAILY_LIMIT` | `90` | safety margin under the 100/24h quota |

Startup fails fast on missing required config.

---

## 9. Code layout

```
cmd/api/main.go
internal/scheduling/
  domain/      types, state, aggregate, snapshot, events, errors, repository ports
  app/         one file per command/query + ports (Clock, IDGenerator, Publisher, Registry)
  adapters/
    sqlite/    write repo, claim, projector, read repo, migrations
    publishers/
      threads/ Threads adapter
      facebook/ Facebook Pages adapter
      graphapi/ shared Meta Graph error envelope + classification
      fake/    in-memory adapter (tests, dev)
    httpapi/   router, handlers, DTOs, problem+json
  scheduler/   worker
internal/platform/  config · clock · idgen
migrations/
api/openapi.yaml
```

Rules: `domain` imports stdlib only; `app` imports `domain`; adapters import both; `cmd` wires.

---

## 10. Acceptance criteria

The MVP is done when all of these pass:

- [x] `POST /schedules` with a future UTC time returns `201` and the row is visible via `GET`.
- [x] `POST` with empty content, a past time, or a non-UTC timestamp returns `422`.
- [x] `POST` with an unknown `platform` returns `422 unsupported_platform`.
- [x] `POST` with content longer than the target platform's `MaxTextLength` returns
      `422 platform_limit_exceeded` (and the domain does not hardcode any platform's number).
- [x] `GET /schedules` filters by status/platform/date, sorts, and paginates without duplicates.
- [x] `PATCH` changes content and/or time; a stale `expected_version` returns `409`.
- [x] `DELETE` returns `204`; the post leaves the default list; repeating returns `204`.
- [x] `DELETE` on a published post returns `409`.
- [x] The worker publishes a due post through the fake adapter, setting `status=published` and an
      `external_post_id`.
- [x] A second test adapter registered under another platform receives its own posts — proving
      dispatch is by platform, with no change to domain or worker code.
- [x] A simulated retryable error returns the post to `scheduled` with `next_attempt_at` set; a
      permanent error marks it `failed`.
- [x] Killing the process mid-publish and restarting recovers the row via the lease sweep.
- [x] `go test -race ./...` is green.
- [x] Migrations apply cleanly to an empty database.
- [x] `api/openapi.yaml` matches the handlers (contract test or review).

---

## 11. Build order

| Step | Deliverable |
|---|---|
| M0 | Scaffold: `go.mod`, config, logging, health endpoints, migrations, Makefile |
| M1 | Create: domain types + aggregate, repository, `POST /schedules` |
| M2 | Read: projection, `GET /schedules`, `GET /schedules/{id}` |
| M3 | Mutate: `PATCH`, `DELETE`, optimistic concurrency |
| M4 | Publisher port + registry + fake adapter; worker: claim, prepare, publish, retries, lease recovery |
| M5 | Threads adapter: real Graph API calls, error mapping, rate budget |
| M6 | Hardening: idempotency keys, Dockerfile, runbook, contract test |

Each step ends with tests for that step only; later steps do not change earlier behaviour.
Adding a second platform later is: new adapter package + one registration line.

---

## 12. Decisions already made

- All timestamps are UTC. No timezone field, no offsets accepted, no conversion in the service.
- Each platform's payload format, limits, and error codes live in its own adapter package and are
  declared through `Capabilities()`. The neutral `Publication` is all the flow ever passes around.
- Publishing goes through the `Publisher` port, resolved per schedule by `platform`.
- The prepare/publish split is generic: adapters that need a platform-side draft return a
  `PrepareRef`; adapters that publish in one call return nil.
- Persistence boundary: `Snapshot` + `Rehydrate` (aggregate stays fully private).
- Cancel is a soft delete.
- One account per platform, credentials from env.
- Static bearer token for API auth.
- Publishing is at-least-once, with a documented duplicate window.
- SQLite, single instance, one writer.

---

## 13. Implementation status

Built and covered by the end-to-end suite (`e2e/`, `go test -race ./...`):

- Domain, CQRS write/read split, SQLite adapters, migrations, HTTP API, worker, fake and Threads
  adapters, platform registry, rate budget, retries, lease recovery, `GET /platforms`.
- Platform edge validation follows each adapter's declared counting unit.
  Threads counts its 500-character limit in UTF-8 bytes (emoji are multi-byte), verified
  against the live docs and pinned by `TestCreateValidation_CountsTextInBytes`.
- The Threads adapter is exercised against a stubbed Graph API (two-call sequence, reference
  reuse on retry, 429 → retryable, 400 → permanent). It has not been pointed at the live API yet —
  that needs `THREADS_USER_ID` and `THREADS_ACCESS_TOKEN`, and it will create a real post.
- The Facebook Pages adapter (text-only, single call, no draft) and the Instagram adapter
  (media-required image posts with a bounded container-status poll in Prepare) share the same
  stub-e2e treatment at `e2e/facebook_adapter_test.go` and `e2e/instagram_adapter_test.go`.
  `graphapi.Classify` maps errors identically for all three Meta adapters. Neither has been
  pointed at the live Graph API yet (`FACEBOOK_*` / `INSTAGRAM_*` env vars); that is M10.

Deferred (M6): idempotency keys on create, Dockerfile, ops runbook, contract test between
`api/openapi.yaml` and the handlers, and the manual reconciliation path for the at-least-once
duplicate window.
