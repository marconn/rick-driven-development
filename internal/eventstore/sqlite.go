package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using SQLite with WAL mode.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a new SQLite-backed event store.
// dsn is the database path (use ":memory:" for testing).
func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("eventstore: open db: %w", err)
	}

	// For in-memory databases, all connections must share the same underlying
	// SQLite connection — otherwise each pool connection gets an empty database.
	// For file-based databases, a single connection is still safe and avoids
	// WAL writer contention; readers can use read-only connections if needed.
	db.SetMaxOpenConns(1)

	// Enable WAL mode for concurrent reads (no-op for :memory: but harmless)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("eventstore: set WAL mode: %w", err)
	}
	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("eventstore: enable foreign keys: %w", err)
	}
	// Performance PRAGMAs safe with WAL mode
	pragmas := []string{
		"PRAGMA synchronous = NORMAL",    // safe with WAL, faster than FULL
		"PRAGMA busy_timeout = 5000",     // wait 5s on lock contention
		"PRAGMA temp_store = memory",     // temp tables in RAM
		"PRAGMA mmap_size = 268435456",   // 256MB memory-mapped I/O
		"PRAGMA cache_size = -64000",     // 64MB page cache
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("eventstore: %s: %w", p, err)
		}
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("eventstore: migrate: %w", err)
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	schema := `
    CREATE TABLE IF NOT EXISTS events (
        id TEXT PRIMARY KEY,
        type TEXT NOT NULL,
        aggregate_id TEXT NOT NULL,
        version INTEGER NOT NULL,
        schema_version INTEGER NOT NULL DEFAULT 1,
        timestamp TEXT NOT NULL,
        causation_id TEXT,
        correlation_id TEXT NOT NULL,
        source TEXT,
        payload TEXT NOT NULL,
        UNIQUE(aggregate_id, version)
    );

    CREATE INDEX IF NOT EXISTS idx_events_aggregate ON events(aggregate_id, version);
    CREATE INDEX IF NOT EXISTS idx_events_correlation ON events(correlation_id);
    CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);

    CREATE TABLE IF NOT EXISTS snapshots (
        aggregate_id TEXT PRIMARY KEY,
        version INTEGER NOT NULL,
        state TEXT NOT NULL,
        timestamp TEXT NOT NULL
    );

    CREATE TABLE IF NOT EXISTS dead_letters (
        id TEXT PRIMARY KEY,
        event_id TEXT NOT NULL,
        handler TEXT NOT NULL,
        error TEXT NOT NULL,
        attempts INTEGER NOT NULL,
        failed_at TEXT NOT NULL
    );

    CREATE TABLE IF NOT EXISTS event_tags (
        correlation_id TEXT NOT NULL,
        key TEXT NOT NULL,
        value TEXT NOT NULL,
        UNIQUE(correlation_id, key, value)
    );

    CREATE INDEX IF NOT EXISTS idx_event_tags_lookup ON event_tags(key, value);
    `
	_, err := s.db.Exec(schema)
	return err
}

func (s *SQLiteStore) Append(ctx context.Context, aggregateID string, expectedVersion int, events []event.Envelope) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eventstore: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is best-effort cleanup

	// Verify current version matches expected (optimistic concurrency)
	var currentVersion int
	err = tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM events WHERE aggregate_id = ?",
		aggregateID,
	).Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("eventstore: check version: %w", err)
	}
	if currentVersion != expectedVersion {
		return fmt.Errorf("%w: expected version %d, current is %d",
			ErrConcurrencyConflict, expectedVersion, currentVersion)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO events (id, type, aggregate_id, version, schema_version, timestamp, causation_id, correlation_id, source, payload)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("eventstore: prepare insert: %w", err)
	}
	defer stmt.Close() //nolint:errcheck // best-effort cleanup

	for i, e := range events {
		version := expectedVersion + i + 1
		_, err := stmt.ExecContext(ctx,
			string(e.ID),
			string(e.Type),
			aggregateID,
			version,
			e.SchemaVersion,
			e.Timestamp.Format(time.RFC3339Nano),
			string(e.CausationID),
			e.CorrelationID,
			e.Source,
			string(e.Payload),
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return fmt.Errorf("%w: version %d already exists for aggregate %s",
					ErrConcurrencyConflict, version, aggregateID)
			}
			return fmt.Errorf("eventstore: insert event: %w", err)
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) Load(ctx context.Context, aggregateID string) ([]event.Envelope, error) {
	return s.loadEvents(ctx,
		"SELECT id, type, aggregate_id, version, schema_version, timestamp, causation_id, correlation_id, source, payload FROM events WHERE aggregate_id = ? ORDER BY version",
		aggregateID,
	)
}

func (s *SQLiteStore) LoadFrom(ctx context.Context, aggregateID string, fromVersion int) ([]event.Envelope, error) {
	return s.loadEvents(ctx,
		"SELECT id, type, aggregate_id, version, schema_version, timestamp, causation_id, correlation_id, source, payload FROM events WHERE aggregate_id = ? AND version >= ? ORDER BY version",
		aggregateID, fromVersion,
	)
}

func (s *SQLiteStore) LoadByCorrelation(ctx context.Context, correlationID string) ([]event.Envelope, error) {
	return s.loadEvents(ctx,
		"SELECT id, type, aggregate_id, version, schema_version, timestamp, causation_id, correlation_id, source, payload FROM events WHERE correlation_id = ? ORDER BY timestamp",
		correlationID,
	)
}

func (s *SQLiteStore) loadEvents(ctx context.Context, query string, args ...any) ([]event.Envelope, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eventstore: query: %w", err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup

	var envelopes []event.Envelope
	for rows.Next() {
		e, err := scanEnvelope(rows)
		if err != nil {
			return nil, err
		}
		envelopes = append(envelopes, e)
	}
	return envelopes, rows.Err()
}

