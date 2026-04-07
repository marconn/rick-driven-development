# package eventstore

Event persistence layer for Rick — append-only SQLite store with optimistic concurrency, snapshots, dead letters, and business-key tag indexing.

## Files
- `store.go` — `Store` interface, sentinel errors, `DeadLetter`, `Snapshot`, `PositionedEvent` types
- `sqlite.go` — `SQLiteStore` implementation, schema migration, all 14 method impls
- `upcasting.go` — `UpcastingStore` decorator that runs `event.Registry.Upcast` on all read paths
- `deadletter.go` — `RetryDeadLetters` helper: loads DLs, republishes via `Publisher`, deletes on success
- `*_test.go` — in-memory SQLite tests; `sqlite_loadall_test.go` exercises catch-up cursor semantics

## Key types
- `Store` interface — 14 methods:
  - `Append(ctx, aggregateID, expectedVersion, events)` — atomic insert, optimistic concurrency check
  - `Load(ctx, aggregateID)` — all events for an aggregate, version order
  - `LoadFrom(ctx, aggregateID, fromVersion)` — events from version onwards
  - `LoadByCorrelation(ctx, correlationID)` — cross-aggregate correlation lookup, rowid order
  - `LoadAll(ctx, afterPosition, limit)` — global stream by `rowid`, used for catch-up subscriptions
  - `LoadEvent(ctx, eventID)` — single event by ID, returns `ErrEventNotFound`
  - `SaveSnapshot(ctx, snapshot)` / `LoadSnapshot(ctx, aggregateID)` — aggregate state cache, returns `ErrSnapshotNotFound`
  - `RecordDeadLetter` / `LoadDeadLetters` / `DeleteDeadLetter` — failed delivery tracking
  - `SaveTags(ctx, correlationID, map)` / `LoadByTag(ctx, key, value)` — business-key index
  - `Close()` — releases the underlying `*sql.DB`
- `SQLiteStore` — concrete impl, single connection (`SetMaxOpenConns(1)`) to share `:memory:` and avoid WAL writer contention
- `UpcastingStore` — embeds `Store`, intercepts `Load`/`LoadFrom`/`LoadByCorrelation`/`LoadAll` to apply registered upcasters
- `PositionedEvent{Position int64, Event event.Envelope}` — `rowid` is the global position
- `Publisher` interface — minimal `Publish(ctx, env)` shape used by `RetryDeadLetters`, satisfied by `eventbus.Bus`

## Schema
- `events` — `id PK, type, aggregate_id, version, schema_version, timestamp, causation_id, correlation_id, source, payload`; `UNIQUE(aggregate_id, version)`; indexes on `(aggregate_id, version)`, `correlation_id`, `type`
- `snapshots` — `aggregate_id PK, version, state, timestamp`; upsert via `ON CONFLICT`
- `dead_letters` — `id PK, event_id, handler, error, attempts, failed_at`
- `event_tags` — `(correlation_id, key, value)` with `UNIQUE` constraint; index on `(key, value)` for `LoadByTag`
- Migration runs idempotently on `NewSQLiteStore` via `CREATE TABLE IF NOT EXISTS`

## Patterns
- Optimistic concurrency: `Append` reads `MAX(version)` inside the tx and rejects with `ErrConcurrencyConflict` if it differs from `expectedVersion`; UNIQUE violations are also wrapped to the same sentinel
- Atomic batches: all events in one `Append` share a single tx — all-or-nothing
- WAL mode + tuned PRAGMAs: `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `mmap_size=256MB`, `cache_size=64MB`, `temp_store=memory`, `foreign_keys=ON`
- Idempotent tag writes via `INSERT OR IGNORE` — safe to call repeatedly with the same business keys
- Catch-up subscriptions: drain `LoadAll(afterPosition, limit)` until empty, then switch to live bus subscription (used by projection runner)
- Upcasting at read time, never at write time — old payloads stay on disk, are migrated in memory on every load
- Errors wrapped with `eventstore:` prefix per the project convention; sentinels: `ErrConcurrencyConflict`, `ErrSnapshotNotFound`, `ErrEventNotFound`

## Related
- `../event` — `Envelope`, `Type`, `ID`, `Registry` for upcasters
- `../engine` — primary consumer; `WorkflowAggregate` uses `LoadByCorrelation`, tag indexing happens in the engine on `WorkflowRequested`
- `../eventbus` — provides the `Publisher` used by `RetryDeadLetters`; outbox bus persists via `Append` before publish
- `../projection` — projection runner uses `LoadAll` for catch-up before live subscription
