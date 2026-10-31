# Plan — Facebook + Instagram adapters

**Status:** draft for review · **Last updated:** 2026-10-31
**Prerequisite:** Threads adapter (M0–M6) complete; this plan adds two platforms behind the same
`app.Publisher` port. Facts below verified against the live Meta docs on 2026-10-31.

---

## 1. What the docs say (verified)

### Facebook Pages (Pages API)

| Fact | Value |
|---|---|
| Endpoint | `POST https://graph.facebook.com/v26.0/{page-id}/feed` with `message`, optional `link` |
| Auth | **Page access token** (derived from a long-lived user token), permissions `pages_manage_posts`, `pages_read_engagement` (+`pages_manage_engagement`, `pages_read_user_engagement`; `publish_video` for video) |
| Text limit | No documented maximum in the Graph reference; the composer enforces 63,206 characters. Treat as generous; surface Meta's error if lower |
| Rate limit | No published-post quota like Threads/IG; Graph API rate limiting is call-based (impressions × 4800/24h, same family as Threads). No client-side post cap needed |
| Publish model | **Single call** — no container/draft step. `Prepare` returns nil |
| Link posts | `link=<URL>` on the same endpoint |
| Native scheduling | `published=false` + `scheduled_publish_time` exists (10 min–30 days ahead). **Not used**: our worker publishes at due time like every other platform, so one publish path, one audit trail |

Docs: `developers.facebook.com/documentation/pages-api/posts`, `.../pages-api/getting-started`.

### Instagram (Instagram Platform / Graph API)

| Fact | Value |
|---|---|
| Endpoints | Create container: `POST /{ig-user-id}/media` (`image_url` or `video_url`, optional `caption`); publish: `POST /{ig-user-id}/media_publish` (`creation_id`); status: `GET /{container-id}?fields=status_code` |
| Auth | **Facebook user access token** with `instagram_basic` + `instagram_content_publish` (+`pages_read_engagement`). A Page token does **not** work for IG endpoints |
| Account | Professional (Business/Creator) account connected to a Facebook Page; Page Publishing Authorization (PPA) may be required or publishing fails |
| Text limit | Caption max **2,200 characters** (also max 30 hashtags, 20 @-tags). Docs say "characters", not bytes — verify emoji counting with a live test |
| Media | **Required** — there are no text-only Instagram posts. Images: JPEG only, must be on a public server. Video via `video_url` |
| Rate limit | **100 API-published posts / rolling 24h**, enforced on `media_publish`; container creation capped at 400/24h. `GET /{ig-user-id}/content_publishing_limit` returns live usage |
| Container | Expires after 24h if unpublished; status `IN_PROGRESS`/`FINISHED`/`PUBLISHED`/`EXPIRED`/`ERROR`; poll ~1/min for ≤5 min |

Docs: `developers.facebook.com/documentation/instagram-platform/content-publishing`,
`.../instagram-platform/instagram-graph-api/reference/ig-user/media`,
`.../reference/ig-user/content_publishing_limit`.

### Consequences for our design

- Facebook is a drop-in second adapter: text (+optional link), single-call publish, no rate cap.
  It only needs the existing port — like Threads, minus the draft step.
- Instagram needs **media support in the core**, which the current spec explicitly deferred
  (`checkCapabilities` rejects `RequiresMedia && !SupportsText` with "media not scheduled yet").
  That core change is milestone M7 below; it must not change behavior for text-only platforms.

---

## 2. Core changes (M7 — the only shared-code milestone)

### Domain

New value object, kept out of `PostContent` (text and media are different concepts):

```
Media { url, kind }     // url: https only, public; kind: image | video
```

- `ScheduledPost` gains an optional `media *Media` (nil = text-only, unchanged for Threads/FB).
- Media is set at creation and **immutable afterward** (edit content/time only; rescheduling a
  failed post may not change media — keeps `Rehydrate` and the read model simple).
- Migration `0002`: `ALTER TABLE scheduled_posts ADD COLUMN media_url TEXT; ADD COLUMN media_kind TEXT;`
  same for `schedules_read`. NULL everywhere for existing rows.

### Application / port

- `Publication` gains `MediaURL string`, `MediaType string` (empty for text-only).
- `Capabilities` gains `MediaKinds []string` (e.g. `{"image","video"}`); drops nothing.
- `checkCapabilities`: replace the blanket "media not scheduled yet" rejection with
  - platform requires media and none provided → `422 media_required`;
  - media kind not in `MediaKinds` → `422 unsupported_media_type`;
  - media provided to a text-only platform (Threads, FB text) → `422 unsupported_media_type`.

### HTTP API

- `POST /schedules` accepts optional `"media": {"url": "...", "kind": "image"}`.
- `GET /platforms` reports `media_kinds` (empty list for text-only platforms).
- OpenAPI updated; `PATCH` still only edits `content` / `scheduled_at`.

Worker, claim flow, retries, lease recovery: unchanged. Media rides along in the aggregate.

---

## 3. Adapter plans

### Facebook (`adapters/publishers/facebook`) — M8

