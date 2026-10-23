# Threads API notes (for the adapter)

This is the reference for the **Threads publisher adapter** (`adapters/publishers/threads`), one
implementation of the generic `app.Publisher` port described in `docs/SPEC.md` §7. `Prepare` =
create container, `Publish` = publish container. Nothing here is visible to the domain, the worker,
or the HTTP API.

Source: Meta for Developers — Threads API.
Verify against the live docs before implementing M5; Meta changes quotas and error codes.

- Publishing reference: https://developers.facebook.com/documentation/threads/reference/publishing/
- Posts guide: https://developers.facebook.com/documentation/threads/posts/

## Base

```
https://graph.threads.net/v1.0
```

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

For media posts, poll the container status before publishing:

```bash
curl -sS "https://graph.threads.net/v1.0/$CREATION_ID?fields=status,error_message&access_token=$THREADS_ACCESS_TOKEN"
```

Text-only containers are normally publishable immediately; tolerate a "container not ready" style
error by treating it as retryable.

## Limits

| Limit | Value |
|---|---|
| Text post length | 500 characters |
| API-published posts | 250 per rolling 24h per profile |
| Replies | 1,000 per 24h |
| Deletions | 100 per 24h |
| Carousel items | 20 (not used in MVP) |
| Video length | 5 minutes (not used in MVP) |
| General call budget | a function of impressions; not a constraint at our volume |

Safety cap in the scheduler: `THREADS_DAILY_POST_LIMIT=240`.

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

- Does Meta count links or emoji as one character, or per code point? Validate with a test post
  containing a URL and a ZWJ emoji before advertising an exact 500-char guarantee.
- Does an unpublished container expire? If so, the create→persist→publish window must stay short.
