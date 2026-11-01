package app

import (
	"context"
	"time"
)

// ReserveResult is the outcome of claiming an idempotency key before the
// create runs. The claim is a single atomic INSERT, so exactly one concurrent
// request can hold it — first writer wins is enforced by the store, not by
// the wider create-then-store dance the previous Get/Put scheme used.
type ReserveResult int

const (
	// ReserveOurs means this request owns the key and must create the
	// schedule (then Complete, or Release if the create fails).
	ReserveOurs ReserveResult = iota
	// ReserveStored means a completed response for this exact request hash
	// is already stored; replay it and create nothing.
	ReserveStored
	// ReserveMismatch means the key exists with a different request hash.
	ReserveMismatch
	// ReserveInProgress means another request owns a pending reservation
	// for this key and hash; the client should retry (HTTP 409).
	ReserveInProgress
)

// IdempotencyRecord is the stored response of a POST /schedules call, keyed by
// the client-supplied Idempotency-Key. A retried request with the same key and
// the same request hash replays the stored response (Location included)
// instead of creating a second schedule; a different hash under the same key
// is a client error.
type IdempotencyRecord struct {
	Key         string
	RequestHash string
	StatusCode  int
	Location    string
	Response    string
}

// IdempotencyPruner removes stale key rows so the table cannot grow without
// bound. The scheduler calls it periodically when retention is configured.
type IdempotencyPruner interface {
	// Prune deletes rows created before the cutoff and returns the count.
	Prune(ctx context.Context, before time.Time) (int64, error)
}

// IdempotencyStore serializes creates behind client keys. Implemented by the
// SQLite adapter; nil disables the feature.
type IdempotencyStore interface {
	// Reserve claims the key for the request hash before any side effect.
	// See ReserveResult for the outcomes; a ReserveOurs holder must call
	// Complete after a successful create or Release after a failed one.
	Reserve(ctx context.Context, key, requestHash string) (ReserveResult, IdempotencyRecord, error)
	// Complete fills the pending reservation owned by this request. Rows
	// are matched on hash and only when still pending.
	Complete(ctx context.Context, key, requestHash string, statusCode int, location, response string) error
	// Release deletes this request's pending reservation (key + hash +
	// still pending); a completed response or a claim re-won by a later
	// request is never deleted.
	Release(ctx context.Context, key, requestHash string) error
}
