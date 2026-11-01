# Code structure & domain encapsulation

Package layout, the strict-typing rules for the domain, and how an aggregate with **no exported
fields** is persisted, queried, and serialized.

Design reference: *Functional Programming in Scala* (Chiusano & Bjarnason) — smart constructors,
algebraic data types, making illegal states unrepresentable. §4 maps each idea to its Go equivalent.

---

## 1. Package map

```
cmd/api/main.go                  # config, publishers, wiring, graceful shutdown

migrations/
  migrations.go                  # embed.FS over the SQL files
  0001_init.sql                  # scheduled_posts, domain_events, schedules_read, publish_attempts

internal/
  bootstrap/bootstrap.go         # builds db + repositories + handlers + worker (used by main and e2e)
  platform/
    clock/clock.go               # Clock implementation (UTC)
    config/config.go             # env parsing, fail-fast validation
    idgen/uuidv7.go              # PostID generator
  scheduling/
    domain/                      # stdlib only: no SQL, no JSON, no HTTP
      values.go                  # PostID, PostContent, ScheduleTime, Platform, Version, Attempt,
                                 # PrepareRef, ExternalPostID, FailureReason, Status
      state.go                   # sealed state ADT: Scheduled / Publishing / Published / Failed / Canceled
      scheduled_post.go          # the aggregate: constructors, behaviour, mutate, PullEvents
      snapshot.go                # Snapshot out, Rehydrate in
      events.go                  # domain events
      errors.go                  # ValidationError, NotFoundError, VersionConflictError, InvalidTransitionError
      repository.go              # write-side port + attempt log
    app/                         # use cases; imports domain
      ports.go                   # Clock, IDGenerator, Publication, Capabilities, Publisher,
                                 # Registry, ScheduleReadRepository, keyset cursor
      errors.go                  # UnsupportedPlatformError, PlatformLimitError
      backoff.go                 # retry schedule
      schedule_post.go           # command: create a schedule
      update_schedule.go         # command: edit content / move the time
      cancel_schedule.go         # command: soft delete
      queries.go                 # queries: get one, list the plan
      publish_post.go            # the publish flow (driven by the worker)
      recover_leases.go          # requeue publishes whose worker died
    adapters/                    # imports domain + app
      sqlite/
        db.go                    # DSN, pragmas, embedded migration runner
        repo.go                  # Get / Save (state + events + projection in one tx), due & lease queries
        projector.go             # schedules_read upsert from the snapshot
        read_repo.go             # query side: Get, List with keyset pagination
      publishers/
        threads/                 # Prepare = POST /threads, Publish = POST /threads_publish
          publisher.go
          errors.go              # delegates to graphapi.Classify
        facebook/                # single-call: POST /{page-id}/feed (Prepare is a no-op)
          publisher.go
        instagram/               # two-step: POST /media -> poll status -> POST /media_publish
          publisher.go           # image-only, bounded container poll in Prepare
        graphapi/                # shared Meta Graph error envelope -> Retryable / Permanent,
          graphapi.go            # form-POST + JSON-GET client, poll timing for containers
        fake/                    # in-memory adapter for tests and local dev
          fake.go
      httpapi/
        server.go                # routes, bearer auth, middleware, problem+json mapping
        dto.go                   # request/response DTOs, strict UTC timestamp parsing
    scheduler/worker.go          # tick loop: recover leases → publish due → per-platform budget

e2e/                             # end-to-end tests: real HTTP + real SQLite + real worker
api/openapi.yaml
```

**Dependency rule:** `domain` ← `app` ← `adapters`; `cmd` imports everything to wire it.
`domain` never imports `app`, adapters, `database/sql`, `net/http`, or `encoding/json`.

### Where interfaces live

| Interface | Package | Why |
|---|---|---|
| `ScheduledPostRepository`, `DuePostClaimer` | `domain` | Their signatures are made of domain types; they are repository ports |
| `Clock`, `IDGenerator`, `Publisher`, `ScheduleReadRepository` | `app` | Pure infrastructure concerns the use case consumes |

Go's "define interfaces where they are consumed" rule holds in both cases — the domain *is* the
consumer for repositories.

---

## 2. Encapsulation contract

**No exported fields anywhere in `domain`.** Exactly two ways in, three ways out.