- `Prepare` returns `nil` (single call; `PreparesDraft: false`), `Publish` posts
  `{page-id}/feed` with `message=content`, `link=` (when the post carries a link — v1: only if
  the content's media/URL story is a plain link; otherwise `message` only), `access_token`.
  Returns the post id (`{post_id}_{story_id}` shape tolerated as an opaque `ExternalPostID`).
- Capabilities: `MaxTextLength: 63206` (composer ceiling, not a documented Graph limit),
  `DailyLimit: 0` (unknown — no post quota), `SupportsText: true`.
- Error mapping: reuse the Graph error shape (`classify` logic is shared-shape with Threads —
  consider lifting `apiError`+`classify` into a small `adapters/publishers/graphapi` helper
  package so both adapters map 429/5xx → retryable, 4xx/190 → permanent).
- Config: `FACEBOOK_ENABLED`, `FACEBOOK_PAGE_ID`, `FACEBOOK_PAGE_TOKEN`,
  `FACEBOOK_API_BASE` (default `https://graph.facebook.com`).

### Instagram (`adapters/publishers/instagram`) — M9

- `Prepare`: `POST /{ig-user-id}/media` with `image_url`/`video_url` + `caption` → container id,
  then **bounded poll** of `GET /{container-id}?fields=status_code` (~15s initial, ≤90s total,
  fits inside the 2m lease) until `FINISHED`; container `ERROR`/`EXPIRED` → permanent;
  still `IN_PROGRESS` at the deadline → retryable "container not ready" (worker requeues with
  backoff; the 24h container expiry makes a slow retry safe).
- `Publish`: `POST /{ig-user-id}/media_publish` with `creation_id` → media id.
- Capabilities: `MaxTextLength: 2200` (characters, rune-counted; verify emoji semantics live),
  `MediaKinds: {"image","video"}`, `RequiresMedia: true`, `PreparesDraft: true`,
  `DailyLimit: 90` (safety margin under the documented 100/24h).
- Config: `INSTAGRAM_ENABLED`, `INSTAGRAM_USER_ID`, `INSTAGRAM_ACCESS_TOKEN`,
  `INSTAGRAM_API_BASE` (default `https://graph.facebook.com`), `INSTAGRAM_DAILY_LIMIT`.

Both adapters register only when their env is complete, exactly like Threads.

---

## 4. Testing (per milestone)

| Layer | Facebook | Instagram |
|---|---|---|
| e2e HTTP | create with `link`-less text → 201; over 63,206 → 422 | create without media → 422 `media_required`; bad kind → 422; image + caption → 201 |
| Adapter vs stub | single POST, message/link params, 500 → retryable, 400 → permanent | container → status poll → publish; `IN_PROGRESS` → retryable requeue; container `ERROR` → permanent; poll bound respected |
| Worker e2e | due post publishes once via FB stub | due post with media publishes once via IG stub; daily budget (set 1) stops the second |
| Regression | `go test -race ./...` stays green; Threads tests unchanged prove the port didn't move | |

Live verification (manual, needs accounts): one real Page post; one real IG image post.
Instagram requires a professional account + PPA check — start this early, it is the usual blocker.

---

## 5. Milestones

| # | Deliverable | Demo |
|---|---|---|
| **M7** | Core media support (VO, migration, port/capabilities, API, capability checks) | create a schedule with `media` through HTTP; Threads/FB behavior unchanged, all tests green |
| **M8** | Facebook adapter + config + stub tests | due post published to a real Page feed |
| **M9** | Instagram adapter + config + stub tests | due image post published to a real IG account |
| **M10** | Live-account verification for both, notes docs (`docs/FACEBOOK_API_NOTES.md`, `docs/INSTAGRAM_API_NOTES.md`), README/plan updates | end-to-end demo on both real accounts |

Sequencing note: M8 and M9 depend only on M7 and can be reviewed independently.

---

## 6. Risks and open questions

| Risk | Mitigation |
|---|---|
| PPA / 2FA on the connected Page blocks IG publishing | Check account setup before M9; the error is documented and loud |
| IG video processing exceeds the in-Prepare poll bound | Retryable requeue; containers valid 24h. Reels/resumable upload (`rupload.facebook.com`) deferred |
| Facebook has no documented message max | Cap at 63,206 with the composer caveat in the notes; Meta's error still surfaces as permanent `failed` |
| Two token types (Page vs user) confuse ops | Separate env prefixes per platform; `bootstrap` fails fast on partial config, runbook lists both |
| Emoji counting for IG caption (2,200) unspecified | Verify with a live emoji post in M10; `TextLengthInBytes` stays false until proven |

Open decisions for you:

1. **Media kinds in M7: image only, or image+video?** (Recommended: image first — video adds
   processing-time handling and the resumable-upload path; the VO and API shape don't change
   when video lands later.)
2. **Facebook link posts**: support `link=` now, or text-only for M8? (Recommended: text-only
   for M8 — link previews are a separate feature with their own validation.)
3. **IG caption editable before publish?** (Recommended: no — media immutable, caption rides
   the content field, rescheduling keeps it.)

**Decided 2026-10-31** ("go ahead with the recommended defaults"): (1) M7 ships the `Media`
value object accepting both `image` and `video` kinds, but platform capabilities gate which
kinds are accepted — Instagram (M9) declares image first; (2) Facebook stays text-only for
M8 (`Publication.MediaURL` stays empty); (3) content and media are immutable on reschedule.
