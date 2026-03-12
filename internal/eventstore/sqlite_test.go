package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func makeEvent(eventType event.Type, payload string) event.Envelope {
	return event.Envelope{
		ID:            event.NewID(),
		Type:          eventType,
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-1",
		Source:        "test",
		Payload:       json.RawMessage(payload),
	}
}

func TestAppendAndLoad(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := []event.Envelope{
		makeEvent("test.first", `{"step":1}`),
		makeEvent("test.second", `{"step":2}`),
	}

	err := store.Append(ctx, "agg-1", 0, events)
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.Load(ctx, "agg-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 events, got %d", len(loaded))
	}
	if loaded[0].Version != 1 {
		t.Errorf("expected version 1, got %d", loaded[0].Version)
	}
	if loaded[1].Version != 2 {
		t.Errorf("expected version 2, got %d", loaded[1].Version)
	}
	if loaded[0].AggregateID != "agg-1" {
		t.Errorf("expected aggregate agg-1, got %s", loaded[0].AggregateID)
	}
}

func TestAppendEmptySlice(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.Append(ctx, "agg-1", 0, nil)
	if err != nil {
		t.Fatalf("append empty should not error: %v", err)
	}
}

func TestOptimisticConcurrency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// First append succeeds
	err := store.Append(ctx, "agg-1", 0, []event.Envelope{
		makeEvent("test.event", `{}`),
	})
	if err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Second append with wrong expected version fails
	err = store.Append(ctx, "agg-1", 0, []event.Envelope{
		makeEvent("test.event", `{}`),
	})
	if !errors.Is(err, ErrConcurrencyConflict) {
		t.Errorf("expected concurrency conflict, got: %v", err)
	}

	// Append with correct expected version succeeds
	err = store.Append(ctx, "agg-1", 1, []event.Envelope{
		makeEvent("test.event", `{}`),
	})
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
}

func TestLoadFrom(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := []event.Envelope{
		makeEvent("test.1", `{}`),
		makeEvent("test.2", `{}`),
		makeEvent("test.3", `{}`),
	}
	if err := store.Append(ctx, "agg-1", 0, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.LoadFrom(ctx, "agg-1", 2)
	if err != nil {
		t.Fatalf("load from: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 events from version 2, got %d", len(loaded))
	}
	if loaded[0].Version != 2 {
		t.Errorf("expected first event version 2, got %d", loaded[0].Version)
	}
}

func TestLoadByCorrelation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Events across different aggregates with same correlation
	e1 := makeEvent("test.1", `{}`)
	e1.CorrelationID = "shared-corr"
	e2 := makeEvent("test.2", `{}`)
	e2.CorrelationID = "shared-corr"
	e3 := makeEvent("test.3", `{}`)
	e3.CorrelationID = "other-corr"

	if err := store.Append(ctx, "agg-1", 0, []event.Envelope{e1}); err != nil {
		t.Fatalf("append agg-1: %v", err)
	}
	if err := store.Append(ctx, "agg-2", 0, []event.Envelope{e2}); err != nil {
		t.Fatalf("append agg-2: %v", err)
	}
	if err := store.Append(ctx, "agg-3", 0, []event.Envelope{e3}); err != nil {
		t.Fatalf("append agg-3: %v", err)
	}

	loaded, err := store.LoadByCorrelation(ctx, "shared-corr")
	if err != nil {
		t.Fatalf("load by correlation: %v", err)
	}
	if len(loaded) != 2 {
		t.Errorf("expected 2 events with shared-corr, got %d", len(loaded))
	}
}

