package sqlite

import (
	"context"
	"database/sql"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// Projector maintains the flat read model. It is written from the aggregate
// snapshot in the same transaction as the write, which gives read-your-writes;
// keeping it a separate type lets it move to an async consumer later.
type Projector struct{}

func NewProjector() *Projector { return &Projector{} }

func (p *Projector) Apply(ctx context.Context, tx *sql.Tx, s domain.Snapshot) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO schedules_read
		(id, content, media_url, media_kind, status, scheduled_at, platform, attempts, last_error, external_post_id,
		 version, created_at, updated_at, published_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			content = excluded.content,
			media_url = excluded.media_url,
			media_kind = excluded.media_kind,
			status = excluded.status,
			scheduled_at = excluded.scheduled_at,
			platform = excluded.platform,
			attempts = excluded.attempts,
			last_error = excluded.last_error,
			external_post_id = excluded.external_post_id,
			version = excluded.version,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			published_at = excluded.published_at
		WHERE excluded.version >= schedules_read.version`,
		s.ID, s.Content, nullableString(s.MediaURL), nullableString(s.MediaKind), s.Status,
		s.ScheduledAt.UTC().UnixMilli(), s.Platform, s.Attempts,
		nullableString(s.LastError), nullableString(s.ExternalPostID), s.Version,
		s.CreatedAt.UTC().UnixMilli(), s.UpdatedAt.UTC().UnixMilli(), nullableMillis(s.PublishedAt))
	return err
}
