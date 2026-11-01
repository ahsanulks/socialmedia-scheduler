# social-scheduler

Backend-only scheduler for social media posts. **MVP targets Threads.**

**Status:** MVP implemented — create / list / get / edit / cancel schedules, plus the worker that
publishes due posts through a platform adapter. End-to-end tests are green
(`go test -race ./...`). Not yet run against the live Threads API.

## Docs

| Doc | Contents |
|---|---|
| [docs/SPEC.md](docs/SPEC.md) | **Build contract** — the implementation spec the code follows |
| [docs/PLAN.md](docs/PLAN.md) | MVP plan: domain model, CQRS architecture, data model, publish pipeline, milestones |
| [docs/CODE_STRUCTURE.md](docs/CODE_STRUCTURE.md) | Package layout, domain encapsulation (no exported fields, value objects, state ADT), persistence boundary |
| [docs/adr/0001-architecture-baseline.md](docs/adr/0001-architecture-baseline.md) | Why CQRS without event sourcing, SQLite single-writer, in-process worker |
| [docs/adr/0002-strict-domain-types.md](docs/adr/0002-strict-domain-types.md) | Why every domain concept is a smart-constructed type, and why status is derived from a sealed state ADT |
| [docs/THREADS_API_NOTES.md](docs/THREADS_API_NOTES.md) | Threads Graph API: endpoints, limits, error mapping |
| [api/openapi.yaml](api/openapi.yaml) | HTTP contract |

## Stack

Go (1.27) · SQLite via `modernc.org/sqlite` (pure Go, WAL) · CQRS + DDD · REST/JSON ·
stdlib `net/http` + `log/slog`.

## Run it

```bash
cp .env.example .env      # or export the variables yourself
make run                  # API + worker on :8080
make test                 # end-to-end suite with -race
```

Everything in the system is UTC: `scheduled_at` must be RFC 3339 ending in `Z`, and offsets are
rejected with `422`.

| Env var | Default | Notes |
|---|---|---|
| `API_TOKEN` | — | **required**; bearer token for `/api/v1` |
| `HTTP_ADDR` | `:8080` | |
| `DATABASE_PATH` | `./data/scheduler.db` | |
| `DEFAULT_PLATFORM` | `threads` | platform used when a request omits it |
| `SCHEDULER_TICK` | `5s` | worker poll interval |
| `SCHEDULER_BATCH` | `10` | max posts claimed per tick |
| `PUBLISH_LEASE` | `2m` | how long a publish may hold a row before it is requeued |
| `MAX_PUBLISH_ATTEMPTS` | `5` | attempts before a retryable failure becomes permanent |
| `LOG_LEVEL` | `info` | |
| `THREADS_ENABLED` | `false` | register the Threads adapter |
| `THREADS_USER_ID` / `THREADS_ACCESS_TOKEN` | — | required when Threads is enabled |
| `THREADS_API_BASE` | `https://graph.threads.net/v1.0` | |
| `THREADS_DAILY_LIMIT` | `240` | safety margin under the 250/24h quota |
| `FACEBOOK_ENABLED` | `false` | register the Facebook Pages adapter |
| `FACEBOOK_PAGE_ID` / `FACEBOOK_PAGE_TOKEN` | — | required when Facebook is enabled (Page access token, not a user token) |
| `FACEBOOK_API_BASE` | `https://graph.facebook.com` | pin a version in production (e.g. `https://graph.facebook.com/v23.0`) |
| `INSTAGRAM_ENABLED` | `false` | register the Instagram adapter (image posts) |
| `INSTAGRAM_USER_ID` / `INSTAGRAM_ACCESS_TOKEN` | — | required when Instagram is enabled (user token with `instagram_basic` + `instagram_content_publish`) |
| `INSTAGRAM_API_BASE` | `https://graph.facebook.com` | pin a version in production |
| `INSTAGRAM_DAILY_LIMIT` | `90` | safety margin under the 100/24h publish quota |
| `FAKE_PLATFORM_ENABLED` | `false` | register the in-memory adapter (local dev) |
| `FAKE_PLATFORM_NAME` | `fake` | platform slug for the fake adapter |

Local development without Meta credentials:

```bash
export API_TOKEN=dev-token FAKE_PLATFORM_ENABLED=true FAKE_PLATFORM_NAME=threads
make run

curl -sS -X POST localhost:8080/api/v1/schedules \
  -H "Authorization: Bearer dev-token" -H 'Content-Type: application/json' \
  -d '{"content":"hello from the dev box","scheduled_at":"2026-10-22T02:00:00Z"}'
```

## API in one screen

| Method | Path | Result |
|---|---|---|
| `POST` | `/api/v1/schedules` | `201 {id, version}` |
| `GET` | `/api/v1/schedules` | `200 {items, next_cursor}` — filter by `status`, `platform`, `from`, `to`; `order`, `limit`, `cursor` |
| `GET` | `/api/v1/schedules/{id}` | `200` schedule |
| `PATCH` | `/api/v1/schedules/{id}` | `200 {id, version}` — needs `expected_version` |
| `DELETE` | `/api/v1/schedules/{id}` | `204` — soft delete |
| `GET` | `/api/v1/platforms` | `200` registered platforms and their capabilities |
| `GET` | `/healthz`, `/readyz` | liveness / readiness (no auth) |

## Layout

```
cmd/api            entrypoint
internal/          domain · app · adapters (sqlite, httpapi, publishers/{threads,fake}) · scheduler
migrations/        embedded SQL
e2e/               end-to-end tests (there are no unit tests)
api/openapi.yaml   HTTP contract
```

Adding a platform is a new package under `internal/scheduling/adapters/publishers/<name>` plus one
registration line in `cmd/api/main.go`. Nothing in the domain, the worker, or the HTTP API changes.

## Testing

`e2e/` drives the whole service: real HTTP against `httptest`, a real SQLite file, the real worker,
and the real Threads adapter against a stubbed Graph API. Covered: create/validate/list/paginate,
optimistic concurrency, soft delete, terminal-state guards, publish success, retryable and permanent
failures, platform dispatch, the per-platform daily budget, lease recovery after a simulated crash,
and container reuse on retry.
