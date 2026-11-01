package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// IdempotencyStore is the SQLite-backed app.IdempotencyStore. The claim on a
// key is the INSERT itself: SQLite serializes writes, and the RETURNING
// clause makes the ownership decision atomic in a single statement — among
// concurrent same-key requests exactly one INSERT returns a row and owns the
// reservation; the rest read the row the winner created (pending or done).
type IdempotencyStore struct {
	db *sql.DB
}

func NewIdempotencyStore(db *sql.DB) *IdempotencyStore {
	return &IdempotencyStore{db: db}
}

// errRowGone marks the transient state where a row known to exist (the
// INSERT just conflicted on it) was deleted before our follow-up read: an
// external release or prune. Not an ownership signal on its own.
var errRowGone = errors.New("idempotency: row vanished before read")

// reserveAttempts bounds the re-reserve loop for errRowGone. Under normal
// operation it never fires; three failing in a row is a storage problem,
// not a livelock.
const reserveAttempts = 3

func (s *IdempotencyStore) Reserve(ctx context.Context, key, requestHash string) (app.ReserveResult, app.IdempotencyRecord, error) {
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		var rowid int64
		// One atomic statement decides ownership: on conflict DO
		// NOTHING returns no row, on success the fresh rowid. ErrNoRows
		// therefore means "someone else owns this key" — never "no row
		// exists" — so ownership can never fall through ambiguous
		// rows-affected reads.
		err := s.db.QueryRowContext(ctx, `
			INSERT INTO idempotency_keys (key, request_hash, status_code, response, location, created_at)
			VALUES (?,?,NULL,NULL,'',?)
			ON CONFLICT(key) DO NOTHING
			RETURNING rowid`,
			key, requestHash, time.Now().UTC().UnixMilli()).Scan(&rowid)
		if err == nil {
			return app.ReserveOurs, app.IdempotencyRecord{}, nil
		}
		if err != sql.ErrNoRows {
			return 0, app.IdempotencyRecord{}, fmt.Errorf("idempotency reserve: %w", err)
		}

		// Lost the claim; read what the owner holds for this request.
		rec, result, err := s.lookup(ctx, key, requestHash)
		if err == nil {
			return result, rec, nil
		}
		if !errors.Is(err, errRowGone) {
			return 0, app.IdempotencyRecord{}, err
		}
		// The owner's row vanished between the failed INSERT and our
		// read. Reserve again; every loop needs an external delete.
	}
	return 0, app.IdempotencyRecord{}, fmt.Errorf(
		"idempotency reserve: claim not resolvable after %d attempts for key %s", reserveAttempts, key)
}

func (s *IdempotencyStore) lookup(ctx context.Context, key, requestHash string) (app.IdempotencyRecord, app.ReserveResult, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT request_hash, status_code, response, location
		FROM idempotency_keys WHERE key = ?`, key)
	var (
		rec  app.IdempotencyRecord
		code sql.NullInt64
		resp sql.NullString
	)
	rec.Key = key
	if err := row.Scan(&rec.RequestHash, &code, &resp, &rec.Location); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return app.IdempotencyRecord{}, 0, errRowGone
		}
		return app.IdempotencyRecord{}, 0, fmt.Errorf("idempotency lookup: %w", err)
	}
	if rec.RequestHash != requestHash {
		return rec, app.ReserveMismatch, nil
	}
	if resp.Valid {
		rec.StatusCode = int(code.Int64)
		rec.Response = resp.String
		return rec, app.ReserveStored, nil
	}
	return rec, app.ReserveInProgress, nil
}

func (s *IdempotencyStore) Complete(ctx context.Context, key, requestHash string, statusCode int, location, response string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET status_code = ?, response = ?, location = ?
		WHERE key = ? AND request_hash = ? AND response IS NULL`,
		statusCode, response, location, key, requestHash)
	if err != nil {
		return fmt.Errorf("idempotency complete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("idempotency complete: row count unknown: %w", err)
	}
	if n != 1 {
		// A not-pending update has two documented causes: an ops prune
		// between Reserve and Complete, or the row being recreated by
		// someone else. The create succeeded, so this is still logged,
		// never failed.
		return fmt.Errorf("idempotency complete: reservation not pending (%d rows) for key %s", n, key)
	}
	return nil
}

func (s *IdempotencyStore) Release(ctx context.Context, key, requestHash string) error {
	// Hash-guarded: a stale owner whose row was pruned or externally
	// deleted must never remove the claim a later request re-won (that
	// re-opens the double-create window). Completed responses survive.
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE key = ? AND request_hash = ? AND response IS NULL`,
		key, requestHash)
	if err != nil {
		return fmt.Errorf("idempotency release: %w", err)
	}
	return nil
}

// Prune deletes rows older than the cutoff — completed rows and pending
// rows whose process has long since died (the release notes accept the
// double-create risk of a lost key row). Retention must stay far above the
// create duration, or an in-flight reservation could be pruned; that is
// spelled out in the runbook and the env docs.
func (s *IdempotencyStore) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE created_at < ?`,
		before.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("idempotency prune: %w", err)
	}
	return res.RowsAffected()
}
