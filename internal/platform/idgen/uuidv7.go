package idgen

import (
	"github.com/google/uuid"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// UUIDv7 generates time-ordered UUIDs, which keeps SQLite index inserts local.
type UUIDv7 struct{}

func (UUIDv7) NewID() (domain.PostID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return domain.PostID{}, err
	}
	return domain.NewPostID(u.String())
}