func TestSnapshots(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No snapshot initially
	_, err := store.LoadSnapshot(ctx, "agg-1")
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("expected snapshot not found, got: %v", err)
	}

	// Save snapshot
	snap := Snapshot{
		AggregateID: "agg-1",
		Version:     5,
		State:       json.RawMessage(`{"phase":"review","iteration":2}`),
		Timestamp:   time.Now().Format(time.RFC3339),
	}
	if err := store.SaveSnapshot(ctx, snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// Load snapshot
	loaded, err := store.LoadSnapshot(ctx, "agg-1")
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if loaded.Version != 5 {
		t.Errorf("expected version 5, got %d", loaded.Version)
	}

	// Update snapshot (upsert)
	snap.Version = 10
	snap.State = json.RawMessage(`{"phase":"complete"}`)
	if err := store.SaveSnapshot(ctx, snap); err != nil {
		t.Fatalf("update snapshot: %v", err)
	}
	loaded, err = store.LoadSnapshot(ctx, "agg-1")
	if err != nil {
		t.Fatalf("load updated snapshot: %v", err)
	}
	if loaded.Version != 10 {
		t.Errorf("expected version 10, got %d", loaded.Version)
	}
}

func TestConcurrentAppends(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const goroutines = 10
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.Append(ctx, "agg-1", 0, []event.Envelope{
				makeEvent("test.event", `{}`),
			})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)

	successes := 0
	conflicts := 0
	for err := range errCh {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConcurrencyConflict) {
			conflicts++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("expected exactly 1 success, got %d", successes)
	}
	if conflicts != goroutines-1 {
		t.Errorf("expected %d conflicts, got %d", goroutines-1, conflicts)
	}
}

func TestLoadNonexistentAggregate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	loaded, err := store.Load(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("load nonexistent: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected 0 events, got %d", len(loaded))
	}
}

func TestTimestampPreservation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Microsecond) // SQLite RFC3339Nano precision
	e := makeEvent("test.event", `{}`)
	e.Timestamp = now

	if err := store.Append(ctx, "agg-1", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.Load(ctx, "agg-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded[0].Timestamp.Equal(now) {
		t.Errorf("timestamp not preserved: got %v, want %v", loaded[0].Timestamp, now)
	}
}

// TestStoreImplementsInterface verifies the compile-time interface compliance.
func TestStoreImplementsInterface(t *testing.T) {
	var _ Store = (*SQLiteStore)(nil)
}

// --- SaveTags + LoadByTag ---

func TestSaveAndLoadByTag_Single(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.SaveTags(ctx, "corr-1", map[string]string{"ticket": "PROJ-123"}); err != nil {
		t.Fatalf("save tags: %v", err)
	}

	ids, err := store.LoadByTag(ctx, "ticket", "PROJ-123")
	if err != nil {
		t.Fatalf("load by tag: %v", err)
	}
	if len(ids) != 1 || ids[0] != "corr-1" {
		t.Errorf("expected [corr-1], got %v", ids)
	}
}

func TestSaveAndLoadByTag_MultipleTags(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tags := map[string]string{
		"ticket": "PROJ-456",
		"repo":   "my-repo",
	}
	if err := store.SaveTags(ctx, "corr-2", tags); err != nil {
		t.Fatalf("save tags: %v", err)
	}

	ids, err := store.LoadByTag(ctx, "repo", "my-repo")
	if err != nil {
		t.Fatalf("load by repo tag: %v", err)
	}
	if len(ids) != 1 || ids[0] != "corr-2" {
		t.Errorf("expected [corr-2], got %v", ids)
	}

	ids2, err := store.LoadByTag(ctx, "ticket", "PROJ-456")
	if err != nil {
		t.Fatalf("load by ticket tag: %v", err)
	}
	if len(ids2) != 1 || ids2[0] != "corr-2" {
		t.Errorf("expected [corr-2], got %v", ids2)
	}
}

func TestLoadByTag_NoMatches(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ids, err := store.LoadByTag(ctx, "ticket", "nonexistent")
	if err != nil {
		t.Fatalf("load by tag: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty result, got %v", ids)
	}
}