```
        in                                   out
  ┌──────────────┐                    ┌───────────────────┐
  │ New...(args) │──┐              ┌──│ behavior methods  │  UpdateContent, Reschedule,
  └──────────────┘  │              │  │                   │  Cancel, MarkPublished, …
  ┌──────────────┐  ├─▶ ScheduledPost ─┤───────────────────┤
  │ Rehydrate()  │──┘   (private     └──│ Snapshot()        │  → persistence adapter
  └──────────────┘       fields)         │ PullEvents()      │  → event log + projector
                                         │ accessors (few)   │  → results / logs
                                         └───────────────────┘
```

- **In:** `NewScheduledPost` (user intent → applies *creation* rules) and `Rehydrate`
  (storage → applies *structural* rules only).
- **Out:** behavior methods return `error`; `Snapshot()` feeds persistence; `PullEvents()` feeds the
  event log; `ID()`, `Version()`, `Status()` feed results and logs.
- No setters, no exported struct, no `Update(func(*ScheduledPost))` escape hatch.

### The two constructors must not share validation

| Rule | `New...` | `Rehydrate` |
|---|---|---|
| Content 1–500, platform slug valid, id parses | ✓ | ✓ |
| `version >= 1`, `attempts >= 0` | ✓ | ✓ |
| `scheduled_at` in the future | ✓ | ✗ — an overdue post is legitimate after downtime |
| State/field consistency (`published` has an external id, etc.) | — | ✓ |

---

## 3. Value objects (smart constructors)

Every domain concept gets its own type: construct once, valid forever, never re-check downstream.
Zero values are invalid by construction.

| Type | Field | Constructor rules |
|---|---|---|
| `PostID` | string | UUID shape (regexp), lowercased |
| `PostContent` | string | trimmed, 1–5000 runes (sanity bound) |
| `ScheduleTime` | `time.Time` | non-zero, UTC, truncated to ms |
| `Platform` | string | slug `[a-z][a-z0-9_]{1,31}`; must be a registered adapter |
| `Status` | closed set | `ParseStatus` only; zero invalid |
| `Version` | int | ≥ 1; grows only via `Next()` |
| `Attempt` | int | ≥ 0; grows only via `Next()` |
| `PrepareRef` | string | opaque platform reference, non-empty, ≤ 128 |
| `ExternalPostID` | string | non-empty, ≤ 128 |
| `FailureReason` | string | trimmed, 1–500 (truncated above that) |

```go
package domain

type PostContent struct{ v string }

// MaxContentRunes is a service sanity bound, NOT a platform rule.
// Platform limits (threads 500, x 280, …) live in each adapter's Capabilities.
const MaxContentRunes = 5000

// NewPostContent is the only way to obtain a PostContent. There is no way to
// build an invalid one, so the aggregate never re-validates content.
func NewPostContent(s string) (PostContent, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return PostContent{}, ValidationError{Field: "content", Message: "must not be empty"}
	}
	if n := utf8.RuneCountInString(s); n > MaxContentRunes {
		return PostContent{}, ValidationError{
			Field:   "content",
			Message: fmt.Sprintf("must be at most %d characters, got %d", MaxContentRunes, n),
		}
	}
	return PostContent{v: s}, nil
}

func (c PostContent) String() string { return c.v }
func (c PostContent) RuneCount() int { return utf8.RuneCountInString(c.v) }
```

```go
type ScheduleTime struct{ t time.Time } // always UTC, ms precision

func NewScheduleTime(t time.Time) (ScheduleTime, error) {
	if t.IsZero() {
		return ScheduleTime{}, ValidationError{Field: "scheduled_at", Message: "is required"}
	}
	return ScheduleTime{t: t.UTC().Truncate(time.Millisecond)}, nil
}

func (s ScheduleTime) Time() time.Time          { return s.t }
func (s ScheduleTime) IsAfter(t time.Time) bool { return s.t.After(t) }
```

```go
type Version struct{ n int }

func InitialVersion() Version { return Version{n: 1} }

func ParseVersion(n int) (Version, error) {
	if n < 1 {
		return Version{}, ValidationError{Field: "version", Message: "must be >= 1"}
	}
	return Version{n: n}, nil
}

func (v Version) Int() int      { return v.n }
func (v Version) Next() Version { return Version{n: v.n + 1} }
func (v Version) IsZero() bool  { return v.n == 0 }
```

