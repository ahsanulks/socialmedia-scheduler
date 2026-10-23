package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/threads"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// graphStub is a stand-in for the Threads Graph API.
type graphStub struct {
	mu sync.Mutex

	containerCalls int
	publishCalls   int
	lastContainer  url.Values
	lastPublish    url.Values

	failContainerStatus int
	failPublishStatus   int
}

func (g *graphStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		defer g.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/threads_publish"):
			g.publishCalls++
			g.lastPublish = r.PostForm
			if g.failPublishStatus != 0 && g.publishCalls == 1 {
				w.WriteHeader(g.failPublishStatus)
				fmt.Fprint(w, `{"error":{"message":"publish exploded","code":2}}`)
				return
			}
			fmt.Fprint(w, `{"id":"post-1"}`)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			g.containerCalls++
			g.lastContainer = r.PostForm
			if g.failContainerStatus != 0 {
				w.WriteHeader(g.failContainerStatus)
				fmt.Fprint(w, `{"error":{"message":"container exploded","code":1}}`)
				return
			}
			fmt.Fprintf(w, `{"id":"container-%d"}`, g.containerCalls)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (g *graphStub) calls() (containers, publishes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.containerCalls, g.publishCalls
}

func newThreadsPublisher(t *testing.T, baseURL string) *threads.Publisher {
	t.Helper()
	p, err := threads.New(threads.Config{
		UserID:      "12345",
		AccessToken: "test-token",
		BaseURL:     baseURL,
	})
	if err != nil {
		t.Fatalf("threads publisher: %v", err)
	}
	return p
}

func TestThreadsAdapterPublishesThroughGraphAPI(t *testing.T) {
	stub := &graphStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newThreadsPublisher(t, srv.URL+"/v1.0")})
	id, _ := e.create("hello from the adapter", atIn(2*time.Second), "threads")
	got := e.waitFor(id, statusIs("published"))

	if got["external_post_id"] != "post-1" {
		t.Fatalf("external_post_id: %v", got["external_post_id"])
	}

	containers, publishes := stub.calls()
	if containers != 1 || publishes != 1 {
		t.Fatalf("graph calls: containers=%d publishes=%d", containers, publishes)
	}
	if stub.lastContainer.Get("text") != "hello from the adapter" {
		t.Fatalf("container text: %v", stub.lastContainer)
	}
	if stub.lastContainer.Get("media_type") != "TEXT" {
		t.Fatalf("container media_type: %v", stub.lastContainer)
	}
	if stub.lastPublish.Get("creation_id") != "container-1" {
		t.Fatalf("creation_id not propagated: %v", stub.lastPublish)
	}
	if stub.lastPublish.Get("access_token") != "test-token" {
		t.Fatalf("access token not sent")
	}
}

func TestThreadsAdapterMapsRetryableError(t *testing.T) {
	stub := &graphStub{failContainerStatus: http.StatusTooManyRequests}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newThreadsPublisher(t, srv.URL+"/v1.0")})
	id, _ := e.create("rate limited", atIn(2*time.Second), "threads")
	got := e.waitFor(id, func(m map[string]any) bool { return m["last_error"] != nil })

	if got["status"] != "scheduled" {
		t.Fatalf("429 must be retryable, got status %v", got["status"])
	}
	if n := e.count(`SELECT COUNT(1) FROM scheduled_posts WHERE id = ? AND next_attempt_at IS NOT NULL`, id); n != 1 {
		t.Fatalf("next_attempt_at not set")
	}
	if !strings.Contains(got["last_error"].(string), "429") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
}

func TestThreadsAdapterMapsPermanentError(t *testing.T) {
	stub := &graphStub{failContainerStatus: http.StatusBadRequest}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newThreadsPublisher(t, srv.URL+"/v1.0")})
	id, _ := e.create("rejected", atIn(2*time.Second), "threads")
	got := e.waitFor(id, statusIs("failed"))

	if !strings.Contains(got["last_error"].(string), "400") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
}

func TestThreadsAdapterReusesContainerOnRetry(t *testing.T) {
	stub := &graphStub{failPublishStatus: http.StatusInternalServerError}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	e := newEnv(t, []app.Publisher{newThreadsPublisher(t, srv.URL+"/v1.0")})
	id, _ := e.create("retry the publish", atIn(2*time.Second), "threads")

	// First attempt: container created, publish fails with a retryable 500.
	got := e.waitFor(id, func(m map[string]any) bool { return m["last_error"] != nil })
	if got["status"] != "scheduled" {
		t.Fatalf("500 must be retryable, got %v", got["status"])
	}
	containers, publishes := stub.calls()
	if containers != 1 || publishes != 1 {
		t.Fatalf("after first attempt: containers=%d publishes=%d", containers, publishes)
	}
	if ref := e.nullStr(`SELECT prepare_ref FROM scheduled_posts WHERE id = ?`, id); ref == nil || *ref != "container-1" {
		t.Fatalf("prepare reference not persisted: %v", ref)
	}

	// Make the retry due now instead of waiting for the one-minute backoff.
	if _, err := e.db.Exec(`UPDATE scheduled_posts SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatalf("force retry: %v", err)
	}

	got = e.waitFor(id, statusIs("published"))
	if got["external_post_id"] != "post-1" {
		t.Fatalf("external_post_id: %v", got["external_post_id"])
	}
	containers, publishes = stub.calls()
	if containers != 1 {
		t.Fatalf("retry must reuse the container, got %d container calls", containers)
	}
	if publishes != 2 {
		t.Fatalf("publish calls: want 2, got %d", publishes)
	}
	if stub.lastPublish.Get("creation_id") != "container-1" {
		t.Fatalf("retry used a different container: %v", stub.lastPublish)
	}
}
