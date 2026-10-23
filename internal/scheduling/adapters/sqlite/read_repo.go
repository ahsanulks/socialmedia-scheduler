package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// ReadRepo serves queries straight from the projection; it never loads an
// aggregate.
type ReadRepo struct {
	db *sql.DB
}

func NewReadRepo(db *sql.DB) *ReadRepo { return &ReadRepo{db: db} }

const viewColumns = `id, content, status, scheduled_at, platform, attempts, last_error,
	external_post_id, version, created_at, updated_at, published_at`

func (r *ReadRepo) Get(ctx context.Context, id string) (app.ScheduleView, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+viewColumns+` FROM schedules_read WHERE id = ?`, id)
	v, err := scanView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return app.ScheduleView{}, domain.NotFoundError{ID: id}
	}
	return v, err
}

func (r *ReadRepo) List(ctx context.Context, q app.ListQuery) (app.Page, error) {
	var where []string
	var args []any

	if len(q.Statuses) > 0 {
		placeholders := make([]string, len(q.Statuses))
		for i, st := range q.Statuses {
			placeholders[i] = "?"
			args = append(args, st.String())
		}
		where = append(where, "status IN ("+strings.Join(placeholders, ",")+")")
	}
	if q.Platform != nil {
		where = append(where, "platform = ?")
		args = append(args, q.Platform.String())
	}
	if q.From != nil {
		where = append(where, "scheduled_at >= ?")
		args = append(args, q.From.UTC().UnixMilli())
	}
	if q.To != nil {
		where = append(where, "scheduled_at < ?")
		args = append(args, q.To.UTC().UnixMilli())
	}
	if q.Cursor != "" {
		at, id, err := app.DecodeCursor(q.Cursor)
		if err != nil {
			return app.Page{}, domain.ValidationError{Field: "cursor", Message: "is malformed"}
		}
		ms := at.UnixMilli()
		if q.Desc {
			where = append(where, "(scheduled_at < ? OR (scheduled_at = ? AND id < ?))")
		} else {
			where = append(where, "(scheduled_at > ? OR (scheduled_at = ? AND id > ?))")
		}
		args = append(args, ms, ms, id)
	}

	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	query := `SELECT ` + viewColumns + ` FROM schedules_read`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY scheduled_at " + dir + ", id " + dir + " LIMIT ?"
	args = append(args, q.Limit+1)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return app.Page{}, err
	}
	defer rows.Close()

	items := make([]app.ScheduleView, 0, q.Limit)
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return app.Page{}, err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return app.Page{}, err
	}

	page := app.Page{Items: items}
	if len(items) > q.Limit {
		page.Items = items[:q.Limit]
		last := page.Items[q.Limit-1]
		page.NextCursor = app.EncodeCursor(last.ScheduledAt, last.ID)
	}
	return page, nil
}

func scanView(row interface{ Scan(dest ...any) error }) (app.ScheduleView, error) {
	var (
		v                                 app.ScheduleView
		scheduledAt, createdAt, updatedAt int64
		lastError, externalID             sql.NullString
		published                         sql.NullInt64
	)
	err := row.Scan(&v.ID, &v.Content, &v.Status, &scheduledAt, &v.Platform, &v.Attempts,
		&lastError, &externalID, &v.Version, &createdAt, &updatedAt, &published)
	if err != nil {
		return app.ScheduleView{}, err
	}
	v.ScheduledAt = time.UnixMilli(scheduledAt).UTC()
	v.CreatedAt = time.UnixMilli(createdAt).UTC()
	v.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	v.LastError = stringPtr(lastError)
	v.ExternalPostID = stringPtr(externalID)
	v.PublishedAt = timePtr(published)
	return v, nil
}
