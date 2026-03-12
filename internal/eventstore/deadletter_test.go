package eventstore

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// mockPublisher records published events for verification.
type mockPublisher struct {
	mu     sync.Mutex
	events []event.Envelope
	err    error // if set, Publish returns this error
}

func (p *mockPublisher) Publish(_ context.Context, env event.Envelope) error {
	if p.err != nil {
		return p.err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, env)
	return nil
}

func TestRetryDeadLetters_HappyPath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store an event
	evt := makeEvent(event.WorkflowRequested, `{"prompt":"test"}`)
	if err := store.Append(ctx, "agg-1", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Record a dead letter for that event
	dl := DeadLetter{
		ID:       "dl-1",
		EventID:  string(evt.ID),
		Handler:  "test-handler",
		Error:    "temporary failure",
		Attempts: 1,
		FailedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.RecordDeadLetter(ctx, dl); err != nil {
		t.Fatalf("record dead letter: %v", err)
	}

	// Retry
	pub := &mockPublisher{}
	result, err := RetryDeadLetters(ctx, store, pub, slog.Default())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Attempted != 1 {
		t.Errorf("expected 1 attempted, got %d", result.Attempted)
	}
	if result.Succeeded != 1 {
		t.Errorf("expected 1 succeeded, got %d", result.Succeeded)
	}
	if result.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", result.Failed)
	}

	// Verify event was republished
	if len(pub.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.events))
	}
	if pub.events[0].ID != evt.ID {
		t.Error("republished event ID mismatch")
	}

	// Verify dead letter was deleted
	dls, err := store.LoadDeadLetters(ctx)
	if err != nil {
		t.Fatalf("load dead letters: %v", err)
	}
	if len(dls) != 0 {
		t.Errorf("expected 0 dead letters after retry, got %d", len(dls))
	}
}

func TestRetryDeadLetters_EventNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Record a dead letter for a non-existent event
	dl := DeadLetter{
		ID:       "dl-1",
		EventID:  "nonexistent-event",
		Handler:  "test-handler",
		Error:    "boom",
		Attempts: 1,
		FailedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.RecordDeadLetter(ctx, dl); err != nil {
		t.Fatalf("record: %v", err)
	}

	pub := &mockPublisher{}
	result, err := RetryDeadLetters(ctx, store, pub, slog.Default())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(result.Errors))
	}
}

func TestRetryDeadLetters_PublishFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	evt := makeEvent(event.WorkflowRequested, `{"prompt":"test"}`)
	if err := store.Append(ctx, "agg-1", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}

	dl := DeadLetter{
		ID:       "dl-1",
		EventID:  string(evt.ID),
		Handler:  "test-handler",
		Error:    "original error",
		Attempts: 1,
		FailedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.RecordDeadLetter(ctx, dl); err != nil {
		t.Fatalf("record: %v", err)
	}

	pub := &mockPublisher{err: context.DeadlineExceeded}
	result, err := RetryDeadLetters(ctx, store, pub, slog.Default())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}

	// Dead letter should NOT be deleted when publish fails
	dls, err := store.LoadDeadLetters(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(dls) != 1 {
		t.Errorf("dead letter should remain when publish fails, got %d", len(dls))
	}
}

func TestRetryDeadLetters_EmptyQueue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pub := &mockPublisher{}
	result, err := RetryDeadLetters(ctx, store, pub, slog.Default())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Attempted != 0 {
		t.Errorf("expected 0 attempted, got %d", result.Attempted)
	}
}

// failingDeleteStore wraps a real SQLiteStore and overrides DeleteDeadLetter
// to always return an error, allowing publish to succeed but delete to fail.
type failingDeleteStore struct {
	Store
}

func (f *failingDeleteStore) DeleteDeadLetter(_ context.Context, _ string) error {
	return errors.New("simulated delete failure")
}

// TestRetryDeadLetters_DeleteAfterPublishFails verifies that when publish
// succeeds but DeleteDeadLetter fails, the result counters reflect a failure
// (not a success) for that dead letter.
func TestRetryDeadLetters_DeleteAfterPublishFails(t *testing.T) {
	base := newTestStore(t)
	ctx := context.Background()

	evt := makeEvent(event.WorkflowRequested, `{"prompt":"test"}`)
	if err := base.Append(ctx, "agg-1", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}

	dl := DeadLetter{
		ID:       "dl-del-fail",
		EventID:  string(evt.ID),
		Handler:  "test-handler",
		Error:    "original error",
		Attempts: 1,
		FailedAt: time.Now().Format(time.RFC3339),
	}
	if err := base.RecordDeadLetter(ctx, dl); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Wrap with a store that fails on DeleteDeadLetter.
	store := &failingDeleteStore{Store: base}
	pub := &mockPublisher{}

	result, err := RetryDeadLetters(ctx, store, pub, slog.Default())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if result.Attempted != 1 {
		t.Errorf("expected 1 attempted, got %d", result.Attempted)
	}
	// Publish succeeded but delete failed → counted as failed, not succeeded.
	if result.Failed != 1 {
		t.Errorf("expected 1 failed (delete error), got %d", result.Failed)
	}
	if result.Succeeded != 0 {
		t.Errorf("expected 0 succeeded, got %d", result.Succeeded)
	}
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error entry, got %d", len(result.Errors))
	}

	// Publish was still called successfully (event was re-published).
	if len(pub.events) != 1 {
		t.Errorf("expected 1 published event, got %d", len(pub.events))
	}
}
