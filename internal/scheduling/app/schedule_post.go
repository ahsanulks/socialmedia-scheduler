package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// SchedulePost creates a schedule.
type SchedulePost struct {
	Content     string
	ScheduledAt time.Time
	Platform    string
	MediaURL    string // optional attachment; empty for text-only posts
	MediaKind   string
}

type SchedulePostHandler struct {
	posts    domain.ScheduledPostRepository
	ids      IDGenerator
	registry *Registry
	clock    Clock
}

func NewSchedulePostHandler(posts domain.ScheduledPostRepository, ids IDGenerator, registry *Registry, clock Clock) *SchedulePostHandler {
	return &SchedulePostHandler{posts: posts, ids: ids, registry: registry, clock: clock}
}

func (h *SchedulePostHandler) Handle(ctx context.Context, cmd SchedulePost) (Result, error) {
	platform, err := domain.NewPlatform(cmd.Platform)
	if err != nil {
		return Result{}, err
	}
	caps, ok := h.registry.Capabilities(platform)
	if !ok {
		return Result{}, UnsupportedPlatformError{Platform: platform.String()}
	}
	content, err := domain.NewPostContent(cmd.Content)
	if err != nil {
		return Result{}, err
	}
	var media *domain.Media
	if cmd.MediaURL != "" || cmd.MediaKind != "" {
		m, err := domain.NewMedia(cmd.MediaURL, cmd.MediaKind)
		if err != nil {
			return Result{}, err
		}
		media = &m
	}
	if err := checkCapabilities(platform, caps, content, media); err != nil {
		return Result{}, err
	}

	id, err := h.ids.NewID()
	if err != nil {
		return Result{}, err
	}
	post, err := domain.NewScheduledPost(id, content, media, domain.NewScheduleTime(cmd.ScheduledAt), platform, h.clock.Now())
	if err != nil {
		return Result{}, err
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		return Result{}, err
	}
	return Result{ID: id.String(), Version: post.Version().Int()}, nil
}

// checkCapabilities rejects what the target platform cannot express, before the
// schedule is stored. Anything not checkable here surfaces in Prepare.
func checkCapabilities(platform domain.Platform, caps Capabilities, content domain.PostContent, media *domain.Media) error {
	got, unit := content.RuneCount(), "characters"
	if caps.TextLengthInBytes {
		got, unit = content.ByteCount(), "bytes"
	}
	if caps.MaxTextLength > 0 && got > caps.MaxTextLength {
		return PlatformLimitError{
			Platform: platform.String(),
			Field:    "content",
			Message:  fmt.Sprintf("platform %q allows at most %d %s, got %d", platform.String(), caps.MaxTextLength, unit, got),
		}
	}
	if media != nil && !slices.Contains(caps.MediaKinds, media.Kind()) {
		return UnsupportedMediaError{Platform: platform.String(), Kind: media.Kind()}
	}
	if media == nil && caps.RequiresMedia {
		return MediaRequiredError{Platform: platform.String()}
	}
	return nil
}
