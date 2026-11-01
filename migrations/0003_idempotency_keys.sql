-- Idempotency keys for POST /api/v1/schedules (M6): a retried request with the
-- same Idempotency-Key and the same request hash replays the stored response.
-- Keys live forever; the table is tiny and ops prunes it or drops it freely —
-- a lost key row only means the same retry could create the schedule twice.
CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    status_code  INTEGER NOT NULL,
    response     TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
