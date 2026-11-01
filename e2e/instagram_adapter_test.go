package e2e

// The Instagram adapter, driven the way a client drives it: real HTTP, real
// SQLite, real worker, a stub standing in for the Graph API's container flow
// (create container -> poll status -> media_publish).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/graphapi"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/instagram"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

const (
	igContainerID = "17841400574329543"
	igMediaID     = "17900000000000001"
)

// igStub stands in for the three endpoints the adapter uses:
// POST /{ig-user-id}/media, GET /{ig-user-id}/{container-id}, and
// POST /{ig-user-id}/media_publish.
type igStub struct {
	mu           sync.Mutex
	createCalls  int
	publishCalls int
	statusCalls  int

	createDelayOnce time.Duration // sleep on the first container-creation call only

	statusScript []string // returned per poll call; the last entry repeats
	lastImageURL string
	lastCaption  string
	lastCreation string

	publishFailTimes  int
	publishFailStatus int
}

func (s *igStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/media"):
			if s.createDelayOnce > 0 {
				// Sleep outside the lock below: the delay happens once, on
				// purpose, to simulate a slow container-creation call.
				delay := s.createDelayOnce
				s.createDelayOnce = 0
				s.mu.Unlock()
				time.Sleep(delay)
				s.mu.Lock()
			}
			s.createCalls++
			s.lastImageURL = r.PostForm.Get("image_url")
			s.lastCaption = r.PostForm.Get("caption")
			_, _ = w.Write([]byte(`{"id":"` + igContainerID + `"}`))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, igContainerID):
			s.statusCalls++
			status := s.statusScript[len(s.statusScript)-1]
			if s.statusCalls <= len(s.statusScript) {
				status = s.statusScript[s.statusCalls-1]
			}
			_, _ = w.Write([]byte(`{"status_code":"` + status + `"}`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/media_publish"):
			s.publishCalls++
			if s.publishFailTimes > 0 && s.publishCalls <= s.publishFailTimes {
				w.WriteHeader(s.publishFailStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"cross-post was rate limited","code":1}}`))
				return
			}
			s.lastCreation = r.PostForm.Get("creation_id")
			_, _ = w.Write([]byte(`{"id":"` + igMediaID + `"}`))

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected stub path"}}`))
		}
	}
}

func newInstagramPublisher(t *testing.T, baseURL string, opts ...func(*instagram.Config)) *instagram.Publisher {
	t.Helper()
	cfg := instagram.Config{
		UserID:  "17841405362640001",
		Token:   "user-token",
		BaseURL: baseURL,
		// Tests must not wait ~15s between status polls.
		Poll: graphapi.PollTiming{Start: 5 * time.Millisecond, Interval: 5 * time.Millisecond, Deadline: 2 * time.Second},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p, err := instagram.New(cfg)
	if err != nil {
		t.Fatalf("instagram publisher: %v", err)
	}
	return p
}

func TestInstagramAdapterPublishesImageContainer(t *testing.T) {
	stub := &igStub{statusScript: []string{"FINISHED"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, srv.URL)})
	const mediaURL = "https://cdn.example.com/photo.jpg"
	id, _ := e.createWithMedia("hello instagram", "instagram", mediaURL, "image", 2*time.Second)
	got := e.waitFor(id, statusIs("published"))

	if got["external_post_id"] != igMediaID {
		t.Fatalf("external_post_id: %v", got["external_post_id"])
	}
	if stub.createCalls != 1 || stub.publishCalls != 1 {
		t.Fatalf("calls: create=%d publish=%d, want 1/1", stub.createCalls, stub.publishCalls)
	}
	if stub.statusCalls < 1 {
		t.Fatalf("container status was never polled")
	}
	if stub.lastImageURL != mediaURL {
		t.Fatalf("image_url: %v", stub.lastImageURL)
	}
	if stub.lastCaption != "hello instagram" {
		t.Fatalf("caption: %v", stub.lastCaption)
	}
	if stub.lastCreation != igContainerID {
		t.Fatalf("creation_id: %v", stub.lastCreation)
	}
}

