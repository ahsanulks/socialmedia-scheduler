package e2e

// The M7 media rules, driven the way a client drives them: through HTTP.
// Threads is text-only, "photo" accepts image attachments, "insta" cannot post
// text alone.

import (
	"net/http"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/fake"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

func photoPublisher(t *testing.T, opts ...fake.Option) *fake.Publisher {
	t.Helper()
	caps := app.Capabilities{
		MaxTextLength: 500,
		SupportsText:  true,
		PreparesDraft: true,
		MediaKinds:    []string{domain.MediaKindImage},
	}
	return fakePublisher(t, "photo", append([]fake.Option{fake.WithCapabilities(caps)}, opts...)...)
}

func instaPublisher(t *testing.T) *fake.Publisher {
	t.Helper()
	return fakePublisher(t, "insta", fake.WithCapabilities(app.Capabilities{
		MaxTextLength: 2200,
		SupportsText:  true,
		PreparesDraft: true,
		RequiresMedia: true,
		MediaKinds:    []string{domain.MediaKindImage, domain.MediaKindVideo},
	}))
}

// createWithMedia schedules a post with an optional attachment and fails the
// test unless the API accepted it.
func (e *env) createWithMedia(content, platform, url, kind string, in time.Duration) (string, int) {
	e.t.Helper()
	body := map[string]any{"content": content, "scheduled_at": atIn(in), "platform": platform}
	if url != "" || kind != "" {
		body["media"] = map[string]any{"url": url, "kind": kind}
	}
	resp, raw := e.request(http.MethodPost, "/api/v1/schedules", body, e.token)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create with media: want 201, got %d: %s", resp.StatusCode, raw)
	}
	m := decode(e.t, raw)
	return m["id"].(string), int(m["version"].(float64))
}

func TestCreateWithMediaStoresAndPublishesIt(t *testing.T) {
	pub := photoPublisher(t)
	e := newEnv(t, []app.Publisher{pub})

	const (
		mediaURL  = "https://cdn.example.com/photo.jpg"
		mediaKind = "image"
	)
	id, version := e.createWithMedia("look at this", "photo", mediaURL, mediaKind, time.Hour)
	if version != 1 {
		t.Fatalf("version: want 1, got %d", version)
	}

	got := e.get(id)
	if got["media_url"] != mediaURL || got["media_kind"] != mediaKind {
		t.Fatalf("media roundtrip: %v / %v", got["media_url"], got["media_kind"])
	}
	// The read projection carries the same attachment.
	if e.str(`SELECT media_url FROM schedules_read WHERE id = ?`, id) != mediaURL {
		t.Fatalf("read model media_url missing")
	}

	// Media is immutable: PATCHing content leaves the attachment untouched.
	resp, raw := e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{
		"content":          "a different caption",
		"expected_version": version,
	}, e.token)
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, raw)
	}
	got = e.get(id)
	if got["content"] != "a different caption" || got["media_url"] != mediaURL {
		t.Fatalf("media must not change through PATCH: %v", got)
	}

	// The worker must deliver the attachment to the adapter. Only "due now"
	// is published; "look at this" is scheduled an hour out.
	id2, _ := e.createWithMedia("due now", "photo", mediaURL, mediaKind, 2*time.Second)
	e.waitFor(id2, statusIs("published"))
	published := pub.Published()
	if len(published) != 1 {
		t.Fatalf("published records: %d", len(published))
	}
	if published[0].MediaURL != mediaURL || published[0].MediaKind != mediaKind {
		t.Fatalf("published record missing media: %+v", published[0])
	}
	prepared := pub.Prepared()
	if len(prepared) != 1 || prepared[0].MediaURL != mediaURL || prepared[0].MediaKind != mediaKind {
		t.Fatalf("media missing from Prepare: %+v", prepared)
	}
}

func TestCreateValidation_MediaRules(t *testing.T) {
	cases := []struct {
		name      string
		pubs      []app.Publisher
		platform  string
		mediaURL  string
		mediaKind string
		wantStat  int
		wantCode  string
	}{
		{
			name:     "media sent to a text-only platform",
			pubs:     []app.Publisher{fakePublisher(t, "threads")},
			platform: "threads",
			mediaURL: "https://cdn.example.com/photo.jpg", mediaKind: "image",
			wantStat: 422, wantCode: "unsupported_media_type",
		},
		{
			name:     "video to an image-only platform",
			pubs:     []app.Publisher{photoPublisher(t)},
			platform: "photo",
			mediaURL: "https://cdn.example.com/clip.mp4", mediaKind: "video",
			wantStat: 422, wantCode: "unsupported_media_type",
		},
		{
			name:     "media-required platform with no media",
			pubs:     []app.Publisher{instaPublisher(t)},
			platform: "insta",
			wantStat: 422, wantCode: "media_required",
		},
		{
			name:     "plain http url",
			pubs:     []app.Publisher{photoPublisher(t)},
			platform: "photo",
			mediaURL: "http://cdn.example.com/photo.jpg", mediaKind: "image",
			wantStat: 422, wantCode: "validation_failed",
		},
		{
			name:     "unknown media kind",
			pubs:     []app.Publisher{photoPublisher(t)},
			platform: "photo",
			mediaURL: "https://cdn.example.com/sticker.gif", mediaKind: "gif",
			wantStat: 422, wantCode: "validation_failed",
		},
		{
			name:     "media required and provided",
			pubs:     []app.Publisher{instaPublisher(t)},
			platform: "insta",
			mediaURL: "https://cdn.example.com/photo.jpg", mediaKind: "image",
			wantStat: 201, wantCode: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.pubs, withoutWorker())
			body := map[string]any{
				"content":      "a caption",
				"scheduled_at": atIn(time.Hour),
				"platform":     tc.platform,
			}
			if tc.mediaURL != "" || tc.mediaKind != "" {
				body["media"] = map[string]any{"url": tc.mediaURL, "kind": tc.mediaKind}
			}
			resp, raw := e.request(http.MethodPost, "/api/v1/schedules", body, e.token)
			if resp.StatusCode != tc.wantStat {
				t.Fatalf("status: want %d, got %d: %s", tc.wantStat, resp.StatusCode, raw)
			}
			if tc.wantCode != "" && decode(t, raw)["code"] != tc.wantCode {
				t.Fatalf("code: want %s, got %s", tc.wantCode, raw)
			}
		})
	}
}

func TestPlatformsEndpointReportsMediaKinds(t *testing.T) {
	e := newEnv(t, []app.Publisher{photoPublisher(t), fakePublisher(t, "threads")})

	resp, raw := e.request(http.MethodGet, "/api/v1/platforms", nil, e.token)
	if resp.StatusCode != 200 {
		t.Fatalf("platforms: %d %s", resp.StatusCode, raw)
	}
	list := decode(t, raw)["platforms"].([]any)
	byName := map[string]map[string]any{}
	for _, it := range list {
		m := it.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if kinds := byName["photo"]["media_kinds"].([]any); len(kinds) != 1 || kinds[0] != "image" {
		t.Fatalf("photo media_kinds: %v", byName["photo"]["media_kinds"])
	}
	if kinds, ok := byName["threads"]["media_kinds"].([]any); !ok || len(kinds) != 0 {
		t.Fatalf("threads media_kinds: %v", byName["threads"]["media_kinds"])
	}
}
