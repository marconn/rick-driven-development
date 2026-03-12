package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

func TestUpcastingStoreLoad(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	// Register event type at v2 with an upcaster from v1
	registry := event.NewRegistry()
	registry.Register("test.event", "test", 2)
	_ = registry.RegisterUpcaster("test.event", 1, func(p json.RawMessage) (json.RawMessage, error) {
		var data map[string]any
		if err := json.Unmarshal(p, &data); err != nil {
			return nil, err
		}
		data["migrated"] = true
		return json.Marshal(data)
	})

	store := NewUpcastingStore(base, registry)

	// Store a v1 event
	e := event.Envelope{
		ID:            event.NewID(),
		Type:          "test.event",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-1",
		Source:        "test",
		Payload:       json.RawMessage(`{"name":"original"}`),
	}
	if err := base.Append(ctx, "agg-1", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Load through upcasting store
	loaded, err := store.Load(ctx, "agg-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 event, got %d", len(loaded))
	}
	if loaded[0].SchemaVersion != 2 {
		t.Errorf("expected schema version 2, got %d", loaded[0].SchemaVersion)
	}

	var data map[string]any
	if err := json.Unmarshal(loaded[0].Payload, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data["migrated"] != true {
		t.Error("expected migrated=true after upcasting")
	}
	if data["name"] != "original" {
		t.Errorf("expected name=original, got %v", data["name"])
	}
}

func TestUpcastingStorePassthrough(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	registry := event.NewRegistry()
	registry.Register("test.event", "test", 1) // current version = 1

	store := NewUpcastingStore(base, registry)

	// Store a v1 event (already current)
	e := event.Envelope{
		ID:            event.NewID(),
		Type:          "test.event",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-1",
		Source:        "test",
		Payload:       json.RawMessage(`{"key":"value"}`),
	}
	if err := base.Append(ctx, "agg-1", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.Load(ctx, "agg-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(loaded[0].Payload) != `{"key":"value"}` {
		t.Errorf("expected unchanged payload, got %s", loaded[0].Payload)
	}
}

// TestUpcastingStoreLoadAll verifies that LoadAll through the UpcastingStore
// applies upcasters to each PositionedEvent returned from the base store.
func TestUpcastingStoreLoadAll(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	registry := event.NewRegistry()
	registry.Register("test.positioned", "test", 2)
	_ = registry.RegisterUpcaster("test.positioned", 1, func(p json.RawMessage) (json.RawMessage, error) {
		var data map[string]any
		if err := json.Unmarshal(p, &data); err != nil {
			return nil, err
		}
		data["upcasted"] = true
		return json.Marshal(data)
	})

	store := NewUpcastingStore(base, registry)

	e := event.Envelope{
		ID:            event.NewID(),
		Type:          "test.positioned",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-loadall",
		Source:        "test",
		Payload:       json.RawMessage(`{"x":1}`),
	}
	if err := base.Append(ctx, "agg-loadall", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	results, err := store.LoadAll(ctx, 0, 0)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 positioned event, got %d", len(results))
	}

	pe := results[0]
	if pe.Event.SchemaVersion != 2 {
		t.Errorf("expected schema version 2 after upcast, got %d", pe.Event.SchemaVersion)
	}
	var data map[string]any
	if err := json.Unmarshal(pe.Event.Payload, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data["upcasted"] != true {
		t.Error("expected upcasted=true after LoadAll upcasting")
	}
}

// TestUpcastingStoreLoadAll_ErrorPath verifies that when the upcaster returns
// an error, LoadAll propagates an error containing the event ID, type, and version.
func TestUpcastingStoreLoadAll_ErrorPath(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	registry := event.NewRegistry()
	registry.Register("test.broken", "test", 2)
	upcastErr := errors.New("upcaster exploded")
	_ = registry.RegisterUpcaster("test.broken", 1, func(_ json.RawMessage) (json.RawMessage, error) {
		return nil, upcastErr
	})

	store := NewUpcastingStore(base, registry)

	e := event.Envelope{
		ID:            event.NewID(),
		Type:          "test.broken",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-err",
		Source:        "test",
		Payload:       json.RawMessage(`{}`),
	}
	if err := base.Append(ctx, "agg-broken", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	_, err := store.LoadAll(ctx, 0, 0)
	if err == nil {
		t.Fatal("expected error from broken upcaster, got nil")
	}
	// Error message must contain the event ID and type.
	if !strings.Contains(err.Error(), string(e.ID)) {
		t.Errorf("error should contain event ID %s, got: %v", e.ID, err)
	}
	if !strings.Contains(err.Error(), "test.broken") {
		t.Errorf("error should contain event type 'test.broken', got: %v", err)
	}
}

// TestUpcastAll_ErrorPath verifies that upcastAll (via Load) wraps errors with
// event ID, type, and version when an upcaster fails.
func TestUpcastAll_ErrorPath(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	registry := event.NewRegistry()
	registry.Register("test.broken-load", "test", 2)
	upcastErr := errors.New("upcast failed")
	_ = registry.RegisterUpcaster("test.broken-load", 1, func(_ json.RawMessage) (json.RawMessage, error) {
		return nil, upcastErr
	})

	store := NewUpcastingStore(base, registry)

	e := event.Envelope{
		ID:            event.NewID(),
		Type:          "test.broken-load",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-load-err",
		Source:        "test",
		Payload:       json.RawMessage(`{}`),
	}
	if err := base.Append(ctx, "agg-broken-load", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	_, err := store.Load(ctx, "agg-broken-load")
	if err == nil {
		t.Fatal("expected error from broken upcaster, got nil")
	}
	// Error must identify the event.
	if !strings.Contains(err.Error(), string(e.ID)) {
		t.Errorf("error should contain event ID %s, got: %v", e.ID, err)
	}
	if !strings.Contains(err.Error(), "test.broken-load") {
		t.Errorf("error should contain event type, got: %v", err)
	}
}

func TestUpcastingStoreLoadByCorrelation(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	registry := event.NewRegistry()
	registry.Register("test.event", "test", 2)
	_ = registry.RegisterUpcaster("test.event", 1, func(p json.RawMessage) (json.RawMessage, error) {
		var data map[string]any
		_ = json.Unmarshal(p, &data)
		data["v2"] = true
		return json.Marshal(data)
	})

	store := NewUpcastingStore(base, registry)

	e := event.Envelope{
		ID:            event.NewID(),
		Type:          "test.event",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "shared",
		Source:        "test",
		Payload:       json.RawMessage(`{}`),
	}
	if err := base.Append(ctx, "agg-1", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.LoadByCorrelation(ctx, "shared")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded[0].SchemaVersion != 2 {
		t.Errorf("expected v2, got %d", loaded[0].SchemaVersion)
	}
}
