// Package fake is an in-memory Publisher for tests and local development.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

type Record struct {
	Content string
	Ref     string
	At      time.Time
}

type Option func(*Publisher)

// WithPrepareDisabled makes the adapter publish in a single call (Prepare
// returns nil), exercising the other branch of the port.
func WithPrepareDisabled() Option {
	return func(p *Publisher) { p.prepare = false }
}

func WithCapabilities(c app.Capabilities) Option {
	return func(p *Publisher) { p.caps = c }
}

type Publisher struct {
	platform domain.Platform
	caps     app.Capabilities
	prepare  bool

	mu        sync.Mutex
	prepared  []Record
	published []Record
	failures  []error
	always    error
	seq       int
}

func New(platform string, opts ...Option) (*Publisher, error) {
	p, err := domain.NewPlatform(platform)
	if err != nil {
		return nil, err
	}
	out := &Publisher{
		platform: p,
		caps: app.Capabilities{
			MaxTextLength: 500,
			SupportsText:  true,
			PreparesDraft: true,
			DailyLimit:    0,
		},
		prepare: true,
	}
	for _, opt := range opts {
		opt(out)
	}
	return out, nil
}

func (p *Publisher) Platform() domain.Platform      { return p.platform }
func (p *Publisher) Capabilities() app.Capabilities { return p.caps }

func (p *Publisher) Prepare(ctx context.Context, pub app.Publication) (*domain.PrepareRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.takeFailureLocked(); err != nil {
		return nil, err
	}
	if !p.prepare {
		return nil, nil
	}
	p.seq++
	raw := fmt.Sprintf("%s-draft-%d", p.platform.String(), p.seq)
	ref, err := domain.NewPrepareRef(raw)
	if err != nil {
		return nil, err
	}
	p.prepared = append(p.prepared, Record{Content: pub.Content, Ref: raw, At: time.Now().UTC()})
	return &ref, nil
}

func (p *Publisher) Publish(ctx context.Context, pub app.Publication, ref *domain.PrepareRef) (domain.ExternalPostID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.takeFailureLocked(); err != nil {
		return domain.ExternalPostID{}, err
	}
	p.seq++
	raw := fmt.Sprintf("%s-post-%d", p.platform.String(), p.seq)
	id, err := domain.NewExternalPostID(raw)
	if err != nil {
		return domain.ExternalPostID{}, err
	}
	rec := Record{Content: pub.Content, At: time.Now().UTC()}
	if ref != nil {
		rec.Ref = ref.String()
	}
	p.published = append(p.published, rec)
	return id, nil
}

// FailNext queues one failure for the next Prepare or Publish call.
func (p *Publisher) FailNext(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = append(p.failures, err)
}

// AlwaysFail makes every subsequent call fail with err.
func (p *Publisher) AlwaysFail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.always = err
}

// ClearFailures resets the failure queue and the always-fail setting.
func (p *Publisher) ClearFailures() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = nil
	p.always = nil
}

func (p *Publisher) Published() []Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Record, len(p.published))
	copy(out, p.published)
	return out
}

func (p *Publisher) Prepared() []Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Record, len(p.prepared))
	copy(out, p.prepared)
	return out
}

func (p *Publisher) takeFailureLocked() error {
	if p.always != nil {
		return p.always
	}
	if len(p.failures) > 0 {
		err := p.failures[0]
		p.failures = p.failures[1:]
		return err
	}
	return nil
}
