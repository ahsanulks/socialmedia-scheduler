package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/fake"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

func fakePublisher(t *testing.T, platform string, opts ...fake.Option) *fake.Publisher {
	t.Helper()
	p, err := fake.New(platform, opts...)
	if err != nil {
		t.Fatalf("fake publisher %q: %v", platform, err)
	}
	return p
}

func TestCreateScheduleStoresAndReturnsIt(t *testing.T) {
	pub := fakePublisher(t, "threads")
	e := newEnv(t, []app.Publisher{pub})
	when := atIn(time.Hour)

	id, version := e.create("Hello Threads", when, "threads")
	if version != 1 {
		t.Fatalf("version: want 1, got %d", version)
	}
	got := e.get(id)
	if got["content"] != "Hello Threads" {
		t.Fatalf("content: %v", got["content"])
	}
	if got["status"] != "scheduled" {
		t.Fatalf("status: %v", got["status"])
	}
	if got["platform"] != "threads" {
		t.Fatalf("platform: %v", got["platform"])
	}
	if got["scheduled_at"] != when {
		t.Fatalf("scheduled_at: want %v, got %v", when, got["scheduled_at"])
	}
	if !strings.HasSuffix(got["created_at"].(string), "Z") {
		t.Fatalf("created_at must be UTC: %v", got["created_at"])
	}

	if n := e.count(`SELECT COUNT(1) FROM scheduled_posts WHERE id = ?`, id); n != 1 {
		t.Fatalf("write model rows: %d", n)
	}
	if n := e.count(`SELECT COUNT(1) FROM schedules_read WHERE id = ?`, id); n != 1 {
		t.Fatalf("read model rows: %d", n)
	}
	if n := e.count(`SELECT COUNT(1) FROM domain_events WHERE aggregate_id = ? AND event_type = 'PostScheduled'`, id); n != 1 {
		t.Fatalf("domain events: %d", n)
	}
}

func TestCreateValidation(t *testing.T) {
	pub := fakePublisher(t, "threads")
	e := newEnv(t, []app.Publisher{pub})

	cases := []struct {
		name     string
		body     map[string]any
		wantStat int
		wantCode string
	}{
		{"empty content", map[string]any{"content": "", "scheduled_at": atIn(time.Hour)}, 422, "validation_failed"},
		{"content over domain limit", map[string]any{"content": strings.Repeat("a", 5001), "scheduled_at": atIn(time.Hour)}, 422, "validation_failed"},
		{"past time", map[string]any{"content": "x", "scheduled_at": pastTime()}, 422, "validation_failed"},
		{"non UTC time", map[string]any{"content": "x", "scheduled_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05+07:00")}, 422, "validation_failed"},
		{"missing time", map[string]any{"content": "x"}, 422, "validation_failed"},
		{"unknown platform", map[string]any{"content": "x", "scheduled_at": atIn(time.Hour), "platform": "myspace"}, 422, "unsupported_platform"},
		{"content over platform limit", map[string]any{"content": strings.Repeat("a", 501), "scheduled_at": atIn(time.Hour)}, 422, "platform_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := e.request(http.MethodPost, "/api/v1/schedules", tc.body, e.token)
			if resp.StatusCode != tc.wantStat {
				t.Fatalf("status: want %d, got %d: %s", tc.wantStat, resp.StatusCode, raw)
			}
			if got := decode(t, raw)["code"]; got != tc.wantCode {
				t.Fatalf("code: want %s, got %v", tc.wantCode, got)
			}
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		resp, raw := e.requestRaw(http.MethodPost, "/api/v1/schedules", `{"content":`, e.token)
		if resp.StatusCode != 400 {
			t.Fatalf("status: want 400, got %d: %s", resp.StatusCode, raw)
		}
	})
}