func TestLoadByTag_MultipleCorrelationsWithSameTag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.SaveTags(ctx, "corr-A", map[string]string{"workflow_id": "jira-dev"}); err != nil {
		t.Fatalf("save tags A: %v", err)
	}
	if err := store.SaveTags(ctx, "corr-B", map[string]string{"workflow_id": "jira-dev"}); err != nil {
		t.Fatalf("save tags B: %v", err)
	}

	ids, err := store.LoadByTag(ctx, "workflow_id", "jira-dev")
	if err != nil {
		t.Fatalf("load by tag: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 correlation IDs, got %d: %v", len(ids), ids)
	}
}

func TestSaveTags_InsertOrIgnoreDeduplication(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Save the same tag twice — INSERT OR IGNORE should prevent duplicate rows.
	if err := store.SaveTags(ctx, "corr-1", map[string]string{"source": "gh:owner/repo#1"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := store.SaveTags(ctx, "corr-1", map[string]string{"source": "gh:owner/repo#1"}); err != nil {
		t.Fatalf("second save (should not error): %v", err)
	}

	ids, err := store.LoadByTag(ctx, "source", "gh:owner/repo#1")
	if err != nil {
		t.Fatalf("load by tag: %v", err)
	}
	// DISTINCT means one row even if there were duplicates
	if len(ids) != 1 {
		t.Errorf("expected 1 distinct correlation ID, got %d", len(ids))
	}
}

func TestSaveTags_EmptyMapIsNoOp(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Empty tags map must return nil without touching the DB.
	if err := store.SaveTags(ctx, "corr-1", map[string]string{}); err != nil {
		t.Fatalf("empty save tags: %v", err)
	}

	// Nothing should be stored.
	ids, err := store.LoadByTag(ctx, "anything", "anything")
	if err != nil {
		t.Fatalf("load by tag: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 results after empty save, got %d", len(ids))
	}
}

// --- Append UNIQUE constraint fallback ---

// TestAppend_DuplicateEventIDReturnsConflict forces the UNIQUE(id) path by
// inserting the same event.ID into two different aggregates so the version
// check passes but the INSERT fails on the primary-key constraint.
func TestAppend_DuplicateEventIDReturnsConflict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	e := makeEvent("test.event", `{}`)

	// First insert succeeds.
	if err := store.Append(ctx, "agg-dup-1", 0, []event.Envelope{e}); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Second insert reuses the same event ID on a new aggregate.
	// The version check passes (agg-dup-2 is empty → currentVersion=0=expectedVersion),
	// but the INSERT fails on the PRIMARY KEY constraint.
	err := store.Append(ctx, "agg-dup-2", 0, []event.Envelope{e})
	if !errors.Is(err, ErrConcurrencyConflict) {
		t.Errorf("expected ErrConcurrencyConflict from UNIQUE id constraint, got: %v", err)
	}
}

// --- Append multi-event atomicity ---

// TestAppend_AtomicRollbackOnPartialConflict verifies that when a batch of
// events partially conflicts (2nd event reuses an existing ID), the entire
// transaction is rolled back and no events are persisted.
func TestAppend_AtomicRollbackOnPartialConflict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Pre-insert event2's ID so it will cause a constraint violation.
	e1 := makeEvent("test.first", `{}`)
	e2 := makeEvent("test.second", `{}`)
	e3 := makeEvent("test.third", `{}`)

	// Put e2 into a different aggregate so its ID is already taken.
	if err := store.Append(ctx, "agg-existing", 0, []event.Envelope{e2}); err != nil {
		t.Fatalf("setup append: %v", err)
	}

	// Now attempt a 3-event batch on a fresh aggregate; e2 will collide on ID.
	err := store.Append(ctx, "agg-atomic", 0, []event.Envelope{e1, e2, e3})
	if !errors.Is(err, ErrConcurrencyConflict) {
		t.Fatalf("expected ErrConcurrencyConflict, got: %v", err)
	}

	// The transaction must have rolled back — agg-atomic must have 0 events.
	loaded, loadErr := store.Load(ctx, "agg-atomic")
	if loadErr != nil {
		t.Fatalf("load: %v", loadErr)
	}
	if len(loaded) != 0 {
		t.Errorf("expected 0 events after rolled-back batch, got %d", len(loaded))
	}
}
