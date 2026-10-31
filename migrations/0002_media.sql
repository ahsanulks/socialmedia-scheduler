-- 0002_media.sql — optional media attachment on a scheduled post.
-- Immutable once created: nothing upstream needs to mutate it, and the publish
-- flow re-uses the stored row on every attempt, so no updatable copy exists.

ALTER TABLE scheduled_posts ADD COLUMN media_url  TEXT;
ALTER TABLE scheduled_posts ADD COLUMN media_kind TEXT CHECK (media_kind IN ('image','video'));

ALTER TABLE schedules_read ADD COLUMN media_url  TEXT;
ALTER TABLE schedules_read ADD COLUMN media_kind TEXT CHECK (media_kind IN ('image','video'));
