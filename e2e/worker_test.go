package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/fake"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

func TestWorkerPublishesDueSchedule(t *testing.T) {
	pub := fakePublisher(t, "alpha")
	e := newEnv(t, []app.Publisher{pub})

	id, _ := e.create("ship it", atIn(2*time.Second), "alpha")
	got := e.waitFor(id, statusIs("published"))

	if got["external_post_id"] == nil || got["external_post_id"] == "" {
		t.Fatalf("external_post_id missing: %v", got)
	}
	if got["published_at"] == nil {
		t.Fatalf("published_at missing: %v", got)
	}
	if n := e.count(`SELECT COUNT(1) FROM publish_attempts WHERE schedule_id = ? AND outcome = 'succeeded'`, id); n != 1 {
		t.Fatalf("attempt log: want 1 succeeded row, got %d", n)
	}
	if n := e.count(`SELECT COUNT(1) FROM domain_events WHERE aggregate_id = ? AND event_type = 'PostPublished'`, id); n != 1 {
		t.Fatalf("PostPublished events: %d", n)
	}
	if n := e.count(`SELECT COUNT(1) FROM domain_events WHERE aggregate_id = ? AND event_type = 'PostPrepared'`, id); n != 1 {
		t.Fatalf("PostPrepared events: %d", n)
	}

	published := pub.Published()
	if len(published) != 1 {
		t.Fatalf("adapter calls: %d", len(published))
	}
	if published[0].Content != "ship it" {
		t.Fatalf("published content: %q", published[0].Content)
	}
	if published[0].Ref == "" {
		t.Fatalf("published without a prepare reference: %+v", published[0])
	}
}

func TestRetryableFailureReturnsToScheduled(t *testing.T) {
	pub := fakePublisher(t, "alpha")
	e := newEnv(t, []app.Publisher{pub})
	pub.FailNext(app.Retryable(errors.New("boom"), "alpha: temporary outage"))

	id, _ := e.create("retry me", atIn(2*time.Second), "alpha")
	got := e.waitFor(id, func(m map[string]any) bool {
		attempts, _ := m["attempts"].(float64)
		return attempts >= 1 && m["last_error"] != nil
	})

	if got["status"] != "scheduled" {
		t.Fatalf("status: want scheduled, got %v", got["status"])
	}
	if !strings.Contains(got["last_error"].(string), "temporary outage") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
	if n := e.count(`SELECT COUNT(1) FROM scheduled_posts WHERE id = ? AND next_attempt_at IS NOT NULL`, id); n != 1 {
		t.Fatalf("next_attempt_at not set")
	}
	if n := e.count(`SELECT COUNT(1) FROM publish_attempts WHERE schedule_id = ? AND outcome = 'retryable'`, id); n != 1 {
		t.Fatalf("attempt log outcome: %d", n)
	}
}

func TestPermanentFailureMarksFailed(t *testing.T) {
	pub := fakePublisher(t, "alpha")
	e := newEnv(t, []app.Publisher{pub})
	pub.AlwaysFail(app.Permanent(errors.New("rejected"), "alpha: content rejected"))

	id, _ := e.create("will fail", atIn(2*time.Second), "alpha")
	got := e.waitFor(id, statusIs("failed"))

	if !strings.Contains(got["last_error"].(string), "content rejected") {
		t.Fatalf("last_error: %v", got["last_error"])
	}
	if n := e.count(`SELECT COUNT(1) FROM scheduled_posts WHERE id = ? AND failed_at IS NOT NULL`, id); n != 1 {
		t.Fatalf("failed_at not set")
	}
	if n := e.count(`SELECT COUNT(1) FROM publish_attempts WHERE schedule_id = ? AND outcome = 'permanent'`, id); n != 1 {
		t.Fatalf("attempt log outcome: %d", n)
	}
}

func TestPlatformDispatch(t *testing.T) {
	alpha := fakePublisher(t, "alpha")
	beta := fakePublisher(t, "beta")
	e := newEnv(t, []app.Publisher{alpha, beta})

	idA, _ := e.create("to alpha", atIn(2*time.Second), "alpha")
	idB, _ := e.create("to beta", atIn(2*time.Second), "beta")
	e.waitFor(idA, statusIs("published"))
	e.waitFor(idB, statusIs("published"))

	if got := alpha.Published(); len(got) != 1 || got[0].Content != "to alpha" {
		t.Fatalf("alpha received: %+v", got)
	}
	if got := beta.Published(); len(got) != 1 || got[0].Content != "to beta" {
		t.Fatalf("beta received: %+v", got)
	}
}

func TestLeaseRecoveryRequeuesStuckPublish(t *testing.T) {
	pub := fakePublisher(t, "alpha")
	e := newEnv(t, []app.Publisher{pub})

	id, _ := e.create("stuck", atIn(2*time.Second), "alpha")

	// Simulate a worker that died after claiming the row: it is publishing and
	// its lease expired a minute ago.
	_, err := e.db.Exec(`UPDATE scheduled_posts SET status = 'publishing', lease_until = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).UnixMilli(), id)
	if err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	got := e.waitFor(id, statusIs("published"))
	if got["external_post_id"] == nil {
		t.Fatalf("not published after recovery: %v", got)
	}
	if n := e.count(`SELECT COUNT(1) FROM domain_events WHERE aggregate_id = ? AND event_type = 'PostLeaseRecovered'`, id); n != 1 {
		t.Fatalf("PostLeaseRecovered events: %d", n)
	}
}

func TestDailyLimitStopsPublishing(t *testing.T) {
	pub := fakePublisher(t, "alpha", fake.WithCapabilities(app.Capabilities{
		MaxTextLength: 500,
		SupportsText:  true,
		PreparesDraft: true,
		DailyLimit:    1,
	}))
	e := newEnv(t, []app.Publisher{pub})

	id1, _ := e.create("first", atIn(2*time.Second), "alpha")
	id2, _ := e.create("second", atIn(2*time.Second), "alpha")

	e.waitFor(id1, statusIs("published"))
	time.Sleep(300 * time.Millisecond)

	got := e.get(id2)
	if got["status"] != "scheduled" {
		t.Fatalf("second post should be held back by the daily budget, got %v", got["status"])
	}
	if n := len(pub.Published()); n != 1 {
		t.Fatalf("adapter publishes: want 1, got %d", n)
	}
}