// TestCreateValidation_CountsTextInBytes pins the Threads counting rule:
// Meta counts the 500-character limit in UTF-8 bytes (emoji are multi-byte),
// so a 300-emoji post (300 runes, 1200 bytes) must be rejected even though a
// rune count would let it through.
func TestCreateValidation_CountsTextInBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no Graph call expected during validation")
	}))
	defer srv.Close()
	e := newEnv(t, []app.Publisher{newThreadsPublisher(t, srv.URL+"/v1.0")}, withoutWorker())

	cases := []struct {
		name     string
		content  string
		wantStat int
	}{
		{"125 emojis fit exactly in 500 bytes", strings.Repeat("\U0001F97B", 125), 201},
		{"126 emojis exceed 500 bytes", strings.Repeat("\U0001F97B", 126), 422},
		{"300 emojis pass a rune check but exceed bytes", strings.Repeat("\U0001F97B", 300), 422},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"content": tc.content, "scheduled_at": atIn(time.Hour)}
			resp, raw := e.request(http.MethodPost, "/api/v1/schedules", body, e.token)
			if resp.StatusCode != tc.wantStat {
				t.Fatalf("status: want %d, got %d: %s", tc.wantStat, resp.StatusCode, raw)
			}
			if tc.wantStat == 201 {
				return
			}
			if got := decode(t, raw)["code"]; got != "platform_limit_exceeded" {
				t.Fatalf("code: want platform_limit_exceeded, got %v", got)
			}
		})
	}
}

func TestAuthAndHealth(t *testing.T) {
	e := newEnv(t, nil)

	for _, token := range []string{"", "wrong-token"} {
		resp, _ := e.request(http.MethodGet, "/api/v1/schedules", nil, token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: want 401, got %d", token, resp.StatusCode)
		}
	}
	resp, _ := e.request(http.MethodGet, "/healthz", nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	resp, _ = e.request(http.MethodGet, "/readyz", nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("readyz: %d", resp.StatusCode)
	}
}

func TestListSchedulesFiltersAndPaginates(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "threads"), fakePublisher(t, "alpha")})

	t1 := atIn(time.Hour)
	t2 := atIn(2 * time.Hour)
	t3 := atIn(3 * time.Hour)
	id1, _ := e.create("one", t1, "threads")
	id2, _ := e.create("two", t2, "alpha")
	id3, _ := e.create("three", t3, "threads")

	resp, _ := e.request(http.MethodDelete, "/api/v1/schedules/"+id2, nil, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}

	// Default list excludes canceled and is sorted ascending.
	_, raw := e.request(http.MethodGet, "/api/v1/schedules", nil, e.token)
	list, cursor := items(t, raw)
	if len(list) != 2 {
		t.Fatalf("default list: want 2 items, got %d", len(list))
	}
	if list[0]["id"] != id1 || list[1]["id"] != id3 {
		t.Fatalf("order: %v", list)
	}
	if cursor != nil {
		t.Fatalf("cursor: want nil, got %v", cursor)
	}

	// Filter by platform.
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?platform=threads", nil, e.token)
	list, _ = items(t, raw)
	if len(list) != 2 {
		t.Fatalf("platform filter: %d", len(list))
	}

	// Filter by status, including canceled explicitly.
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?status=canceled", nil, e.token)
	list, _ = items(t, raw)
	if len(list) != 1 || list[0]["id"] != id2 {
		t.Fatalf("status filter: %v", list)
	}

	// Filter by time window. The default status filter still applies, so the
	// canceled post inside the window stays hidden.
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?from="+t3+"&to="+atIn(4*time.Hour), nil, e.token)
	list, _ = items(t, raw)
	if len(list) != 1 || list[0]["id"] != id3 {
		t.Fatalf("time filter: %v", list)
	}
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?status=canceled&from="+t2+"&to="+t3, nil, e.token)
	list, _ = items(t, raw)
	if len(list) != 1 || list[0]["id"] != id2 {
		t.Fatalf("time filter with canceled status: %v", list)
	}

	// Descending order.
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?order=desc", nil, e.token)
	list, _ = items(t, raw)
	if len(list) != 2 || list[0]["id"] != id3 {
		t.Fatalf("desc order: %v", list)
	}

	// Keyset pagination must not repeat or skip rows.
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?limit=1", nil, e.token)
	list, cursor = items(t, raw)
	if len(list) != 1 || list[0]["id"] != id1 {
		t.Fatalf("page 1: %v", list)
	}
	next, ok := cursor.(string)
	if !ok || next == "" {
		t.Fatalf("page 1 cursor: %v", cursor)
	}
	_, raw = e.request(http.MethodGet, "/api/v1/schedules?limit=1&cursor="+next, nil, e.token)
	list, _ = items(t, raw)
	if len(list) != 1 || list[0]["id"] != id3 {
		t.Fatalf("page 2: %v", list)
	}

	// A bad cursor is a client error.
	resp, raw = e.request(http.MethodGet, "/api/v1/schedules?cursor=not-a-cursor", nil, e.token)
	if resp.StatusCode != 422 {
		t.Fatalf("bad cursor: %d %s", resp.StatusCode, raw)
	}
}