// scanEnvelope reads a single event row into an Envelope.
func scanEnvelope(rows *sql.Rows) (event.Envelope, error) {
	var (
		e           event.Envelope
		id          string
		eventType   string
		timestamp   string
		causationID string
		payload     string
	)
	err := rows.Scan(&id, &eventType, &e.AggregateID, &e.Version, &e.SchemaVersion,
		&timestamp, &causationID, &e.CorrelationID, &e.Source, &payload)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("eventstore: scan: %w", err)
	}
	e.ID = event.ID(id)
	e.Type = event.Type(eventType)
	e.CausationID = event.ID(causationID)
	e.Payload = json.RawMessage(payload)

	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("eventstore: parse timestamp: %w", err)
	}
	e.Timestamp = t
	return e, nil
}

func (s *SQLiteStore) SaveSnapshot(ctx context.Context, snapshot Snapshot) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshots (aggregate_id, version, state, timestamp) VALUES (?, ?, ?, ?)
         ON CONFLICT(aggregate_id) DO UPDATE SET version=excluded.version, state=excluded.state, timestamp=excluded.timestamp`,
		snapshot.AggregateID, snapshot.Version, string(snapshot.State), snapshot.Timestamp,
	)
	if err != nil {
		return fmt.Errorf("eventstore: save snapshot: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LoadSnapshot(ctx context.Context, aggregateID string) (*Snapshot, error) {
	var snap Snapshot
	var state string
	err := s.db.QueryRowContext(ctx,
		"SELECT aggregate_id, version, state, timestamp FROM snapshots WHERE aggregate_id = ?",
		aggregateID,
	).Scan(&snap.AggregateID, &snap.Version, &state, &snap.Timestamp)
	if err == sql.ErrNoRows {
		return nil, ErrSnapshotNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("eventstore: load snapshot: %w", err)
	}
	snap.State = json.RawMessage(state)
	return &snap, nil
}

func (s *SQLiteStore) RecordDeadLetter(ctx context.Context, dl DeadLetter) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dead_letters (id, event_id, handler, error, attempts, failed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		dl.ID, dl.EventID, dl.Handler, dl.Error, dl.Attempts, dl.FailedAt,
	)
	if err != nil {
		return fmt.Errorf("eventstore: record dead letter: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LoadDeadLetters(ctx context.Context) ([]DeadLetter, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, event_id, handler, error, attempts, failed_at FROM dead_letters ORDER BY failed_at")
	if err != nil {
		return nil, fmt.Errorf("eventstore: load dead letters: %w", err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup

	var dls []DeadLetter
	for rows.Next() {
		var dl DeadLetter
		if err := rows.Scan(&dl.ID, &dl.EventID, &dl.Handler, &dl.Error, &dl.Attempts, &dl.FailedAt); err != nil {
			return nil, fmt.Errorf("eventstore: scan dead letter: %w", err)
		}
		dls = append(dls, dl)
	}
	return dls, rows.Err()
}

func (s *SQLiteStore) LoadAll(ctx context.Context, afterPosition int64, limit int) ([]PositionedEvent, error) {
	query := `SELECT rowid, id, type, aggregate_id, version, schema_version, timestamp,
		causation_id, correlation_id, source, payload
		FROM events WHERE rowid > ? ORDER BY rowid`
	args := []any{afterPosition}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eventstore: load all: %w", err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup

	var result []PositionedEvent
	for rows.Next() {
		var (
			position    int64
			id          string
			eventType   string
			e           event.Envelope
			timestamp   string
			causationID string
			payload     string
		)
		err := rows.Scan(&position, &id, &eventType, &e.AggregateID, &e.Version,
			&e.SchemaVersion, &timestamp, &causationID, &e.CorrelationID, &e.Source, &payload)
		if err != nil {
			return nil, fmt.Errorf("eventstore: scan all: %w", err)
		}
		e.ID = event.ID(id)
		e.Type = event.Type(eventType)
		e.CausationID = event.ID(causationID)
		e.Payload = json.RawMessage(payload)

		t, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("eventstore: parse timestamp: %w", err)
		}
		e.Timestamp = t
		result = append(result, PositionedEvent{Position: position, Event: e})
	}
	return result, rows.Err()
}

func (s *SQLiteStore) LoadEvent(ctx context.Context, eventID string) (*event.Envelope, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, aggregate_id, version, schema_version, timestamp,
			causation_id, correlation_id, source, payload
			FROM events WHERE id = ?`,
		eventID,
	)
	if err != nil {
		return nil, fmt.Errorf("eventstore: load event: %w", err)
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("eventstore: load event: %w", err)
		}
		return nil, ErrEventNotFound
	}
	e, err := scanEnvelope(rows)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *SQLiteStore) DeleteDeadLetter(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM dead_letters WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("eventstore: delete dead letter: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("eventstore: delete dead letter rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("eventstore: dead letter %q not found", id)
	}
	return nil
}

func (s *SQLiteStore) SaveTags(ctx context.Context, correlationID string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eventstore: save tags begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO event_tags (correlation_id, key, value) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("eventstore: save tags prepare: %w", err)
	}
	defer stmt.Close() //nolint:errcheck

	for k, v := range tags {
		if _, err := stmt.ExecContext(ctx, correlationID, k, v); err != nil {
			return fmt.Errorf("eventstore: save tag %s=%s: %w", k, v, err)
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) LoadByTag(ctx context.Context, key, value string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT correlation_id FROM event_tags WHERE key = ? AND value = ? ORDER BY correlation_id`,
		key, value,
	)
	if err != nil {
		return nil, fmt.Errorf("eventstore: load by tag: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("eventstore: scan tag: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