```go
type Status struct{ v status }

type status uint8

const (
	statusInvalid status = iota
	statusScheduled
	statusPublishing
	statusPublished
	statusFailed
	statusCanceled
)

var (
	StatusScheduled  = Status{statusScheduled}
	StatusPublishing = Status{statusPublishing}
	StatusPublished  = Status{statusPublished}
	StatusFailed     = Status{statusFailed}
	StatusCanceled   = Status{statusCanceled}
)

func ParseStatus(s string) (Status, error) {
	switch s {
	case "scheduled":  return StatusScheduled, nil
	case "publishing": return StatusPublishing, nil
	case "published":  return StatusPublished, nil
	case "failed":     return StatusFailed, nil
	case "canceled":   return StatusCanceled, nil
	default:
		return Status{}, ValidationError{Field: "status", Message: "unknown status " + s}
	}
}

func (s Status) String() string { /* switch on s.v */ }
func (s Status) IsZero() bool   { return s.v == statusInvalid }
```

The struct wrapper (rather than `type Status int` with exported constants) means `Status(42)` cannot
be written outside the package. Treat the package vars as constants; never reassign them.

`Platform` is a validated slug. The domain does not know which platforms exist — that is the
registry's job at the application edge:

```go
type Platform struct{ v string }

var platformPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

func NewPlatform(s string) (Platform, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if !platformPattern.MatchString(s) {
		return Platform{}, ValidationError{Field: "platform", Message: "must match [a-z][a-z0-9_]{1,31}"}
	}
	return Platform{v: s}, nil
}

func (p Platform) String() string { return p.v }
```

---

## 4. Scala → Go mapping

| FP in Scala | Go equivalent here |
|---|---|
| `case class Email private (value: String)` + smart constructor | struct with an unexported field + `NewEmail(string) (Email, error)` |
| `Either[E, A]` | `(A, error)`; typed errors matched with `errors.As` |
| `Option[A]` | a pointer only where absence is real; mostly eliminated by the state ADT |
| `sealed trait Status` + case objects | unexported interface + unexported state structs, sealed by an unexported method |
| exhaustive `match` | type switch with `default: panic` + a test that enumerates every state |
| immutable case class + `copy` | value structs replaced wholesale (`p.state = next`) |
| newtype | named struct wrapper, so `PrepareRef` can never be passed as `ExternalPostID` |
| `require(...)` in a constructor | `ValidationError{Field, Message}` returned from the smart constructor |

**We do not build an `Option` or `Either` monad.** Go's multiple return *is* the Either, and
optionals are reduced to three places, each checked at the point of use:

1. `Snapshot` pointers — they exist because SQL has `NULL`, not because the domain likes pointers.
2. `scheduled.retry` — `nil` means "due as soon as possible"; when present it carries both the time
   and the reason, so a retry time without a reason is unrepresentable.
3. `ScheduledPost.prepareRef` — `nil` means "nothing prepared yet"; monotonic,
   set once.

---

## 5. State ADT

The lifecycle is a sealed set of variants, each carrying only the data valid in that stage. `Status`
is **derived** from the variant, so a status field can never disagree with the state.

```go
type state interface {
	isState()
	status() Status
}

// scheduled: waiting for its due time. retry == nil means due immediately.
type scheduled struct{ retry *retry }

type retry struct {
	at     time.Time
	reason FailureReason
}

func (scheduled) isState()       {}
func (scheduled) status() Status { return StatusScheduled }

// publishing: claimed by the worker under a lease.
type publishing struct{ leaseUntil time.Time }

func (publishing) isState()       {}
func (publishing) status() Status { return StatusPublishing }

// published: terminal success.
type published struct {
	externalPostID ExternalPostID
	publishedAt    time.Time
}

func (published) isState()       {}
func (published) status() Status { return StatusPublished }

// failed: terminal failure (permanent error, or attempts exhausted).
type failed struct {
	reason   FailureReason
	failedAt time.Time
}

func (failed) isState()       {}
func (failed) status() Status { return StatusFailed }

// canceled: terminal, user-initiated.
type canceled struct{ canceledAt time.Time }

func (canceled) isState()       {}
func (canceled) status() Status { return StatusCanceled }
```

All five types are unexported and the interface method is unexported, so no package outside
`domain` can add a sixth variant. Go has no exhaustiveness checking, so every type switch ends in
`default: panic("unhandled state")` and `state_test.go` asserts all five variants are covered.

Deliberate exception: `prepareRef` lives on the aggregate, not inside `publishing`. It is an
infrastructure artifact (a platform-side draft reference) that must survive a lease
recovery back into `scheduled`; it is not a business fact about the lifecycle.