func TestInstagramAdapterPollsUntilFinished(t *testing.T) {
	// A container still processing must be polled, not published: the adapter
	// waits for FINISHED before handing the container to media_publish.
	stub := &igStub{statusScript: []string{"IN_PROGRESS", "IN_PROGRESS", "FINISHED"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, srv.URL)})
	id, _ := e.createWithMedia("patience", "instagram", "https://cdn.example.com/a.jpg", "image", 2*time.Second)
	got := e.waitFor(id, statusIs("published"))

	if got["external_post_id"] != igMediaID {
		t.Fatalf("external_post_id: %v", got["external_post_id"])
	}
	if stub.statusCalls < 3 {
		t.Fatalf("polls: want at least 3, got %d", stub.statusCalls)
	}
}

func TestInstagramAdapterContainerTimeoutIsRetryable(t *testing.T) {
	// IN_PROGRESS when the bounded poll deadline passes must not fail the
	// post: it stays scheduled with backoff and a fresh container on retry.
	stub := &igStub{statusScript: []string{"IN_PROGRESS"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, srv.URL, func(c *instagram.Config) {
		c.Poll = graphapi.PollTiming{Start: time.Millisecond, Interval: time.Millisecond, Deadline: 50 * time.Millisecond}
	})})
	id, _ := e.createWithMedia("slow upload", "instagram", "https://cdn.example.com/a.jpg", "image", 2*time.Second)

	got := e.waitFor(id, func(m map[string]any) bool { return m["last_error"] != nil })
	if got["status"] != "scheduled" {
		t.Fatalf("container timeout must be retryable, got status %v", got["status"])
	}
	if !strings.Contains(got["last_error"].(string), "container") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
	if stub.publishCalls != 0 {
		t.Fatalf("media_publish must never run for an unfinished container")
	}

	// The retry heals: the next attempt sees FINISHED.
	stub.mu.Lock()
	stub.statusScript = []string{"FINISHED"}
	stub.mu.Unlock()
	if _, err := e.db.Exec(`UPDATE scheduled_posts SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatalf("force retry: %v", err)
	}
	e.waitFor(id, statusIs("published"))
	if stub.publishCalls != 1 {
		t.Fatalf("publish after retry: want 1, got %d", stub.publishCalls)
	}
}

func TestInstagramAdapterContainerErrorIsPermanent(t *testing.T) {
	// A container that reports ERROR is dead: retrying cannot revive it.
	stub := &igStub{statusScript: []string{"ERROR"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, srv.URL)}, withMaxAttempts(5))
	id, _ := e.createWithMedia("bad container", "instagram", "https://cdn.example.com/a.jpg", "image", 2*time.Second)
	got := e.waitFor(id, statusIs("failed"))

	if !strings.Contains(got["last_error"].(string), "container") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
	if stub.publishCalls != 0 {
		t.Fatalf("media_publish must never run for an errored container")
	}
}

func TestInstagramAdapterRateLimitIsRetryable(t *testing.T) {
	// 429 from media_publish: retryable, publishes once the budget clears.
	stub := &igStub{statusScript: []string{"FINISHED"}, publishFailTimes: 1, publishFailStatus: http.StatusTooManyRequests}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, srv.URL)})
	id, _ := e.createWithMedia("rate limited", "instagram", "https://cdn.example.com/a.jpg", "image", 2*time.Second)

	got := e.waitFor(id, func(m map[string]any) bool { return m["last_error"] != nil })
	if got["status"] != "scheduled" {
		t.Fatalf("429 must be retryable, got status %v", got["status"])
	}
	if _, err := e.db.Exec(`UPDATE scheduled_posts SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatalf("force retry: %v", err)
	}
	got = e.waitFor(id, statusIs("published"))
	if got["external_post_id"] != igMediaID {
		t.Fatalf("external_post_id after retry: %v", got["external_post_id"])
	}
}

func TestInstagramAdapterDailyBudgetHoldsSecondPost(t *testing.T) {
	stub := &igStub{statusScript: []string{"FINISHED"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, srv.URL, func(c *instagram.Config) {
		c.DailyLimit = 1
	})})
	first, _ := e.createWithMedia("first", "instagram", "https://cdn.example.com/a.jpg", "image", 2*time.Second)
	e.waitFor(first, statusIs("published"))

	second, _ := e.createWithMedia("second", "instagram", "https://cdn.example.com/b.jpg", "image", 2*time.Second)
	e.waitFor(second, func(m map[string]any) bool { return m["status"] == "scheduled" })

	// Give the worker a few more ticks to (not) publish it.
	time.Sleep(6 * time.Second)
	if got := e.get(second); got["status"] != "scheduled" {
		t.Fatalf("second post over budget: %v", got)
	}
	if stub.publishCalls != 1 {
		t.Fatalf("publishes: want 1 (budget held), got %d", stub.publishCalls)
	}
}