func TestUpdateSchedule(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "threads")})
	id, version := e.create("original", atIn(time.Hour), "threads")

	// Edit the content: version advances by one.
	resp, raw := e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{
		"content":          "edited",
		"expected_version": version,
	}, e.token)
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, raw)
	}
	got := decode(t, raw)
	if int(got["version"].(float64)) != version+1 {
		t.Fatalf("version: want %d, got %v", version+1, got["version"])
	}
	if e.get(id)["content"] != "edited" {
		t.Fatalf("content not updated")
	}

	// Edit content and time: version advances by two.
	newTime := atIn(2 * time.Hour)
	resp, raw = e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{
		"content":          "edited again",
		"scheduled_at":     newTime,
		"expected_version": version + 1,
	}, e.token)
	if resp.StatusCode != 200 {
		t.Fatalf("patch both: %d %s", resp.StatusCode, raw)
	}
	got = decode(t, raw)
	if int(got["version"].(float64)) != version+3 {
		t.Fatalf("version after two changes: want %d, got %v", version+3, got["version"])
	}
	if e.get(id)["scheduled_at"] != newTime {
		t.Fatalf("scheduled_at not updated")
	}

	// A stale expected_version conflicts.
	resp, raw = e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{
		"content":          "stale",
		"expected_version": version,
	}, e.token)
	if resp.StatusCode != 409 || decode(t, raw)["code"] != "version_conflict" {
		t.Fatalf("stale version: %d %s", resp.StatusCode, raw)
	}

	// expected_version is mandatory.
	resp, raw = e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{"content": "x"}, e.token)
	if resp.StatusCode != 422 {
		t.Fatalf("missing expected_version: %d %s", resp.StatusCode, raw)
	}

	// At least one field must be present.
	resp, raw = e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{"expected_version": version + 3}, e.token)
	if resp.StatusCode != 422 {
		t.Fatalf("empty patch: %d %s", resp.StatusCode, raw)
	}

	// Unknown ids are 404.
	resp, raw = e.request(http.MethodPatch, "/api/v1/schedules/0192f3c4-1111-7222-8333-444455556666", map[string]any{
		"content":          "x",
		"expected_version": 1,
	}, e.token)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown id: %d %s", resp.StatusCode, raw)
	}
}