---

## 6. Aggregate

```go
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {} // makes `go vet` flag accidental value copies

type ScheduledPost struct {
	noCopy noCopy

	id          PostID
	content     PostContent
	scheduledAt ScheduleTime
	platform    Platform
	version     Version
	attempts    Attempt
	prepareRef *PrepareRef // infrastructure artifact, see §5

	createdAt time.Time
	updatedAt time.Time

	state  state
	events []Event
}

func NewScheduledPost(id PostID, c PostContent, at ScheduleTime, platform Platform, now time.Time) (*ScheduledPost, error) {
	if !at.IsAfter(now) {
		return nil, ValidationError{Field: "scheduled_at", Message: "must be in the future"}
	}
	p := &ScheduledPost{
		id: id, content: c, scheduledAt: at, platform: platform,
		version: InitialVersion(), attempts: NoAttempts(),
		createdAt: now, updatedAt: now,
		state: scheduled{},
	}
	p.record(PostScheduled{ID: id.String(), Content: c.String(), ScheduledAt: at.Time(), Version: p.version.Int()})
	return p, nil
}
```

### Helpers that make drift impossible

```go
// mutate is the ONLY place version and updatedAt change.
func (p *ScheduledPost) mutate(now time.Time) Version {
	p.version = p.version.Next()
	p.updatedAt = now
	return p.version
}

func (p *ScheduledPost) record(e Event) { p.events = append(p.events, e) }

func (p *ScheduledPost) guardVersion(expected Version) error {
	if p.version != expected {
		return VersionConflictError{Expected: expected, Actual: p.version}
	}
	return nil
}

func (p *ScheduledPost) invalid(event string) error {
	return InvalidTransitionError{From: p.Status(), Event: event}
}
```

### Behavior methods

```go
func (p *ScheduledPost) UpdateContent(c PostContent, now time.Time, expected Version) error {
	if err := p.guardVersion(expected); err != nil {
		return err
	}
	if _, ok := p.state.(scheduled); !ok {
		return p.invalid("UpdateContent")
	}
	p.content = c
	p.record(PostContentUpdated{ID: p.id.String(), Content: c.String(), Version: p.mutate(now).Int()})
	return nil
}

func (p *ScheduledPost) Reschedule(at ScheduleTime, now time.Time, expected Version) error {
	if err := p.guardVersion(expected); err != nil {
		return err
	}
	switch p.state.(type) {
	case scheduled, failed:
	default:
		return p.invalid("Reschedule")
	}
	if !at.IsAfter(now) {
		return ValidationError{Field: "scheduled_at", Message: "must be in the future"}
	}
	p.scheduledAt = at
	p.attempts = NoAttempts()
	p.prepareRef = nil
	p.state = scheduled{}
	p.record(PostRescheduled{ID: p.id.String(), ScheduledAt: at.Time(), Version: p.mutate(now).Int()})
	return nil
}

func (p *ScheduledPost) Cancel(now time.Time, expected *Version) error {
	if _, ok := p.state.(canceled); ok {
		return nil // idempotent: repeated DELETE succeeds
	}
	if expected != nil {
		if err := p.guardVersion(*expected); err != nil {
			return err
		}
	}
	switch p.state.(type) {
	case scheduled, failed:
	default:
		return p.invalid("Cancel")
	}
	p.state = canceled{canceledAt: now}
	p.record(PostCanceled{ID: p.id.String(), Version: p.mutate(now).Int()})
	return nil
}

// Worker-facing transitions live here too, not in the scheduler package.

func (p *ScheduledPost) StartPublishing(now time.Time, lease time.Duration) error {
	st, ok := p.state.(scheduled)
	if !ok {
		return p.invalid("StartPublishing")
	}
	if st.retry != nil && st.retry.at.After(now) {
		return p.invalid("StartPublishing: backoff not elapsed")
	}
	p.attempts = p.attempts.Next()
	p.state = publishing{leaseUntil: now.Add(lease)}
	p.record(PostPublishStarted{ID: p.id.String(), Attempt: p.attempts.Int(), Version: p.mutate(now).Int()})
	return nil
}

func (p *ScheduledPost) AttachPrepareRef(id PrepareRef, now time.Time) error {
	if _, ok := p.state.(publishing); !ok {
		return p.invalid("AttachPrepareRef")
	}
	p.prepareRef = &id
	p.record(PostPrepared{ID: p.id.String(), PrepareRef: id.String(), Version: p.mutate(now).Int()})
	return nil
}

func (p *ScheduledPost) MarkPublished(externalID ExternalPostID, now time.Time) error {
	if _, ok := p.state.(publishing); !ok {
		return p.invalid("MarkPublished")
	}
	p.state = published{externalPostID: externalID, publishedAt: now}
	p.record(PostPublished{ID: p.id.String(), ExternalPostID: externalID.String(), Version: p.mutate(now).Int()})
	return nil
}

func (p *ScheduledPost) MarkFailure(reason FailureReason, retryAt *time.Time, now time.Time) error {
	if _, ok := p.state.(publishing); !ok {
		return p.invalid("MarkFailure")
	}
	if retryAt != nil && p.attempts.Int() < MaxPublishAttempts {
		p.state = scheduled{retry: &retry{at: *retryAt, reason: reason}}
		p.record(PostPublishFailed{ID: p.id.String(), Retryable: true, Reason: reason.String(), Version: p.mutate(now).Int()})
		return nil
	}
	p.state = failed{reason: reason, failedAt: now}
	p.record(PostPublishFailed{ID: p.id.String(), Retryable: false, Reason: reason.String(), Version: p.mutate(now).Int()})
	return nil
}

// RecoverExpiredLease returns true when the aggregate was actually reset.
func (p *ScheduledPost) RecoverExpiredLease(now time.Time) bool {
	st, ok := p.state.(publishing)
	if !ok || st.leaseUntil.After(now) {
		return false
	}
	p.state = scheduled{retry: &retry{at: now, reason: leaseExpiredReason}}
	p.record(PostLeaseRecovered{ID: p.id.String(), Version: p.mutate(now).Int()})
	return true
}
```

