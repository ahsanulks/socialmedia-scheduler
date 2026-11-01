-- Recreate the idempotency table (M6 review): reservation must precede the
-- create so concurrent same-key requests cannot both create schedules.
-- response NULL marks a pending reservation (claimed, not yet completed);
-- completed rows carry the stored 201 response and its Location header.
-- The 0003 feature never shipped to a persistent database, so DROP+CREATE is
-- the honest path; a lost key row only means a retry could create a schedule
-- twice, which the release notes already state.
DROP TABLE idempotency_keys;
CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    status_code  INTEGER,              -- NULL while the reservation is pending
    response     TEXT,                 -- NULL while the reservation is pending
    location     TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
