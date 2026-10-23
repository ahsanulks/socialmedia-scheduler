# ADR-0002: Strict domain types and a sealed state ADT

**Date:** 2026-10-21
**Status:** accepted

## Context

The domain must have no exported fields, and the modelling style requested is the one from
*Functional Programming in Scala*: smart constructors, algebraic data types, and making illegal
states unrepresentable. Go has no `sealed` traits, no `Option`/`Either` in the stdlib, and no
exhaustiveness checking, so each idea needs a concrete Go equivalent that does not turn the domain
into unidiomatic ceremony.

The previous design already had `PostContent` and `ScheduleTime` wrappers, but the aggregate still
carried `status` plus six nullable fields (`nextAttemptAt`, `prepareRef`, `externalPostID`,
`lastError`, `leaseUntil`, `publishedAt`) that were only meaningful in some statuses.

## Decision

### 1. A value object per domain concept

`PostID`, `PostContent`, `ScheduleTime`, `Platform`, `Status`, `Version`, `Attempt`, `PrepareRef`,
`ExternalPostID`, `FailureReason` — each with an unexported field, a validating constructor, and
accessors. Zero values are invalid. No raw `string`/`int` crosses the domain API for these concepts.

`time.Time` and `time.Duration` are *not* wrapped: they already carry their unit and meaning, and
wrapping them would add conversion noise without preventing a real bug.

### 2. A sealed state ADT instead of a status field

```
scheduled{retry *retry} · publishing{leaseUntil} · published{externalPostID, publishedAt}
failed{reason, failedAt} · canceled{canceledAt}
```

`Status` is derived from the variant. A status field and a state can no longer disagree, and
state-specific fields cannot exist in the wrong state (no `publishedAt` on a canceled post).

Go sealing: unexported interface method + unexported implementations. Exhaustiveness: every type
switch has `default: panic("unhandled state")`, backed by a test that enumerates all five variants.

### 3. `(T, error)` is the Either

Smart constructors return `(T, error)`; errors are typed so adapters can map them. No generic
`Result`/`Option` monad. Optionals survive in exactly three places: `Snapshot` pointers (SQL `NULL`),
`scheduled.retry` (nil = due now), and `prepareRef` (nil = nothing prepared yet). In each case
the nil is checked immediately at the point of use, and the first two pair related fields so a
partial state is unrepresentable.

### 4. Snapshot remains primitives

The persistence contract stays a flat struct of primitives + pointers. Value objects never cross
into adapters; `Rehydrate` rebuilds them inside the domain and validates state/field consistency,
rejecting impossible rows (e.g. `published` without an external id).

## Consequences

- **Cost:** roughly 250 extra lines in `domain`, and conversions at the HTTP/SQL boundaries
  (`cmd.ID` string → `PostID`, `Version.Int()` → JSON).
- **Benefit:** no primitive mix-ups (a `PrepareRef` cannot be passed as an `ExternalPostID`), no
  re-validation in handlers or adapters, and state-specific fields are impossible to misuse.
- Go cannot prove exhaustiveness; the panic + coverage test is the substitute, and adding a sixth
  state will fail `TestStatus_AllStatesCovered`.
- `Status` values are exported package vars (structs cannot be constants). Treat them as immutable;
  nothing in the codebase reassigns them.
- `prepareRef` is a documented exception to "state data lives in the state": it is an
  infrastructure artifact that must survive lease recovery.
- If strictness later needs to include *persistence* (no exported `Snapshot` at all), the escape
  hatch is event-sourced persistence (ADR-0001 option C), not a bigger `Snapshot`.

## Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Plain `string`/`int` + validate at each use | Re-checks the same rules in handlers, adapters, and tests; allows id mix-ups |
| `type Status int` with exported constants | `Status(42)` is constructible outside the package |
| Generic `Option[T]` / `Result[T, E]` | Unidiomatic Go; `(T, error)` already carries the same information with tooling support |
| Keeping `status` + nullable fields | Illegal combinations remain representable; every method must re-check them |
| Full event sourcing now | Solves a persistence-encapsulation problem we don't have yet, at the cost of replay/versioning (ADR-0001) |
