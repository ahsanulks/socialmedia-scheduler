package e2e

// The Facebook Pages adapter, driven the way a client drives it: real HTTP,
// real SQLite, real worker, a stub standing in for graph.facebook.com.

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/facebook"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// feedStub stands in for the Facebook Graph API's /{page-id}/feed endpoint.
type feedStub struct {
	mu         sync.Mutex
	calls      int
	lastPath   string
	lastParams map[string]string

	failTimes  int // fail this many calls, then succeed
	failStatus int // status code for the failures
}

func (s *feedStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls++
		s.lastPath = r.URL.Path
		params := map[string]string{}
		for k := range r.PostForm {
			params[k] = r.PostForm.Get(k)
		}
		if s.failTimes > 0 && s.calls <= s.failTimes {
			w.WriteHeader(s.failStatus)
			_, _ = w.Write([]byte(`{"error":{"message":"cross-post was rate limited","code":1,"fbtrace_id":"AbC"}}`))
			return
		}
		s.lastParams = params
		_, _ = w.Write([]byte(`{"id":"2345678901_9876543210"}`))
	}
}

func (s *feedStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newFacebookPublisher(t *testing.T, baseURL string) *facebook.Publisher {
	t.Helper()
	p, err := facebook.New(facebook.Config{
		PageID:    "1234567890",
		PageToken: "page-token",
		BaseURL:   baseURL,
	})
	if err != nil {
		t.Fatalf("facebook publisher: %v", err)
	}
	return p
}

func TestFacebookAdapterPublishesThroughGraphAPI(t *testing.T) {
	stub := &feedStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newFacebookPublisher(t, srv.URL)})
	id, _ := e.create("hello from the page", atIn(2*time.Second), "facebook")
	got := e.waitFor(id, statusIs("published"))

	if got["external_post_id"] != "2345678901_9876543210" {
		t.Fatalf("external_post_id: %v", got["external_post_id"])
	}

	if stub.callCount() != 1 {
		t.Fatalf("single-call publish: want 1 call, got %d", stub.callCount())
	}
	if !strings.HasSuffix(stub.lastPath, "/1234567890/feed") {
		t.Fatalf("feed path: %v", stub.lastPath)
	}
	want := map[string]string{
		"message":      "hello from the page",
		"access_token": "page-token",
	}
	if stub.lastParams == nil || !reflect.DeepEqual(stub.lastParams, want) {
		t.Fatalf("feed params: want %v, got %v", want, stub.lastParams)
	}
}

func TestFacebookAdapterMapsRetryableError(t *testing.T) {
	stub := &feedStub{failTimes: 1, failStatus: http.StatusInternalServerError}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newFacebookPublisher(t, srv.URL)})
	id, _ := e.create("tentative", atIn(2*time.Second), "facebook")
	got := e.waitFor(id, func(m map[string]any) bool { return m["last_error"] != nil })

	if got["status"] != "scheduled" {
		t.Fatalf("500 must be retryable, got status %v", got["status"])
	}

	// Make the retry due now instead of waiting for the backoff.
	if _, err := e.db.Exec(`UPDATE scheduled_posts SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatalf("force retry: %v", err)
	}

	got = e.waitFor(id, statusIs("published"))
	if got["external_post_id"] != "2345678901_9876543210" {
		t.Fatalf("external_post_id after retry: %v", got["external_post_id"])
	}
	if stub.callCount() < 2 {
		t.Fatalf("retry did not reach the adapter again: %d calls", stub.callCount())
	}
}

func TestFacebookAdapterMapsRateLimitRetryable(t *testing.T) {
	// A 429 from the Graph API must be retryable, same as a 5xx: the post
	// stays scheduled with a backoff and publishes once the budget clears.
	stub := &feedStub{failTimes: 1, failStatus: http.StatusTooManyRequests}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newFacebookPublisher(t, srv.URL)})
	id, _ := e.create("rate limited", atIn(2*time.Second), "facebook")

	got := e.waitFor(id, func(m map[string]any) bool { return m["last_error"] != nil })
	if got["status"] != "scheduled" {
		t.Fatalf("429 must be retryable, got status %v", got["status"])
	}
	if !strings.Contains(got["last_error"].(string), "429") {
		t.Fatalf("last_error: %v", got["last_error"])
	}

	// Make the retry due now instead of waiting for the backoff.
	if _, err := e.db.Exec(`UPDATE scheduled_posts SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatalf("force retry: %v", err)
	}
	e.waitFor(id, statusIs("published"))
	if stub.callCount() != 2 {
		t.Fatalf("calls after retry: want 2, got %d", stub.callCount())
	}
}

func TestFacebookAdapterMapsPermanentError(t *testing.T) {
	// A Page-authorization failure (code 190 / expired token) must fail the
	// post for good, never retry it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Error validating access token","type":"OAuthException","code":190}}`))
	}))
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newFacebookPublisher(t, srv.URL)}, withMaxAttempts(5))
	id, _ := e.create("will fail", atIn(2*time.Second), "facebook")
	got := e.waitFor(id, statusIs("failed"))

	if !strings.Contains(got["last_error"].(string), "401") || !strings.Contains(got["last_error"].(string), "token") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
}

func TestCreateSchedule_FacebookRules(t *testing.T) {
	e := newEnv(t, []app.Publisher{newFacebookPublisher(t, "http://localhost:1/none")}, withoutWorker())

	// Beyond the domain's 5000-character sanity bound it is a validation
	// failure; the 63,206 composer ceiling stays as defense in depth inside
	// the adapter, unreachable through the API.
	resp, raw := e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      strings.Repeat("a", 5001),
		"scheduled_at": atIn(time.Hour),
		"platform":     "facebook",
	}, e.token)
	if resp.StatusCode != 422 || decode(t, raw)["code"] != "validation_failed" {
		t.Fatalf("over domain bound: %d %s", resp.StatusCode, raw)
	}

	// The domain max is accepted for facebook.
	resp, raw = e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      strings.Repeat("a", 5000),
		"scheduled_at": atIn(time.Hour),
		"platform":     "facebook",
	}, e.token)
	if resp.StatusCode != 201 {
		t.Fatalf("at domain max: want 201, got %d: %s", resp.StatusCode, raw)
	}

	// Facebook is text-only for v1: media is rejected with its own code.
	resp, raw = e.request(http.MethodPost, "/api/v1/schedules", map[string]any{
		"content":      "a caption",
		"scheduled_at": atIn(time.Hour),
		"platform":     "facebook",
		"media":        map[string]any{"url": "https://cdn.example.com/photo.jpg", "kind": "image"},
	}, e.token)
	if resp.StatusCode != 422 || decode(t, raw)["code"] != "unsupported_media_type" {
		t.Fatalf("media to facebook: %d %s", resp.StatusCode, raw)
	}
}
