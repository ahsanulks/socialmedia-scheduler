package app

// IdempotencyRecord is the stored response of a POST /schedules call, keyed by
// the client-supplied Idempotency-Key. A retried request with the same key and
// the same request hash replays the stored response instead of creating a
// second schedule; a different hash under the same key is a client error.
type IdempotencyRecord struct {
	Key         string
	RequestHash string
	StatusCode  int
	Response    string
}

// IdempotencyStore remembers create responses so retries with the same key
// replay them. Implemented by the SQLite adapter; nil disables the feature.
type IdempotencyStore interface {
	// Get returns found=false when the key is unknown.
	Get(key string) (IdempotencyRecord, bool, error)
	// Put stores the record; an already-stored key is never overwritten
	// (first writer wins), which keeps replays stable under racing retries.
	Put(rec IdempotencyRecord) error
}
