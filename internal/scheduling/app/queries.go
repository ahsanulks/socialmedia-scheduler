package app

import (
	"context"
	"fmt"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// GetSchedule returns one row from the read model.
type GetScheduleHandler struct {
	read ScheduleReadRepository
}

func NewGetScheduleHandler(read ScheduleReadRepository) *GetScheduleHandler {
	return &GetScheduleHandler{read: read}
}

func (h *GetScheduleHandler) Handle(ctx context.Context, rawID string) (ScheduleView, error) {
	if _, err := domain.NewPostID(rawID); err != nil {
		return ScheduleView{}, domain.NotFoundError{ID: rawID}
	}
	return h.read.Get(ctx, rawID)
}

// ListSchedules returns the schedule plan.
type ListSchedules struct {
	Statuses []string
	Platform string
	From     *time.Time
	To       *time.Time
	Limit    int
	Desc     bool
	Cursor   string
}

type ListSchedulesHandler struct {
	read ScheduleReadRepository
}

func NewListSchedulesHandler(read ScheduleReadRepository) *ListSchedulesHandler {
	return &ListSchedulesHandler{read: read}
}

func (h *ListSchedulesHandler) Handle(ctx context.Context, cmd ListSchedules) (Page, error) {
	q := ListQuery{
		Limit:  cmd.Limit,
		Desc:   cmd.Desc,
		Cursor: cmd.Cursor,
		From:   cmd.From,
		To:     cmd.To,
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 100 {
		return Page{}, domain.ValidationError{Field: "limit", Message: "must be between 1 and 100"}
	}
	if len(cmd.Statuses) == 0 {
		q.Statuses = []domain.Status{
			domain.StatusScheduled(), domain.StatusPublishing(),
			domain.StatusPublished(), domain.StatusFailed(),
		}
	} else {
		for _, raw := range cmd.Statuses {
			st, err := domain.ParseStatus(raw)
			if err != nil {
				return Page{}, err
			}
			q.Statuses = append(q.Statuses, st)
		}
	}
	if cmd.Platform != "" {
		p, err := domain.NewPlatform(cmd.Platform)
		if err != nil {
			return Page{}, err
		}
		q.Platform = &p
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return Page{}, domain.ValidationError{Field: "to", Message: "must be after from"}
	}
	if q.Cursor != "" {
		if _, _, err := DecodeCursor(q.Cursor); err != nil {
			return Page{}, domain.ValidationError{Field: "cursor", Message: "is malformed"}
		}
	}
	return h.read.List(ctx, q)
}

// Formatted for logging/tests.
func (q ListQuery) String() string {
	return fmt.Sprintf("statuses=%v platform=%v limit=%d desc=%v", q.Statuses, q.Platform, q.Limit, q.Desc)
}