### Accessors and the two exits

```go
func (p *ScheduledPost) ID() PostID             { return p.id }
func (p *ScheduledPost) Version() Version       { return p.version }
func (p *ScheduledPost) Status() Status         { return p.state.status() }
func (p *ScheduledPost) ScheduledAt() time.Time  { return p.scheduledAt.Time() }

// PullEvents drains uncommitted events. Call once, at the end of the handler.
func (p *ScheduledPost) PullEvents() []Event {
	ev := p.events
	p.events = nil
	return ev
}

func (p *ScheduledPost) Snapshot() Snapshot { /* see §7 */ }
```

Accessors are added only when a real caller needs one. No blanket getters.

---

## 7. Persistence contract: `Snapshot` + `Rehydrate`

The adapter cannot see private fields, so the aggregate exports a **primitives-only** snapshot. It
is a data-transfer struct, not a second model: no methods, no invariants, no domain types.

```go
type Snapshot struct {
	ID          string
	Content     string
	ScheduledAt time.Time
	Platform    string
	Status      string
	Version     int
	Attempts    int
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// State-specific. Nil means "not applicable to this state"; pointers exist
	// because SQL has NULL, not because the domain likes pointers.
	PrepareRef    *string
	NextAttemptAt  *time.Time
	LastError      *string
	LeaseUntil     *time.Time
	ExternalPostID *string
	PublishedAt    *time.Time
	FailedAt       *time.Time
	CanceledAt     *time.Time
}
```

```go
func (p *ScheduledPost) Snapshot() Snapshot {
	s := Snapshot{
		ID: p.id.String(), Content: p.content.String(),
		ScheduledAt: p.scheduledAt.Time(), Platform: p.platform.String(),
		Status: p.Status().String(), Version: p.version.Int(), Attempts: p.attempts.Int(),
		CreatedAt: p.createdAt, UpdatedAt: p.updatedAt,
	}
	if p.prepareRef != nil {
		v := p.prepareRef.String()
		s.PrepareRef = &v
	}
	switch st := p.state.(type) {
	case scheduled:
		if st.retry != nil {
			at, reason := st.retry.at, st.retry.reason.String()
			s.NextAttemptAt, s.LastError = &at, &reason
		}
	case publishing:
		lease := st.leaseUntil
		s.LeaseUntil = &lease
	case published:
		id, at := st.externalPostID.String(), st.publishedAt
		s.ExternalPostID, s.PublishedAt = &id, &at
	case failed:
		reason, at := st.reason.String(), st.failedAt
		s.LastError, s.FailedAt = &reason, &at
	case canceled:
		at := st.canceledAt
		s.CanceledAt = &at
	default:
		panic("unhandled state")
	}
	return s
}
```

