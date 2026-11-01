package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// IdempotencyStore is the SQLite-backed app.IdempotencyStore.
type IdempotencyStore struct {
	db *sql.DB
}

func NewIdempotencyStore(db *sql.DB) *IdempotencyStore {
	return &IdempotencyStore{db: db}
}

func (s *IdempotencyStore) Get(key string) (app.IdempotencyRecord, bool, error) {
	row := s.db.QueryRowContext(context.Background(), `
		SELECT key, request_hash, status_code, response
		FROM idempotency_keys WHERE key = ?`, key)
	var rec app.IdempotencyRecord
	err := row.Scan(&rec.Key, &rec.RequestHash, &rec.StatusCode, &rec.Response)
	if err == sql.ErrNoRows {
		return app.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return app.IdempotencyRecord{}, false, fmt.Errorf("idempotency lookup: %w", err)
	}
	return rec, true, nil
}

func (s *IdempotencyStore) Put(rec app.IdempotencyRecord) error {
	// First writer wins: a racing retry with the same key must not overwrite
	// the response another attempt already replays.
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO idempotency_keys (key, request_hash, status_code, response, created_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(key) DO NOTHING`,
		rec.Key, rec.RequestHash, rec.StatusCode, rec.Response, time.Now().UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("idempotency store: %w", err)
	}
	return nil
}
