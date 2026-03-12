package eventstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// Sentinel errors for the event store.
var (
	ErrConcurrencyConflict = errors.New("eventstore: optimistic concurrency conflict")
	ErrSnapshotNotFound    = errors.New("eventstore: snapshot not found")
	ErrEventNotFound       = errors.New("eventstore: event not found")
)

// DeadLetter records a failed event delivery for forensics and retry.
type DeadLetter struct {
	ID       string `json:"id"`
	EventID  string `json:"event_id"`
	Handler  string `json:"handler"`
	Error    string `json:"error"`
	Attempts int    `json:"attempts"`
	FailedAt string `json:"failed_at"`
}

// Snapshot holds a materialized aggregate state at a specific version.
type Snapshot struct {
	AggregateID string          `json:"aggregate_id"`
	Version     int             `json:"version"`
	State       json.RawMessage `json:"state"`
	Timestamp   string          `json:"timestamp"`
}

// PositionedEvent pairs an event with its global position in the store.
// Position is the monotonically increasing sequence used for catch-up subscriptions.
type PositionedEvent struct {
	Position int64
	Event    event.Envelope
}

// Store defines the event store interface.
type Store interface {
	// Append stores events with optimistic concurrency.
	// expectedVersion is the last known version; use 0 for new aggregates.
	// Events are appended atomically — all succeed or none.
	Append(ctx context.Context, aggregateID string, expectedVersion int, events []event.Envelope) error

	// Load returns all events for an aggregate in version order.
	Load(ctx context.Context, aggregateID string) ([]event.Envelope, error)

	// LoadFrom returns events for an aggregate starting from a specific version (inclusive).
	LoadFrom(ctx context.Context, aggregateID string, fromVersion int) ([]event.Envelope, error)

	// LoadByCorrelation returns all events with a given correlation ID across aggregates.
	LoadByCorrelation(ctx context.Context, correlationID string) ([]event.Envelope, error)

	// LoadAll returns events across all aggregates ordered by global position.
	// afterPosition filters to events after the given position (use 0 for all).
	// limit caps the number of returned events (use 0 for no limit).
	// Required for catch-up subscriptions and cross-aggregate projections.
	LoadAll(ctx context.Context, afterPosition int64, limit int) ([]PositionedEvent, error)

	// LoadEvent returns a single event by its ID.
	LoadEvent(ctx context.Context, eventID string) (*event.Envelope, error)

	// SaveSnapshot stores a snapshot for an aggregate.
	SaveSnapshot(ctx context.Context, snapshot Snapshot) error

	// LoadSnapshot returns the latest snapshot for an aggregate.
	LoadSnapshot(ctx context.Context, aggregateID string) (*Snapshot, error)

	// RecordDeadLetter stores a failed event delivery.
	RecordDeadLetter(ctx context.Context, dl DeadLetter) error

	// LoadDeadLetters returns all dead letter entries.
	LoadDeadLetters(ctx context.Context) ([]DeadLetter, error)

	// DeleteDeadLetter removes a dead letter entry after successful reprocessing.
	DeleteDeadLetter(ctx context.Context, id string) error

	// SaveTags associates key-value tags with a correlation ID.
	// Tags enable lookup by business keys (e.g., Jira ticket, repo+branch).
	// Multiple tags per correlation and multiple correlations per tag are supported.
	SaveTags(ctx context.Context, correlationID string, tags map[string]string) error

	// LoadByTag returns correlation IDs matching a tag key-value pair.
	// Use this to discover workflow correlation IDs from business identifiers.
	LoadByTag(ctx context.Context, key, value string) ([]string, error)

	// Close releases resources.
	Close() error
}