func TestCancelSchedule(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "threads")})
	id, _ := e.create("cancel me", atIn(time.Hour), "threads")

	resp, _ := e.request(http.MethodDelete, "/api/v1/schedules/"+id, nil, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	got := e.get(id)
	if got["status"] != "canceled" {
		t.Fatalf("status: %v", got["status"])
	}

	// Canceled schedules drop out of the default list.
	_, raw := e.request(http.MethodGet, "/api/v1/schedules", nil, e.token)
	list, _ := items(t, raw)
	if len(list) != 0 {
		t.Fatalf("default list should be empty: %v", list)
	}

	// The row is kept for audit.
	if n := e.count(`SELECT COUNT(1) FROM scheduled_posts WHERE id = ? AND status = 'canceled'`, id); n != 1 {
		t.Fatalf("canceled row missing")
	}

	// Cancelling again is a no-op.
	resp, _ = e.request(http.MethodDelete, "/api/v1/schedules/"+id, nil, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second cancel: %d", resp.StatusCode)
	}

	// A stale expected_version conflicts on a live schedule.
	other, _ := e.create("still live", atIn(time.Hour), "threads")
	resp, raw = e.request(http.MethodDelete, "/api/v1/schedules/"+other+"?expected_version=99", nil, e.token)
	if resp.StatusCode != 409 {
		t.Fatalf("stale cancel: %d %s", resp.StatusCode, raw)
	}
	if e.get(other)["status"] != "scheduled" {
		t.Fatalf("stale cancel must not change the schedule")
	}

	// Unknown ids are 404.
	resp, _ = e.request(http.MethodDelete, "/api/v1/schedules/0192f3c4-1111-7222-8333-444455556666", nil, e.token)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
}

func TestPlatformsEndpoint(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "threads")})
	resp, raw := e.request(http.MethodGet, "/api/v1/platforms", nil, e.token)
	if resp.StatusCode != 200 {
		t.Fatalf("platforms: %d %s", resp.StatusCode, raw)
	}
	m := decode(t, raw)
	list, ok := m["platforms"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("platforms payload: %s", raw)
	}
	info := list[0].(map[string]any)
	if info["name"] != "threads" {
		t.Fatalf("platform name: %v", info["name"])
	}
	if int(info["max_text_length"].(float64)) != 500 {
		t.Fatalf("max text length: %v", info["max_text_length"])
	}
}

func TestTerminalStatesCannotBeMutated(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "alpha")})
	id, _ := e.create("published soon", atIn(2*time.Second), "alpha")
	e.waitFor(id, statusIs("published"))

	version := int(e.get(id)["version"].(float64))

	resp, raw := e.request(http.MethodDelete, "/api/v1/schedules/"+id, nil, e.token)
	if resp.StatusCode != 409 || decode(t, raw)["code"] != "invalid_state" {
		t.Fatalf("delete published: %d %s", resp.StatusCode, raw)
	}

	resp, raw = e.request(http.MethodPatch, "/api/v1/schedules/"+id, map[string]any{
		"content":          "too late",
		"expected_version": version,
	}, e.token)
	if resp.StatusCode != 409 || decode(t, raw)["code"] != "invalid_state" {
		t.Fatalf("patch published: %d %s", resp.StatusCode, raw)
	}

	if e.get(id)["content"] != "published soon" {
		t.Fatalf("published schedule was modified")
	}
}

func TestCancelWhilePublishingConflicts(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "alpha")})
	id, _ := e.create("in flight", atIn(time.Hour), "alpha")

	// Simulate a publish that is currently running: a live lease.
	if _, err := e.db.Exec(`UPDATE scheduled_posts SET status = 'publishing', lease_until = ? WHERE id = ?`,
		time.Now().UTC().Add(time.Minute).UnixMilli(), id); err != nil {
		t.Fatalf("simulate in-flight publish: %v", err)
	}

	resp, raw := e.request(http.MethodDelete, "/api/v1/schedules/"+id, nil, e.token)
	if resp.StatusCode != 409 || decode(t, raw)["code"] != "invalid_state" {
		t.Fatalf("cancel while publishing: %d %s", resp.StatusCode, raw)
	}
}

func TestGetUnknownScheduleIsNotFound(t *testing.T) {
	e := newEnv(t, nil)
	resp, raw := e.request(http.MethodGet, "/api/v1/schedules/0192f3c4-1111-7222-8333-444455556666", nil, e.token)
	if resp.StatusCode != 404 || decode(t, raw)["code"] != "not_found" {
		t.Fatalf("want 404 not_found, got %d %s", resp.StatusCode, raw)
	}
	resp, _ = e.request(http.MethodGet, "/api/v1/schedules/not-a-uuid", nil, e.token)
	if resp.StatusCode != 404 {
		t.Fatalf("malformed id: want 404, got %d", resp.StatusCode)
	}
}
