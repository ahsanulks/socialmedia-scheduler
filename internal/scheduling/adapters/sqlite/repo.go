package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

const postColumns = `id, content, media_url, media_kind, scheduled_at, platform, status, version, attempts, prepare_ref,
	next_attempt_at, last_error, lease_until, external_post_id, published_at, failed_at, canceled_at,
	created_at, updated_at`

// Repo is the write-side adapter: it persists the aggregate, its events and the
// read projection in one transaction.
type Repo struct {
	db        *sql.DB
	projector *Projector
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db, projector: NewProjector()}
}

func (r *Repo) Get(ctx context.Context, id domain.PostID) (*domain.ScheduledPost, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+postColumns+` FROM scheduled_posts WHERE id = ?`, id.String())
	snap, err := scanPost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFoundError{ID: id.String()}
	}
	if err != nil {
		return nil, err
	}
	return domain.Rehydrate(snap)
}

// Save writes state, events and projection atomically. The version guard is the
// aggregate's base version, so a concurrent writer loses and gets a conflict.
func (r *Repo) Save(ctx context.Context, post *domain.ScheduledPost, events []domain.Event) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	s := post.Snapshot()
	if post.BaseVersion().IsZero() {
		_, err = tx.ExecContext(ctx, `INSERT INTO scheduled_posts (`+postColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.ID, s.Content, nullableString(s.MediaURL), nullableString(s.MediaKind),
			s.ScheduledAt.UTC().UnixMilli(), s.Platform, s.Status, s.Version, s.Attempts,
			nullableString(s.PrepareRef), nullableMillis(s.NextAttemptAt), nullableString(s.LastError),
			nullableMillis(s.LeaseUntil), nullableString(s.ExternalPostID), nullableMillis(s.PublishedAt),
			nullableMillis(s.FailedAt), nullableMillis(s.CanceledAt),
			s.CreatedAt.UTC().UnixMilli(), s.UpdatedAt.UTC().UnixMilli())
		if err != nil {
			return fmt.Errorf("insert schedule: %w", err)
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE scheduled_posts SET
				media_url = ?, media_kind = ?,
				content = ?, scheduled_at = ?, platform = ?, status = ?, version = ?, attempts = ?,
				prepare_ref = ?, next_attempt_at = ?, last_error = ?, lease_until = ?,
				external_post_id = ?, published_at = ?, failed_at = ?, canceled_at = ?, updated_at = ?
			WHERE id = ? AND version = ?`,
			nullableString(s.MediaURL), nullableString(s.MediaKind),
			s.Content, s.ScheduledAt.UTC().UnixMilli(), s.Platform, s.Status, s.Version, s.Attempts,
			nullableString(s.PrepareRef), nullableMillis(s.NextAttemptAt), nullableString(s.LastError),
			nullableMillis(s.LeaseUntil), nullableString(s.ExternalPostID), nullableMillis(s.PublishedAt),
			nullableMillis(s.FailedAt), nullableMillis(s.CanceledAt), s.UpdatedAt.UTC().UnixMilli(),
			s.ID, post.BaseVersion().Int())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			var actual int
			_ = tx.QueryRowContext(ctx, `SELECT version FROM scheduled_posts WHERE id = ?`, s.ID).Scan(&actual)
			return domain.VersionConflictError{Expected: post.BaseVersion().Int(), Actual: actual}
		}
	}

	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO domain_events
			(aggregate_id, aggregate_type, aggregate_version, event_type, payload, occurred_at)
			VALUES (?,?,?,?,?,?)`,
			e.AggregateID().String(), "ScheduledPost", e.Version().Int(), e.EventType(),
			string(payload), e.OccurredAt().UTC().UnixMilli()); err != nil {
			return err
		}
	}

	if err := r.projector.Apply(ctx, tx, s); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	post.MarkPersisted()
	return nil
}

func (r *Repo) FindDueIDs(ctx context.Context, now time.Time, limit int) ([]domain.PostID, error) {
	ms := now.UTC().UnixMilli()
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM scheduled_posts
		WHERE status = 'scheduled'
		  AND scheduled_at <= ?
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY scheduled_at ASC, id ASC
		LIMIT ?`, ms, ms, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (r *Repo) FindExpiredLeaseIDs(ctx context.Context, now time.Time, limit int) ([]domain.PostID, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM scheduled_posts
		WHERE status = 'publishing'
		  AND lease_until IS NOT NULL
		  AND lease_until <= ?
		ORDER BY lease_until ASC
		LIMIT ?`, now.UTC().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (r *Repo) CountPublishedSince(ctx context.Context, platform domain.Platform, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM scheduled_posts
		WHERE platform = ? AND published_at IS NOT NULL AND published_at >= ?`,
		platform.String(), since.UTC().UnixMilli()).Scan(&n)
	return n, err
}

func (r *Repo) StartAttempt(ctx context.Context, rec domain.AttemptRecord) (int64, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO publish_attempts
		(schedule_id, platform, attempt_no, started_at, outcome, prepare_ref)
		VALUES (?,?,?,?,?,?)`,
		rec.ScheduleID, rec.Platform, rec.AttemptNo, rec.StartedAt.UTC().UnixMilli(),
		string(domain.OutcomeStarted), nullableString(rec.PrepareRef))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repo) FinishAttempt(ctx context.Context, attemptID int64, outcome domain.AttemptOutcome, errCode, errMsg string, finishedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE publish_attempts
		SET finished_at = ?, outcome = ?, error_code = ?, error_msg = ?
		WHERE id = ?`,
		finishedAt.UTC().UnixMilli(), string(outcome), nullIfEmpty(errCode), nullIfEmpty(errMsg), attemptID)
	return err
}

func scanIDs(rows *sql.Rows) ([]domain.PostID, error) {
	var out []domain.PostID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := domain.NewPostID(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanPost(row interface{ Scan(dest ...any) error }) (domain.Snapshot, error) {
	var (
		s                                          domain.Snapshot
		scheduledAt, createdAt, updatedAt          int64
		mediaURL, mediaKind                        sql.NullString
		prepareRef, lastError, externalID          sql.NullString
		nextAttempt, leaseUntil, published, failed sql.NullInt64
		canceled                                   sql.NullInt64
		status                                     string
	)
	err := row.Scan(&s.ID, &s.Content, &mediaURL, &mediaKind, &scheduledAt, &s.Platform, &status, &s.Version, &s.Attempts,
		&prepareRef, &nextAttempt, &lastError, &leaseUntil, &externalID, &published, &failed, &canceled,
		&createdAt, &updatedAt)
	if err != nil {
		return domain.Snapshot{}, err
	}
	s.Status = status
	s.ScheduledAt = time.UnixMilli(scheduledAt).UTC()
	s.CreatedAt = time.UnixMilli(createdAt).UTC()
	s.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	s.MediaURL = stringPtr(mediaURL)
	s.MediaKind = stringPtr(mediaKind)
	s.PrepareRef = stringPtr(prepareRef)
	s.LastError = stringPtr(lastError)
	s.ExternalPostID = stringPtr(externalID)
	s.NextAttemptAt = timePtr(nextAttempt)
	s.LeaseUntil = timePtr(leaseUntil)
	s.PublishedAt = timePtr(published)
	s.FailedAt = timePtr(failed)
	s.CanceledAt = timePtr(canceled)
	return s, nil
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMilli()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func stringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func timePtr(ni sql.NullInt64) *time.Time {
	if !ni.Valid {
		return nil
	}
	t := time.UnixMilli(ni.Int64).UTC()
	return &t
}
