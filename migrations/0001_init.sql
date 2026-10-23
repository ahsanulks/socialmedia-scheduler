-- 0001_init.sql — write model, event log, read projection, attempt log.

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

CREATE INDEX idx_due        ON scheduled_posts(status, scheduled_at);
CREATE INDEX idx_retry      ON scheduled_posts(status, next_attempt_at);
CREATE INDEX idx_created    ON scheduled_posts(created_at DESC);
CREATE INDEX idx_platform   ON scheduled_posts(platform, published_at);

CREATE TABLE domain_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    aggregate_id      TEXT NOT NULL,
    aggregate_type    TEXT NOT NULL,
    aggregate_version INTEGER NOT NULL,
    event_type        TEXT NOT NULL,
    payload           TEXT NOT NULL,
    occurred_at       INTEGER NOT NULL,
    UNIQUE (aggregate_id, aggregate_version)
);

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

CREATE TABLE publish_attempts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    schedule_id  TEXT NOT NULL REFERENCES scheduled_posts(id),
    platform     TEXT NOT NULL,
    attempt_no   INTEGER NOT NULL,
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER,
    outcome      TEXT NOT NULL,
    prepare_ref  TEXT,
    error_code   TEXT,
    error_msg    TEXT
);

CREATE INDEX idx_attempts_schedule ON publish_attempts(schedule_id, attempt_no);
