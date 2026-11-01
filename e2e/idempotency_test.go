package e2e

// The Idempotency-Key contract on POST /api/v1/schedules (M6): a retried
// request with the same key and same body replays the stored response instead
// of creating a second schedule.

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

// requestKeyed sends a request with arbitrary extra headers and returns the
// status code plus the raw body.
func (e *env) requestKeyed(method, path string, body []byte, token string, headers map[string]string) (int, []byte) {
	e.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, raw
}

func idemBody(content string) []byte {
	return []byte(`{"content":"` + content + `","scheduled_at":"` + atIn(time.Hour) + `","platform":"fake"}`)
}

func TestIdempotencyKeyReplaysTheFirstCreate(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	status, raw := e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("once only"), e.token,
		map[string]string{"Idempotency-Key": "client-key-1"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	id1 := decode(t, raw)["id"].(string)

	// Same bytes, same key: the stored response replays, no second schedule.
	status, raw2 := e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("once only"), e.token,
		map[string]string{"Idempotency-Key": "client-key-1"})
	if status != http.StatusCreated {
		t.Fatalf("replay: %d %s", status, raw2)
	}
	if decode(t, raw2)["id"].(string) != id1 {
		t.Fatalf("replayed a different schedule: %s vs %s", raw, raw2)
	}

	var n int
	if err := e.db.QueryRow(`SELECT COUNT(1) FROM scheduled_posts WHERE id = ?`, id1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schedules created: want 1, got %d", n)
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

	status, raw := e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("first body"), e.token,
		map[string]string{"Idempotency-Key": "reused-key"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}

	status, raw = e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("second body"), e.token,
		map[string]string{"Idempotency-Key": "reused-key"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched key: want 422, got %d %s", status, raw)
	}
	if decode(t, raw)["code"] != "idempotency_key_mismatch" {
		t.Fatalf("code: %s", raw)
	}
}

func TestIdempotencyKeysArePerKey(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())

	// Different keys with the same body create two schedules: a key says
	// "this exact intent, once", it is not a duplicate-content filter.
	s1, raw1 := e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("same body"), e.token,
		map[string]string{"Idempotency-Key": "key-a"})
	s2, raw2 := e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("same body"), e.token,
		map[string]string{"Idempotency-Key": "key-b"})
	if s1 != http.StatusCreated || s2 != http.StatusCreated {
		t.Fatalf("%d %s | %d %s", s1, raw1, s2, raw2)
	}
	if decode(t, raw1)["id"] == decode(t, raw2)["id"] {
		t.Fatalf("different keys must create distinct schedules")
	}

	// Without a key: plain duplicate, exactly like the API was before M6.
	s3, raw3 := e.requestKeyed(http.MethodPost, "/api/v1/schedules", idemBody("same body"), e.token, nil)
	if s3 != http.StatusCreated {
		t.Fatalf("no key: %d %s", s3, raw3)
	}

	var all int
	if err := e.db.QueryRow(`SELECT COUNT(1) FROM scheduled_posts`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != 3 {
		t.Fatalf("total schedules: want 3, got %d", all)
	}
}
