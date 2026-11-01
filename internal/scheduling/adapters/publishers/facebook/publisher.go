// Package facebook implements the Facebook Pages publisher adapter.
//
// Publishing is a single call: POST /{page-id}/feed with the message and the
// Page access token. There is no platform-side draft, so Prepare does nothing.
// v1 is text-only: link previews and media posts would need their own
// parameters (link=, photo_url=) and validation, and are deferred per the
// facebook/instagram plan (§6 decisions).
package facebook

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/graphapi"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

const (
	// defaultBaseURL intentionally carries no version segment; production
	// setups should pin one (FACEBOOK_API_BASE=https://graph.facebook.com/v23.0)
	// so Meta's deprecations cannot change behavior underneath them.
	defaultBaseURL = "https://graph.facebook.com"
	requestTimeout = 30 * time.Second
)

type Config struct {
	PageID     string
	PageToken  string // Page access token, not a user token
	BaseURL    string
	HTTPClient *http.Client
}

type Publisher struct {
	platform domain.Platform
	cfg      Config
	client   *http.Client
}

func New(cfg Config) (*Publisher, error) {
	if strings.TrimSpace(cfg.PageID) == "" {
		return nil, fmt.Errorf("facebook: page id is required")
	}
	if strings.TrimSpace(cfg.PageToken) == "" {
		return nil, fmt.Errorf("facebook: page token is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	platform, err := domain.NewPlatform("facebook")
	if err != nil {
		return nil, err
	}
	return &Publisher{platform: platform, cfg: cfg, client: cfg.HTTPClient}, nil
}

func (p *Publisher) Platform() domain.Platform { return p.platform }

func (p *Publisher) Capabilities() app.Capabilities {
	return app.Capabilities{
		// No documented Graph limit on a feed message; 63,206 is the composer
		// ceiling observed in the wild and treated as a sanity bound.
		MaxTextLength: 63206,
		SupportsText:  true,
		PreparesDraft: false,
		// Meta publishes no post quota for Pages; 0 = no client-side budget.
		DailyLimit: 0,
		// Text-only until link/media posts land; media to facebook is rejected
		// at the API edge via the empty MediaKinds list.
		MediaKinds: nil,
	}
}

// Prepare is a no-op: a feed post is one irreversible call, so the port's
// prepare phase has nothing platform-side to create.
func (p *Publisher) Prepare(ctx context.Context, pub app.Publication) (*domain.PrepareRef, error) {
	return nil, nil
}

func (p *Publisher) Publish(ctx context.Context, pub app.Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error) {
	if pub.MediaURL != "" || pub.MediaKind != "" {
		return domain.ExternalPostID{}, app.Permanent(
			fmt.Errorf("facebook: publication carries media %+v", pub),
			"facebook: media posts are not implemented yet",
		)
	}
	form := url.Values{
		"message":      {pub.Content},
		"access_token": {p.cfg.PageToken},
	}
	var out struct {
		ID string `json:"id"`
	}
	// PageID is operator-configured, but escape it anyway: a slash or space in
	// a misconfigured env var must not silently change the endpoint path.
	if err := p.postForm(ctx, "/"+url.PathEscape(p.cfg.PageID)+"/feed", form, &out); err != nil {
		return domain.ExternalPostID{}, err
	}
	// The id from /feed is opaque (often "{post_id}_{story_id}") — stored as-is.
	id, err := domain.NewExternalPostID(out.ID)
	if err != nil {
		return domain.ExternalPostID{}, app.Permanent(err, "facebook: empty post id in response")
	}
	return id, nil
}

func (p *Publisher) postForm(ctx context.Context, path string, form url.Values, out any) error {
	// See graphapi.PostForm: the retryable unreadable-2xx corner could
	// double-post for this single-call adapter; documented tradeoff, kept
	// for one shared client code path.
	return graphapi.PostForm(ctx, "facebook", p.cfg.BaseURL, p.client, path, form, out)
}
