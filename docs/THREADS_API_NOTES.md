# Threads API notes (for the adapter)

This is the reference for the **Threads publisher adapter** (`adapters/publishers/threads`), one
implementation of the generic `app.Publisher` port described in `docs/SPEC.md` §7. `Prepare` =
create container, `Publish` = publish container. Nothing here is visible to the domain, the worker,
or the HTTP API.

Source: Meta for Developers — Threads API (the docs moved from
`developers.facebook.com/docs/threads/*` to
`developers.facebook.com/documentation/threads/*`; every topic also serves raw
markdown at `<topic>.md/`). Re-verify against the live docs before relying on
numbers here; Meta changes quotas and counting rules. Verified against the live
site on 2026-10-31.

- Publishing reference: https://developers.facebook.com/documentation/threads/reference/publishing/
- Posts guide: https://developers.facebook.com/documentation/threads/posts
- Overview (rate limits): https://developers.facebook.com/documentation/threads/overview
- Troubleshooting (container status): https://developers.facebook.com/documentation/threads/troubleshooting

## Base

```
https://graph.threads.net/v1.0
```

Both `graph.threads.net` and `graph.threads.com` are officially valid hosts;
newer doc examples use `.com`, either works.

Auth: a Threads user access token (long-lived, ~60 days) passed as `access_token`.
Required scopes for text posting: `threads_basic`, `threads_content_publish`.
App Review is required for accounts outside the app's tester list — for MVP testing, add the
target account as a tester.

## Two-step publish (text only)

Step 1 — create a container:

```bash
curl -sS -X POST "https://graph.threads.net/v1.0/$THREADS_USER_ID/threads" \
  -d "media_type=TEXT" \
  -d "text=First post from the scheduler" \
  -d "access_token=$THREADS_ACCESS_TOKEN"
# -> {"id": "18000000000000000"}
```

Step 2 — publish it:

```bash
curl -sS -X POST "https://graph.threads.net/v1.0/$THREADS_USER_ID/threads_publish" \
  -d "creation_id=18000000000000000" \
  -d "access_token=$THREADS_ACCESS_TOKEN"
# -> {"id": "18000000000000001"}
```

Container status endpoint (media posts, or whenever publish errors):

```bash
curl -sS "https://graph.threads.net/v1.0/$CREATION_ID?fields=status,error_message&access_token=$THREADS_ACCESS_TOKEN"
# status: IN_PROGRESS | FINISHED | PUBLISHED | EXPIRED | ERROR
```

`EXPIRED` confirms an unpublished container is discarded after 24h, so our short
create→persist→publish window is safe. Meta recommends polling at most once per
minute, for no more than 5 minutes.

## Limits

| Limit | Value |
|---|---|
| Text post length | 500 characters, **counted in UTF-8 bytes** — emoji count as their byte length (verified sentence: "For the post character limit, emojis are counted as the number of UTF-8 bytes") |
| API-published posts | 250 per rolling 24h per profile (enforced server-side on `threads_publish`; the docs tell scheduling apps to enforce it client-side too) |
| Replies | 1,000 per 24h |
| Deletions | 100 per 24h |
| Carousel items | 20, minimum 2 (not used in MVP) |
| Video length | 5 minutes (not used in MVP) |
| General call budget | `4800 × impressions` per 24h; not a constraint at our volume |

Consequences for the adapter:

- The Threads edge validation counts UTF-8 bytes (`Capabilities.TextLengthInBytes`),
  not runes — a rune-based check would accept posts Meta rejects with 400.
- Live quota is also queryable:
  `GET /{user-id}/threads_publishing_limit?fields=quota_usage,config` →
  `quota_usage`, `config.quota_total` (250), `config.quota_duration` (86400).
  The scheduler currently counts its own published rows (`THREADS_DAILY_LIMIT=240`,
  safety margin under 250), which satisfies Meta's client-side guidance.

Also on the container-create endpoint (text posts, optional, not used in MVP):

- `auto_publish_text=true` publishes a text post in one call — no `threads_publish`
  step, and it eliminates the create→publish duplicate-post window for text posts.
- Meta recommends waiting ~30 seconds between container creation and
  `threads_publish` in general; for text-only containers our immediate publish has
  worked against the stub, but tolerate "container not ready" as retryable.

Safety cap in the scheduler: `THREADS_DAILY_LIMIT=240`.

Token refresh (run manually / future job):

```bash
curl -sS "https://graph.threads.net/refresh_access_token?grant_type=th_refresh_token&access_token=$THREADS_ACCESS_TOKEN"
```

## Error shape

```json
{
  "error": {
    "message": "…",
    "type": "OAuthException",
    "code": 190,
    "error_subcode": 463,
    "fbtrace_id": "…"
  }
}
```

Adapter classification (verify codes against current docs before relying on them):

| Condition | Class | Scheduler behavior |
|---|---|---|
| Network error, timeout, HTTP 429, HTTP 5xx | retryable | requeue with backoff, max 5 attempts |
| HTTP 400/403, invalid parameter, permission error | permanent | `failed`, keep `last_error` |
| Token invalid/expired (401, code 190) | permanent | `failed` + alert; needs a new token |
| Container not ready | retryable | requeue with short backoff |

Log `fbtrace_id` on every failure — it is what Meta support asks for.

## Verification checklist for M5

1. Create a text container by hand and confirm a container id comes back.
2. Publish it and confirm the returned id is a real post on the account.
3. Confirm `GET /{post-id}?fields=id,permalink` resolves the published post.
4. Schedule one post through the API and let the worker publish it end to end.
5. Force a retryable failure (point `THREADS_API_BASE` at a stub returning 500) and confirm the
   post returns to `scheduled` with backoff.
6. Force a permanent failure (empty content at the adapter boundary is not possible; use a stub
   returning 400) and confirm `failed` + `last_error`.

## Open questions

Closed by the current docs: an unpublished container expires after 24 hours
(`EXPIRED` status), and the 500-character limit is counted in UTF-8 bytes.

Remaining:

- Links and normal multi-byte text: confirm a real post containing a URL and a
  mixed-script body is accepted, before advertising an exact 500-byte guarantee.
- Whether Meta recommends the ~30s wait for text containers too, or only media.
  Our immediate publish is standard for text; revisit only if live testing shows
  "container not ready" failures.