```go
// Rehydrate rebuilds an aggregate from storage. Structural rules only: an
// overdue post is legitimate, so creation-time rules are NOT applied here.
func Rehydrate(s Snapshot) (*ScheduledPost, error) {
	id, err := NewPostID(s.ID)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	content, err := NewPostContent(s.Content)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	at, err := NewScheduleTime(s.ScheduledAt)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	platform, err := NewPlatform(s.Platform)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	version, err := ParseVersion(s.Version)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	attempts, err := ParseAttempt(s.Attempts)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	st, err := rehydrateState(s)
	if err != nil {
		return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
	}
	p := &ScheduledPost{
		id: id, content: content, scheduledAt: at, platform: platform,
		version: version, attempts: attempts,
		createdAt: s.CreatedAt, updatedAt: s.UpdatedAt,
		state: st,
	}
	if s.PrepareRef != nil {
		cid, err := NewPrepareRef(*s.PrepareRef)
		if err != nil {
			return nil, fmt.Errorf("rehydrate %q: %w", s.ID, err)
		}
		p.prepareRef = &cid
	}
	return p, nil
}

// rehydrateState is the parser that rejects impossible rows: each state may
// only carry the fields that state can produce.
func rehydrateState(s Snapshot) (state, error) {
	status, err := ParseStatus(s.Status)
	if err != nil {
		return nil, err
	}
	switch status {
	case StatusScheduled:
		if s.LeaseUntil != nil || s.ExternalPostID != nil || s.PublishedAt != nil ||
			s.FailedAt != nil || s.CanceledAt != nil {
			return nil, errors.New("scheduled state carries fields from another state")
		}
		if (s.NextAttemptAt == nil) != (s.LastError == nil) {
			return nil, errors.New("retry requires both next_attempt_at and last_error")
		}
		st := scheduled{}
		if s.NextAttemptAt != nil {
			reason, err := NewFailureReason(*s.LastError)
			if err != nil {
				return nil, err
			}
			st.retry = &retry{at: s.NextAttemptAt.UTC(), reason: reason}
		}
		return st, nil
	case StatusPublishing:
		if s.LeaseUntil == nil {
			return nil, errors.New("publishing requires lease_until")
		}
		if s.NextAttemptAt != nil || s.ExternalPostID != nil || s.PublishedAt != nil {
			return nil, errors.New("publishing state carries fields from another state")
		}
		return publishing{leaseUntil: s.LeaseUntil.UTC()}, nil
	case StatusPublished:
		if s.ExternalPostID == nil || s.PublishedAt == nil {
			return nil, errors.New("published requires external_post_id and published_at")
		}
		ext, err := NewExternalPostID(*s.ExternalPostID)
		if err != nil {
			return nil, err
		}
		return published{externalPostID: ext, publishedAt: s.PublishedAt.UTC()}, nil
	case StatusFailed:
		if s.LastError == nil || s.FailedAt == nil {
			return nil, errors.New("failed requires last_error and failed_at")
		}
		reason, err := NewFailureReason(*s.LastError)
		if err != nil {
			return nil, err
		}
		return failed{reason: reason, failedAt: s.FailedAt.UTC()}, nil
	case StatusCanceled:
		if s.CanceledAt == nil {
			return nil, errors.New("canceled requires canceled_at")
		}
		return canceled{canceledAt: s.CanceledAt.UTC()}, nil
	default:
		return nil, errors.New("unhandled status")
	}
}
```

The SQLite adapter only moves `Snapshot` ↔ columns:

```go
func scanPost(rs rowScanner) (*domain.ScheduledPost, error) {
	var s domain.Snapshot
	if err := rs.Scan(
		&s.ID, &s.Content, &s.ScheduledAt, &s.Platform, &s.Status,
		&s.Version, &s.Attempts, &s.PrepareRef, &s.NextAttemptAt, &s.LastError,
		&s.LeaseUntil, &s.ExternalPostID, &s.PublishedAt, &s.FailedAt, &s.CanceledAt,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return domain.Rehydrate(s)
}
```

### Errors

```go
var ErrNotFound = errors.New("scheduled post not found")

type ValidationError struct{ Field, Message string }
func (e ValidationError) Error() string { return e.Field + ": " + e.Message }

type VersionConflictError struct{ Expected, Actual Version }
func (e VersionConflictError) Error() string { … }

type InvalidTransitionError struct{ From Status; Event string }
func (e InvalidTransitionError) Error() string { … }
```

