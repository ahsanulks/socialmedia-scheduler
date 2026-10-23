// Package threads implements the Threads (Meta Graph API) publisher adapter.
// Prepare creates a text container; Publish publishes it.
package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

const (
	defaultBaseURL    = "https://graph.threads.net/v1.0"
	defaultDailyLimit = 240 // 250/24h quota with a safety margin
	requestTimeout    = 30 * time.Second
)

type Config struct {
	UserID      string
	AccessToken string
	BaseURL     string
	DailyLimit  int
	HTTPClient  *http.Client
}

type Publisher struct {
	platform domain.Platform
	cfg      Config
	client   *http.Client
}

func New(cfg Config) (*Publisher, error) {
	if strings.TrimSpace(cfg.UserID) == "" {
		return nil, fmt.Errorf("threads: user id is required")
	}
	if strings.TrimSpace(cfg.AccessToken) == "" {
		return nil, fmt.Errorf("threads: access token is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.DailyLimit == 0 {
		cfg.DailyLimit = defaultDailyLimit
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	platform, err := domain.NewPlatform("threads")
	if err != nil {
		return nil, err
	}
	return &Publisher{platform: platform, cfg: cfg, client: cfg.HTTPClient}, nil
}

func (p *Publisher) Platform() domain.Platform { return p.platform }

func (p *Publisher) Capabilities() app.Capabilities {
	return app.Capabilities{
		MaxTextLength: 500,
		SupportsText:  true,
		PreparesDraft: true,
		DailyLimit:    p.cfg.DailyLimit,
	}
}

func (p *Publisher) Prepare(ctx context.Context, pub app.Publication) (*domain.PrepareRef, error) {
	form := url.Values{
		"media_type":   {"TEXT"},
		"text":         {pub.Content},
		"access_token": {p.cfg.AccessToken},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := p.postForm(ctx, "/"+p.cfg.UserID+"/threads", form, &out); err != nil {
		return nil, err
	}
	ref, err := domain.NewPrepareRef(out.ID)
	if err != nil {
		return nil, app.Permanent(err, "threads: empty container id in response")
	}
	return &ref, nil
}

func (p *Publisher) Publish(ctx context.Context, pub app.Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error) {
	if ref == nil {
		return domain.ExternalPostID{}, app.Permanent(
			fmt.Errorf("threads: publish called without a prepare reference"),
			"threads: no container to publish",
		)
	}
	form := url.Values{
		"creation_id":  {ref.String()},
		"access_token": {p.cfg.AccessToken},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := p.postForm(ctx, "/"+p.cfg.UserID+"/threads_publish", form, &out); err != nil {
		return domain.ExternalPostID{}, err
	}
	id, err := domain.NewExternalPostID(out.ID)
	if err != nil {
		return domain.ExternalPostID{}, app.Permanent(err, "threads: empty post id in response")
	}
	return id, nil
}

func (p *Publisher) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(p.cfg.BaseURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return app.Permanent(err, "threads: bad request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return app.Retryable(err, "threads: network error")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			return app.Retryable(err, "threads: unreadable response")
		}
		return nil
	}
	return classify(resp.StatusCode, body)
}
