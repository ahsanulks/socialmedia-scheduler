// Package instagram implements the Instagram (Graph API) publisher adapter
// for a professional/creator account linked to a Facebook Page.
//
// M9 v1 scope, per the facebook/instagram plan:
//
//	Prepare: POST /{ig-user-id}/media (image_url + caption) creates a media
//	         container; then poll GET /{container-id}?fields=status_code with
//	         a bounded wait until FINISHED. VIDEO is out of scope: capabilities
//	         declare images only ("image"), and the adapter rejects media of
//	         any other kind as a second line of defense.
//	Publish: POST /{ig-user-id}/media_publish with creation_id returns the
//	         final media id.
//
// A container still processing when the bounded poll ends is retryable: the
// worker requeues with backoff, and an unpublished container stays usable for
// 24h (after that it is EXPIRED and a fresh Prepare runs on the next attempt).
package instagram

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
	defaultBaseURL    = "https://graph.facebook.com"
	defaultDailyLimit = 90 // 100/24h quota with a safety margin
	requestTimeout    = 30 * time.Second

	statusFinished   = "FINISHED"
	statusInProgress = "IN_PROGRESS"
	statusError      = "ERROR"
	statusExpired    = "EXPIRED"
)

type Config struct {
	UserID     string // IG user id (the account's id, not the Page id)
	Token      string // user access token with instagram_basic + instagram_content_publish
	BaseURL    string
	DailyLimit int
	// Poll bounds the container-status wait during Prepare. Zero values keep
	// the plan defaults (~15s first poll, ~15s interval, ~90s cap); tests pass
	// small values so the processing paths run in milliseconds.
	Poll       graphapi.PollTiming
	HTTPClient *http.Client
}

type Publisher struct {
	platform domain.Platform
	cfg      Config
	client   *http.Client
}

func New(cfg Config) (*Publisher, error) {
	if strings.TrimSpace(cfg.UserID) == "" {
		return nil, fmt.Errorf("instagram: user id is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("instagram: access token is required")
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
	platform, err := domain.NewPlatform("instagram")
	if err != nil {
		return nil, err
	}
	return &Publisher{platform: platform, cfg: cfg, client: cfg.HTTPClient}, nil
}

func (p *Publisher) Platform() domain.Platform { return p.platform }

func (p *Publisher) Capabilities() app.Capabilities {
	return app.Capabilities{
		// Captions are counted in characters (Unicode code points), not bytes;
		// 2,200 is Meta's documented caption limit.
		MaxTextLength: 2200,
		SupportsText:  true,
		PreparesDraft: true,
		DailyLimit:    p.cfg.DailyLimit,
		MediaKinds:    []string{"image"},
		// Instagram publishes media posts; a captionless image is valid, so
		// media is required or the publish would have nothing to ship.
		RequiresMedia: true,
	}
}

func (p *Publisher) Prepare(ctx context.Context, pub app.Publication) (*domain.PrepareRef, error) {
	if pub.MediaURL == "" || pub.MediaKind != string(domain.MediaKindImage) {
		return nil, app.Permanent(
			fmt.Errorf("instagram: publication media kind=%q url=%q", pub.MediaKind, pub.MediaURL),
			"instagram: an image publication is required",
		)
	}
	// UserID is operator-configured, but escape it anyway: a slash or space in
	// a misconfigured env var must not silently change the endpoint path.
	igPath := "/" + url.PathEscape(p.cfg.UserID)

	form := url.Values{
		"image_url":    {pub.MediaURL},
		"access_token": {p.cfg.Token},
	}
	// Caption is optional for IG image posts; empty string is a valid caption.
	if pub.Content != "" {
		form.Set("caption", pub.Content)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := graphapi.PostForm(ctx, "instagram", p.cfg.BaseURL, p.client, igPath+"/media", form, &created); err != nil {
		return nil, err
	}
	container, err := domain.NewPrepareRef(created.ID)
	if err != nil {
		return nil, app.Permanent(err, "instagram: empty container id in response")
	}

	// Retryable poll transport failures keep the wait going (the container is
	// likely fine), but they are recorded so a deadline miss carries the last
	// poll error into the requeue reason instead of waiting silently.
	var lastPollErr error
	err = p.cfg.Poll.Wait(ctx, "instagram", func() (done bool, err error) {
		status, pollErr := p.containerStatus(ctx, igPath, container.String())
		if pollErr != nil {
			// Poll transport failures are retryable but the container is
			// likely fine: treat like a missed poll tick (keep waiting).
			if app.IsRetryable(pollErr) {
				if lastPollErr == nil {
					lastPollErr = pollErr
				}
				return false, nil
			}
			return false, pollErr
		}
		switch status {
		case statusFinished:
			return true, nil
		case statusInProgress:
			return false, nil // keep waiting inside the bounded poll
		case statusError, statusExpired:
			return false, app.Permanent(
				fmt.Errorf("instagram: container %s status %s", container.String(), status),
				"instagram: container "+strings.ToLower(status),
			)
		default:
			return false, app.Permanent(
				fmt.Errorf("instagram: container %s unknown status %q", container.String(), status),
				"instagram: unknown container status",
			)
		}
	})
	if err != nil {
		if app.IsRetryable(err) && lastPollErr != nil {
			return nil, app.Retryable(err,
				"instagram: container still processing; requeued (last status poll error: "+
					app.FailureReasonOf(lastPollErr)+")")
		}
		return nil, err
	}
	return &container, nil
}

// containerStatus reads status_code for one container and maps the
// not-yet-processed 400 answers onto the retryable split via Classify.
func (p *Publisher) containerStatus(ctx context.Context, igPath, container string) (string, error) {
	var st struct {
		StatusCode string `json:"status_code"`
	}
	q := url.Values{"fields": {"status_code"}, "access_token": {p.cfg.Token}}
	path := igPath + "/" + url.PathEscape(container) + "?" + q.Encode()
	if err := graphapi.GetJSON(ctx, "instagram", p.cfg.BaseURL, p.client, path, &st); err != nil {
		return "", err
	}
	return strings.TrimSpace(st.StatusCode), nil
}

func (p *Publisher) Publish(ctx context.Context, pub app.Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error) {
	if ref == nil {
		return domain.ExternalPostID{}, app.Permanent(
			fmt.Errorf("instagram: publish called without a prepare reference"),
			"instagram: no container to publish",
		)
	}
	igPath := "/" + url.PathEscape(p.cfg.UserID)
	form := url.Values{
		"creation_id":  {ref.String()},
		"access_token": {p.cfg.Token},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := graphapi.PostForm(ctx, "instagram", p.cfg.BaseURL, p.client, igPath+"/media_publish", form, &out); err != nil {
		return domain.ExternalPostID{}, err
	}
	id, err := domain.NewExternalPostID(out.ID)
	if err != nil {
		return domain.ExternalPostID{}, app.Permanent(err, "instagram: empty media id in response")
	}
	return id, nil
}
