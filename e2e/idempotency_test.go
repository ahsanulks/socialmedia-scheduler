package e2e

// The Idempotency-Key contract on POST /api/v1/schedules (M6): the key is
// reserved atomically before the create, so a retried request with the same
// key and same body replays the stored response, concurrent retries cannot
// create more than one schedule, and a different body under the same key is
// a client error.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/sqlite"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// requestKeyed sends a raw-body request with arbitrary extra headers and
// returns the full response so tests can inspect headers
// (Idempotency-Replayed, Location).
func (e *env) requestKeyed(method, path string, body []byte, token string, headers map[string]string) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read body: %w", err)
	}
	return resp, raw, nil
}

// keyedBody builds the create body ONCE per test: the replay contract is
// over raw bytes, so a test that recomputes its body per call can straddle a
// second boundary in the timestamp and flake into 422.
func keyedBody(content string) []byte {
	return []byte(`{"content":"` + content + `","scheduled_at":"` + atIn(time.Hour) + `","platform":"fake"}`)
}

// createWithKey fails the test when the transport itself fails; on main test
// threads Fatalf is valid.
func createWithKey(t *testing.T, e *env, body []byte, key string) (*http.Response, []byte) {
	t.Helper()
	resp, raw, err := e.requestKeyed(http.MethodPost, "/api/v1/schedules", body, e.token,
		map[string]string{"Idempotency-Key": key})
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func TestIdempotencyKeyReplaysTheFirstCreate(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	body := keyedBody("once only")
	resp, raw := createWithKey(t, e, body, "client-key-1")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	id1 := decode(t, raw)["id"].(string)
	firstLocation := resp.Header.Get("Location")

	// Same bytes, same key: the stored response replays, no second schedule.
	resp, raw2 := createWithKey(t, e, body, "client-key-1")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay: %d %s", resp.StatusCode, raw2)
	}
	if decode(t, raw2)["id"].(string) != id1 {
		t.Fatalf("replayed a different schedule: %s vs %s", raw, raw2)
	}
	if got := resp.Header.Get("Idempotency-Replayed"); got != "true" {
		t.Fatalf("Idempotency-Replayed: want true, got %q", got)
	}
	if got := resp.Header.Get("Location"); got != firstLocation {
		t.Fatalf("replayed Location: want %q, got %q", firstLocation, got)
	}

	// Rows only for the replayed id: the retry did not create anything.
	var all int
	if err := e.db.QueryRow(`SELECT COUNT(1) FROM scheduled_posts`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != 1 {
		t.Fatalf("total schedules: want 1, got %d", all)
	}
}

func TestIdempotencyKeyRejectsReusedKeyWithDifferentBody(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	resp, raw := createWithKey(t, e, keyedBody("first body"), "reused-key")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}

	resp, raw = createWithKey(t, e, keyedBody("second body"), "reused-key")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched key: want 422, got %d %s", resp.StatusCode, raw)
	}
	if decode(t, raw)["code"] != "idempotency_key_mismatch" {
		t.Fatalf("code: %s", raw)
	}
}

func TestIdempotencyKeysArePerKey(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	// Different keys with the same body create two schedules: a key says
	// "this exact intent, once", it is not a duplicate-content filter.
	body := keyedBody("same body")
	s1, raw1 := createWithKey(t, e, body, "key-a")
	s2, raw2 := createWithKey(t, e, body, "key-b")
	if s1.StatusCode != http.StatusCreated || s2.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s | %d %s", s1.StatusCode, raw1, s2.StatusCode, raw2)
	}
	if decode(t, raw1)["id"] == decode(t, raw2)["id"] {
		t.Fatalf("different keys must create distinct schedules")
	}

	// Without a key: plain duplicate, exactly like the API was before M6.
	resp, _ := e.requestRaw(http.MethodPost, "/api/v1/schedules", string(body), e.token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("no key: %d", resp.StatusCode)
	}

	var all int
	if err := e.db.QueryRow(`SELECT COUNT(1) FROM scheduled_posts`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != 3 {
		t.Fatalf("total schedules: want 3, got %d", all)
	}
}