`httpapi/problem.go` maps them with `errors.As`: `ValidationError` → 422,
`VersionConflictError` → 409 `version_conflict`, `InvalidTransitionError` → 409 `invalid_state`,
`ErrNotFound` → 404, everything else → 500.

### Repository ports

```go
type ScheduledPostRepository interface {
	Get(ctx context.Context, id PostID) (*ScheduledPost, error) // ErrNotFound when absent
	// Save commits state + events + read-model projection in ONE transaction,
	// guarded by version (rows affected must be 1).
	Save(ctx context.Context, p *ScheduledPost, events []Event) error
}

type DuePostClaimer interface {
	ClaimDue(ctx context.Context, now time.Time, batch int, lease time.Duration) ([]*ScheduledPost, error)
}
```

`Save` owning the transaction means a handler cannot forget the projection or split the write. The
projector stays a separate type inside the SQLite adapter so it can be rebuilt or made async later.

---

## 8. Application layer

Handlers orchestrate: parse primitives into domain types, load, call domain methods, save, return a
result. No business rules live here.

```go
type UpdateSchedule struct {
	ID              string
	Content         *string
	ScheduledAt     *time.Time
	ExpectedVersion int
}

type UpdateScheduleHandler struct {
	posts domain.ScheduledPostRepository
	clock Clock
}

func NewUpdateScheduleHandler(posts domain.ScheduledPostRepository, clock Clock) *UpdateScheduleHandler {
	return &UpdateScheduleHandler{posts: posts, clock: clock}
}

func (h *UpdateScheduleHandler) Handle(ctx context.Context, cmd UpdateSchedule) (Result, error) {
	id, err := domain.NewPostID(cmd.ID)
	if err != nil {
		return Result{}, err
	}
	expected, err := domain.ParseVersion(cmd.ExpectedVersion)
	if err != nil {
		return Result{}, err
	}
	post, err := h.posts.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	now := h.clock.Now()

	if cmd.Content != nil {
		content, err := domain.NewPostContent(*cmd.Content)
		if err != nil {
			return Result{}, err
		}
		if err := post.UpdateContent(content, now, expected); err != nil {
			return Result{}, err
		}
	}
	if cmd.ScheduledAt != nil {
		at, err := domain.NewScheduleTime(*cmd.ScheduledAt)
		if err != nil {
			return Result{}, err
		}
		// the first mutation advanced the version, so re-read it here
		if err := post.Reschedule(at, now, post.Version()); err != nil {
			return Result{}, err
		}
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		return Result{}, err
	}
	return Result{ID: post.ID().String(), Version: post.Version().Int()}, nil
}
```

Two things handlers must get right: re-read `post.Version()` for a second mutation, and call
`PullEvents()` exactly once after the last mutation. Both are covered by handler tests.

Ports (`app/ports.go`):

```go
type Clock interface{ Now() time.Time }
type IDGenerator interface{ NewID() string }

type Publication struct {
	Platform domain.Platform
	Content  string
}

type Publisher interface {
	Platform() domain.Platform
	Capabilities() Capabilities // this platform's own rules
	Prepare(ctx context.Context, pub Publication) (*domain.PrepareRef, error)
	Publish(ctx context.Context, pub Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error)
}

type Capabilities struct {
	MaxTextLength int
	RequiresMedia bool
	SupportsText  bool
	PreparesDraft bool
	DailyLimit    int
}

type ScheduleReadRepository interface {
	Get(ctx context.Context, id string) (ScheduleView, error)
	List(ctx context.Context, q ListQuery) (Page, error)
}
```

The query side never touches the aggregate:

```go
type ScheduleView struct {
	ID             string
	Content        string
	Status         string
	ScheduledAt    time.Time
	Platform       string
	Attempts       int
	LastError      *string
	ExternalPostID *string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PublishedAt    *time.Time
}
```

---

## 9. Write side vs read side at the HTTP boundary

| | Request in | Response out |
|---|---|---|
| Commands | `httpapi.CreateScheduleRequest` → `app.SchedulePost` | `app.Result` → `{id, version}` |
| Queries | query string → `app.ListQuery` | `app.ScheduleView` → `httpapi.ScheduleResponse` |

Rules:

- Domain types never carry JSON tags and never cross into `httpapi` DTOs directly.
- `httpapi/dto.go` owns the wire format; `openapi.yaml` documents exactly those structs.
- Command responses are deliberately minimal; clients `GET` the read model for details.

