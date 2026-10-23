// Package e2e drives the whole service the way a client does: real HTTP, real
// SQLite file, real worker. There are no unit tests by design.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/bootstrap"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

const testToken = "test-token"

type envOpts struct {
	tick        time.Duration
	batch       int
	lease       time.Duration
	maxAttempts int
	startWorker bool
}

type envOption func(*envOpts)

func withTick(d time.Duration) envOption  { return func(o *envOpts) { o.tick = d } }
func withLease(d time.Duration) envOption { return func(o *envOpts) { o.lease = d } }
func withMaxAttempts(n int) envOption     { return func(o *envOpts) { o.maxAttempts = n } }
func withoutWorker() envOption            { return func(o *envOpts) { o.startWorker = false } }
func withBatch(n int) envOption           { return func(o *envOpts) { o.batch = n } }

type env struct {
	t      *testing.T
	token  string
	app    *bootstrap.App
	server *httptest.Server
	client *http.Client
	db     *sql.DB
	cancel context.CancelFunc
}

// newEnv builds the application with the given publishers and starts the worker
// unless told otherwise.
func newEnv(t *testing.T, publishers []app.Publisher, opts ...envOption) *env {
	t.Helper()
	o := envOpts{tick: 25 * time.Millisecond, batch: 10, lease: 2 * time.Second, maxAttempts: 5, startWorker: true}
	for _, opt := range opts {
		opt(&o)
	}

	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	application, err := bootstrap.New(ctx, bootstrap.Config{
		DatabasePath: filepath.Join(t.TempDir(), "scheduler.db"),
		APIToken:     testToken,
		Publishers:   publishers,
		Tick:         o.tick,
		Batch:        o.batch,
		Lease:        o.lease,
		MaxAttempts:  o.maxAttempts,
		Logger:       logger,
	})
	if err != nil {
		cancel()
		t.Fatalf("bootstrap: %v", err)
	}

	server := httptest.NewServer(application.Handler)
	e := &env{
		t:      t,
		token:  testToken,
		app:    application,
		server: server,
		client: server.Client(),
		db:     application.DB,
		cancel: cancel,
	}
	t.Cleanup(func() {
		cancel()
		server.Close()
		_ = application.Close()
	})
	if o.startWorker {
		go application.Worker.Run(ctx)
	}
	return e
}

func (e *env) request(method, path string, body any, token string) (*http.Response, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", string(raw), err)
	}
	return out
}

func items(t *testing.T, raw []byte) ([]map[string]any, any) {
	t.Helper()
	m := decode(t, raw)
	rawItems, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("no items in %s", string(raw))
	}
	out := make([]map[string]any, 0, len(rawItems))
	for _, it := range rawItems {
		out = append(out, it.(map[string]any))
	}
	return out, m["next_cursor"]
}

// create schedules a post and returns its id and version.
func (e *env) create(content, scheduledAt, platform string) (string, int) {
	e.t.Helper()
	body := map[string]any{"content": content, "scheduled_at": scheduledAt}
	if platform != "" {
		body["platform"] = platform
	}
	resp, raw := e.request(http.MethodPost, "/api/v1/schedules", body, e.token)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create: want 201, got %d: %s", resp.StatusCode, raw)
	}
	m := decode(e.t, raw)
	return m["id"].(string), int(m["version"].(float64))
}

func (e *env) get(id string) map[string]any {
	e.t.Helper()
	resp, raw := e.request(http.MethodGet, "/api/v1/schedules/"+id, nil, e.token)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("get %s: want 200, got %d: %s", id, resp.StatusCode, raw)
	}
	return decode(e.t, raw)
}

// waitFor polls the API until pred is satisfied.
func (e *env) waitFor(id string, pred func(map[string]any) bool) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp, raw := e.request(http.MethodGet, "/api/v1/schedules/"+id, nil, e.token)
		if resp.StatusCode == http.StatusOK {
			last = decode(e.t, raw)
			if pred(last) {
				return last
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("condition not met for %s; last state: %v", id, last)
	return nil
}

func statusIs(want string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["status"] == want }
}

// Time helpers. Timestamps are whole seconds in UTC, as the API requires.

func atIn(d time.Duration) string {
	return time.Now().UTC().Add(d).Truncate(time.Second).Format(time.RFC3339)
}

func pastTime() string {
	return time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
}

// Database helpers, for asserting what the service actually stored.

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func (e *env) str(query string, args ...any) string {
	e.t.Helper()
	var s sql.NullString
	if err := e.db.QueryRow(query, args...).Scan(&s); err != nil {
		e.t.Fatalf("string query %q: %v", query, err)
	}
	return s.String
}

func (e *env) nullStr(query string, args ...any) *string {
	e.t.Helper()
	var s sql.NullString
	if err := e.db.QueryRow(query, args...).Scan(&s); err != nil {
		e.t.Fatalf("string query %q: %v", query, err)
	}
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func (e *env) int(query string, args ...any) int {
	e.t.Helper()
	var n sql.NullInt64
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("int query %q: %v", query, err)
	}
	return int(n.Int64)
}

// requestRaw sends a body verbatim, for malformed-payload cases.
func (e *env) requestRaw(method, path, body, token string) (*http.Response, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return resp, raw
}