func TestIdempotencyConcurrentRetriesCreateExactlyOneSchedule(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	// N racing identical requests: the atomic reservation must leave exactly
	// one schedule behind. Losers get either the replay (winner finished
	// first) or a 409 pointing at the in-flight winner.
	const n = 8
	body := keyedBody("racing body")
	type outcome struct {
		status   int
		replayed bool
		location string
	}
	results := make([]outcome, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // fire all at once
			resp, raw, reqErr := e.requestKeyed(http.MethodPost, "/api/v1/schedules", body, e.token,
				map[string]string{"Idempotency-Key": "race-key"})
			if reqErr != nil {
				t.Errorf("racer %d: %v", i, reqErr)
				return
			}
			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
				t.Errorf("racer %d: unexpected %d %s", i, resp.StatusCode, raw)
				return
			}
			if resp.StatusCode == http.StatusConflict && !strings.Contains(string(raw), "idempotency_in_progress") {
				t.Errorf("racer %d: conflict without code %s", i, raw)
			}
			results[i] = outcome{
				status:   resp.StatusCode,
				replayed: resp.Header.Get("Idempotency-Replayed") == "true",
				location: resp.Header.Get("Location"),
			}
		}(i)
	}
	close(start)
	wg.Wait()

	var created, replayed, conflicts int
	ids := map[string]bool{}
	for i, r := range results {
		switch {
		case r.status == http.StatusCreated:
			if r.replayed {
				replayed++
			} else {
				created++
			}
		case r.status == http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("racer %d: got neither 201 nor 409: %d", i, r.status)
		}
		if r.location != "" {
			ids[r.location] = true
		}
	}
	if created != 1 {
		t.Fatalf("want exactly one non-replayed 201, got %d (replayed %d, conflicts %d)",
			created, replayed, conflicts)
	}
	if created+replayed+conflicts != n {
		t.Fatalf("lost racers: %d + %d + %d != %d", created, replayed, conflicts, n)
	}
	if len(ids) != 1 {
		t.Fatalf("all responders must report one schedule location, got %v", ids)
	}
	var schedules int
	if err := e.db.QueryRow(`SELECT COUNT(1) FROM scheduled_posts`).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if schedules != 1 {
		t.Fatalf("schedules created: want 1, got %d", schedules)
	}

	// A trailing retry replays the winner (201, no new schedule).
	resp, raw := createWithKey(t, e, body, "race-key")
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("after race: %d %s replayed=%q", resp.StatusCode, raw, resp.Header.Get("Idempotency-Replayed"))
	}
}

func TestIdempotencyKeyValidationAndPruning(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	// A key over 255 characters is rejected before anything else happens.
	resp, raw := createWithKey(t, e, keyedBody("long"), strings.Repeat("k", 256))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "255") {
		t.Fatalf("oversized key: %d %s", resp.StatusCode, raw)
	}

	// An empty body with a key reserves then releases on the decode failure;
	// a well-formed retry with the same key must still create (no stuck key).
	resp, _ = createWithKey(t, e, []byte(""), "empty-body-key")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body: want 400, got %d", resp.StatusCode)
	}
	resp, raw = createWithKey(t, e, keyedBody("after failed attempt"), "empty-body-key")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry after failed reserve: %d %s", resp.StatusCode, raw)
	}

	// Pruning drops stored rows; afterwards the same key creates a fresh
	// schedule (the documented double-create risk of a pruned key).
	store := sqlite.NewIdempotencyStore(e.db)
	deleted, err := store.Prune(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("prune deleted: want 1, got %d", deleted)
	}
	resp, raw = createWithKey(t, e, keyedBody("after failed attempt"), "empty-body-key")
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotency-Replayed") == "true" {
		t.Fatalf("after prune: %d %s", resp.StatusCode, raw)
	}
}