---

## 10. Testing implications

| Test package | Style | What it can see |
|---|---|---|
| `domain` (white box) | state-machine tables; construct a `publishing` aggregate directly | unexported fields and states |
| `domain_test` (black box) | public API: constructors, behavior, `Snapshot`/`Rehydrate` round-trip | exported surface only |
| `app` | fakes for `Clock`, `IDGenerator`, `Publisher`, repos | the use-case contract |
| `adapters/sqlite` | temp DB + real migrations | Snapshot ↔ row mapping, claim races |
| `adapters/httpapi` | `httptest` end-to-end | JSON shapes, status codes, error mapping |
| `adapters/threads` | `httptest` fake Graph API | two-step flow, error classification |

White-box tests reach impossible-looking states without driving twenty calls:

```go
func TestReschedule_WhilePublishing_Rejected(t *testing.T) {
	p := mustPost(t, publishing{leaseUntil: now.Add(time.Minute)})
	err := p.Reschedule(future(), now, p.Version())
	var ite InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("want InvalidTransitionError, got %v", err)
	}
}
```

`export_test.go` exposes such helpers only to the package's tests; nothing leaks into the binary.

Also required:

- `TestStatus_AllStatesCovered` — the five state variants yield five distinct statuses.
- `TestSnapshot_RoundTrip` — `Rehydrate(p.Snapshot())` preserves every observable field.
- `TestRehydrate_RejectsImpossibleRows` — `published` without `external_post_id`, `scheduled` with
  `lease_until`, a retry time without a reason, etc.

---

## 11. Rules to keep it from rotting

1. No primitives across the domain API except `time.Time`, `time.Duration`, `error`, `context` — and
   those must have unambiguous meaning (`time.Duration` already carries its unit).
2. Every type validates in its constructor; the zero value is invalid; no re-validation downstream.
3. `Status` is derived from the state. Never add a `status` field.
4. Transitions replace the whole state value (`p.state = published{…}`), never mutate a shared one.
5. Every type switch over `state` has `default: panic("unhandled state")` plus a coverage test.
6. `New...` applies creation rules; `Rehydrate` applies structural rules and rejects impossible rows.
7. `Snapshot` is primitives + pointers only. No domain types, no methods, no JSON tags.
8. `mutate(now)` is the only place `version`/`updatedAt` change; one event per mutation.
9. `PullEvents()` once per handler, after the last mutation.
10. Wrap a primitive when it has a rule or a unit to enforce; do not wrap for ceremony.

---

## 12. Alternatives considered

**Persistence boundary for "no exported fields":**

| Option | How persistence works | Trade-off |
|---|---|---|
| **A. `Snapshot` + `Rehydrate`** *(chosen)* | adapter maps a primitives struct ↔ columns | aggregate stays fully private; one exported DTO at the boundary |
| **B. Repository inside `package domain`** | repos are files in the domain package, so they read private fields | zero exported state, but the domain imports the SQL driver and needs a DB to test |
| **C. Event-sourced persistence** | the only contract is `PullEvents()` out and `Replay(events)` in | strongest encapsulation; costs event versioning, replay, snapshots — deferred in ADR-0001 |
| **D. Accessor getters only** | fine for reading, still needs something for writing | collapses back into A on the write path |

**Strict types vs. plain primitives:** plain `string`/`int` plus validation at each use site was
rejected — it re-checks the same rules in handlers, adapters, and tests, and lets a prepare ref be
passed where a post id is expected. The wrapper types remove that class of bug for ~250 lines of
domain code and a few conversions at the boundaries.

---

## 13. Trace: one `PATCH` request

1. `httpapi` decodes JSON into `UpdateScheduleRequest`, builds `app.UpdateSchedule` (primitives).
2. Handler parses `PostID` / `Version`, loads the aggregate via `repo.Get` → `Rehydrate`.
3. `NewPostContent` / `NewScheduleTime` validate; `post.UpdateContent` / `post.Reschedule` check
   version + state, replace the state value, append events, advance `Version`.
4. `repo.Save(ctx, post, post.PullEvents())` in one transaction: version-guarded state upsert,
   `domain_events` append, `schedules_read` projection upsert.
5. Handler returns `Result{id, version}`; `httpapi` writes `200 {id, version}`.
6. Client `GET /schedules/{id}` reads the projection — never the aggregate.
