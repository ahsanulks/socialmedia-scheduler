package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// Clock is injected everywhere; the domain and application layers never call
// time.Now directly.
type Clock interface {
	Now() time.Time
}

// IDGenerator produces aggregate ids (UUIDv7).
type IDGenerator interface {
	NewID() (domain.PostID, error)
}

// Publication is the platform-neutral description of what to publish. It grows
// as the domain grows (media, replies, …) without changing existing adapters.
type Publication struct {
	Platform domain.Platform
	Content  string
}

// Capabilities is each adapter's declaration of its own format's rules.
type Capabilities struct {
	MaxTextLength int  // 0 = unknown
	RequiresMedia bool // platform cannot post text alone
	SupportsText  bool
	PreparesDraft bool // Prepare creates a platform-side draft
	DailyLimit    int  // posts per rolling 24h; 0 = unknown
}

// Publisher is the platform seam. Implementations live in
// adapters/publishers/<name>; nothing platform-specific may cross this port.
type Publisher interface {
	Platform() domain.Platform
	Capabilities() Capabilities
	Prepare(ctx context.Context, pub Publication) (*domain.PrepareRef, error)
	Publish(ctx context.Context, pub Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error)
}

// PublishError carries the adapter's classification of a failure.
type PublishError struct {
	Retryable bool
	Reason    string
	Err       error
}

func (e *PublishError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "publish failed"
}

func (e *PublishError) Unwrap() error { return e.Err }

// Retryable marks an error as worth retrying.
func Retryable(err error, reason string) error {
	return &PublishError{Retryable: true, Reason: reason, Err: err}
}

// Permanent marks an error as not worth retrying.
func Permanent(err error, reason string) error {
	return &PublishError{Retryable: false, Reason: reason, Err: err}
}

// IsRetryable reports the adapter's classification; unknown errors default to
// retryable, because a transient unknown is better recovered than dropped.
func IsRetryable(err error) bool {
	var pe *PublishError
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return true
}

// FailureReasonOf extracts a human-readable reason for last_error.
func FailureReasonOf(err error) string {
	var pe *PublishError
	if errors.As(err, &pe) {
		if pe.Reason != "" {
			return pe.Reason
		}
		if pe.Err != nil {
			return pe.Err.Error()
		}
	}
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// Registry maps a platform slug to its adapter.
type Registry struct {
	byPlatform map[domain.Platform]Publisher
}

func NewRegistry(publishers ...Publisher) (*Registry, error) {
	m := make(map[domain.Platform]Publisher, len(publishers))
	for _, p := range publishers {
		if p == nil {
			return nil, errors.New("nil publisher")
		}
		key := p.Platform()
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("duplicate publisher for platform %q", key.String())
		}
		m[key] = p
	}
	return &Registry{byPlatform: m}, nil
}

func (r *Registry) Resolve(p domain.Platform) (Publisher, bool) {
	pub, ok := r.byPlatform[p]
	return pub, ok
}

func (r *Registry) Capabilities(p domain.Platform) (Capabilities, bool) {
	pub, ok := r.byPlatform[p]
	if !ok {
		return Capabilities{}, false
	}
	return pub.Capabilities(), true
}

func (r *Registry) Platforms() []domain.Platform {
	out := make([]domain.Platform, 0, len(r.byPlatform))
	for p := range r.byPlatform {
		out = append(out, p)
	}
	return out
}

// ScheduleView is the read model returned by queries.
type ScheduleView struct {
	ID             string
	Content        string
	Status         string
	ScheduledAt    time.Time
	Platform       string
	Attempts       int
	LastError      *string
	ExternalPostID *string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PublishedAt    *time.Time
}

// ListQuery is the normalized query for the read side.
type ListQuery struct {
	Statuses []domain.Status
	Platform *domain.Platform
	From     *time.Time
	To       *time.Time
	Limit    int
	Desc     bool
	Cursor   string
}

// Page is one page of the read model.
type Page struct {
	Items      []ScheduleView
	NextCursor string
}

// ScheduleReadRepository is the query-side port; it never loads aggregates.
type ScheduleReadRepository interface {
	Get(ctx context.Context, id string) (ScheduleView, error)
	List(ctx context.Context, q ListQuery) (Page, error)
}

// EncodeCursor builds the opaque keyset cursor for (scheduled_at, id).
func EncodeCursor(t time.Time, id string) string {
	raw := fmt.Sprintf("%d|%s", t.UTC().UnixMilli(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor produced by EncodeCursor.
func DecodeCursor(s string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	return time.UnixMilli(ms).UTC(), parts[1], nil
}

// Result is what every command returns.
type Result struct {
	ID      string
	Version int
}