func TestInstagramAdapterExpiredLeasePreventsDuplicatePublish(t *testing.T) {
	// A container-creation call that outlives the lease must not publish on a
	// lost lease: recovery requeues the row and the next attempt does the
	// publish, exactly once.
	stub := &igStub{
		statusScript:    []string{"FINISHED"},
		createDelayOnce: 700 * time.Millisecond,
	}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t,
		[]app.Publisher{newInstagramPublisher(t, srv.URL, func(c *instagram.Config) {
			c.Poll = graphapi.PollTiming{Start: time.Millisecond, Interval: time.Millisecond, Deadline: 500 * time.Millisecond}
		})},
		withLease(250*time.Millisecond))
	id, _ := e.createWithMedia("lost lease", "instagram", "https://cdn.example.com/a.jpg", "image", 2*time.Second)
	got := e.waitFor(id, statusIs("published"))

	if stub.publishCalls != 1 {
		t.Fatalf("publishes across attempts: want exactly 1, got %d", stub.publishCalls)
	}
	if got["external_post_id"] != igMediaID {
		t.Fatalf("external_post_id: %v", got["external_post_id"])
	}
}

func TestCreateSchedule_InstagramRules(t *testing.T) {
	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, "http://localhost:1/none")}, withoutWorker())

	// Instagram cannot post text alone.
	resp, raw := e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      "caption only",
		"scheduled_at": atIn(time.Hour),
		"platform":     "instagram",
	}, e.token)
	if resp.StatusCode != 422 || decode(t, raw)["code"] != "media_required" {
		t.Fatalf("no media: %d %s", resp.StatusCode, raw)
	}

	// Images only for v1; video is rejected at the API edge.
	resp, raw = e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      "a clip",
		"scheduled_at": atIn(time.Hour),
		"platform":     "instagram",
		"media":        map[string]any{"url": "https://cdn.example.com/clip.mp4", "kind": "video"},
	}, e.token)
	if resp.StatusCode != 422 || decode(t, raw)["code"] != "unsupported_media_type" {
		t.Fatalf("video to instagram: %d %s", resp.StatusCode, raw)
	}

	// Captions are 2,200 characters (rune-counted, unlike Threads).
	resp, raw = e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      strings.Repeat("a", 2201),
		"scheduled_at": atIn(time.Hour),
		"platform":     "instagram",
		"media":        map[string]any{"url": "https://cdn.example.com/a.jpg", "kind": "image"},
	}, e.token)
	if resp.StatusCode != 422 || decode(t, raw)["code"] != "platform_limit_exceeded" {
		t.Fatalf("over caption limit: %d %s", resp.StatusCode, raw)
	}

	resp, raw = e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      strings.Repeat("a", 2200),
		"scheduled_at": atIn(time.Hour),
		"platform":     "instagram",
		"media":        map[string]any{"url": "https://cdn.example.com/a.jpg", "kind": "image"},
	}, e.token)
	if resp.StatusCode != 201 {
		t.Fatalf("at caption limit: want 201, got %d: %s", resp.StatusCode, raw)
	}
}

func TestPlatformsListDeclaresInstagramRules(t *testing.T) {
	e := newEnv(t, []app.Publisher{newInstagramPublisher(t, "http://localhost:1/none")}, withoutWorker())
	resp, raw := e.request(http.MethodGet, "/api/v1/platforms", nil, e.token)
	if resp.StatusCode != 200 {
		t.Fatalf("platforms: %d %s", resp.StatusCode, raw)
	}

	list, _ := decode(t, raw)["platforms"].([]any)
	for _, entry := range list {
		p, _ := entry.(map[string]any)
		if name, _ := p["name"].(string); name != "instagram" {
			continue
		}
		if unit, _ := p["max_text_length_unit"].(string); unit != "characters" {
			t.Fatalf("instagram captions must stay rune-counted until verified live: unit=%q", unit)
		}
		if requiresMedia, _ := p["requires_media"].(bool); !requiresMedia {
			t.Fatalf("instagram must require media: %v", p)
		}
		kinds, _ := p["media_kinds"].([]any)
		if len(kinds) != 1 || kinds[0] != "image" {
			t.Fatalf("instagram media kinds: %v", kinds)
		}
		return
	}
	t.Fatalf("instagram missing from platforms: %s", raw)
}
