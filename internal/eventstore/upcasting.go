package eventstore

import (
	"context"
	"fmt"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// UpcastingStore wraps a Store and applies upcasters from the event Registry
// to events as they are loaded. This ensures old events are automatically
// migrated to current schema versions.
type UpcastingStore struct {
	Store    // embedded base store
	registry *event.Registry
}

// NewUpcastingStore wraps a Store with automatic upcasting.
func NewUpcastingStore(store Store, registry *event.Registry) *UpcastingStore {
	return &UpcastingStore{Store: store, registry: registry}
}

func (u *UpcastingStore) Load(ctx context.Context, aggregateID string) ([]event.Envelope, error) {
	events, err := u.Store.Load(ctx, aggregateID)
	if err != nil {
		return nil, err
	}
	return u.upcastAll(events)
}

func (u *UpcastingStore) LoadFrom(ctx context.Context, aggregateID string, fromVersion int) ([]event.Envelope, error) {
	events, err := u.Store.LoadFrom(ctx, aggregateID, fromVersion)
	if err != nil {
		return nil, err
	}
	return u.upcastAll(events)
}

func (u *UpcastingStore) LoadByCorrelation(ctx context.Context, correlationID string) ([]event.Envelope, error) {
	events, err := u.Store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return nil, err
	}
	return u.upcastAll(events)
}

func (u *UpcastingStore) LoadAll(ctx context.Context, afterPosition int64, limit int) ([]PositionedEvent, error) {
	positioned, err := u.Store.LoadAll(ctx, afterPosition, limit)
	if err != nil {
		return nil, err
	}
	for i, pe := range positioned {
		payload, version, upErr := u.registry.Upcast(pe.Event.Type, pe.Event.SchemaVersion, pe.Event.Payload)
		if upErr != nil {
			return nil, fmt.Errorf("eventstore: upcast event %s (type=%s, v%d): %w",
				pe.Event.ID, pe.Event.Type, pe.Event.SchemaVersion, upErr)
		}
		positioned[i].Event.Payload = payload
		positioned[i].Event.SchemaVersion = version
	}
	return positioned, nil
}

func (u *UpcastingStore) upcastAll(events []event.Envelope) ([]event.Envelope, error) {
	for i, env := range events {
		payload, version, err := u.registry.Upcast(env.Type, env.SchemaVersion, env.Payload)
		if err != nil {
			return nil, fmt.Errorf("eventstore: upcast event %s (type=%s, v%d): %w",
				env.ID, env.Type, env.SchemaVersion, err)
		}
		events[i].Payload = payload
		events[i].SchemaVersion = version
	}
	return events, nil
}
