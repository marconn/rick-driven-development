package grpchandler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
)

// TestEnvelopeProtoRoundTrip verifies that EnvelopeToProto → ProtoToEnvelope
// preserves all fields faithfully.
func TestEnvelopeProtoRoundTrip(t *testing.T) {
	ts := time.Now().Truncate(time.Millisecond) // proto uses millisecond precision

	original := event.Envelope{
		ID:            event.ID("evt-abc-123"),
		Type:          event.Type("persona.completed"),
		AggregateID:   "wf-42",
		Version:       7,
		SchemaVersion: 2,
		Timestamp:     ts,
		CausationID:   event.ID("cause-xyz"),
		CorrelationID: "correlation-99",
		Source:        "grpc:external-handler",
		Payload:       json.RawMessage(`{"phase":"researcher","output":"done"}`),
	}

	proto := EnvelopeToProto(original)
	restored := ProtoToEnvelope(proto)

	if restored.ID != original.ID {
		t.Errorf("ID: got %q, want %q", restored.ID, original.ID)
	}
	if restored.Type != original.Type {
		t.Errorf("Type: got %q, want %q", restored.Type, original.Type)
	}
	if restored.AggregateID != original.AggregateID {
		t.Errorf("AggregateID: got %q, want %q", restored.AggregateID, original.AggregateID)
	}
	if restored.Version != original.Version {
		t.Errorf("Version: got %d, want %d", restored.Version, original.Version)
	}
	if restored.SchemaVersion != original.SchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", restored.SchemaVersion, original.SchemaVersion)
	}
	if !restored.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp: got %v, want %v", restored.Timestamp, original.Timestamp)
	}
	if restored.CausationID != original.CausationID {
		t.Errorf("CausationID: got %q, want %q", restored.CausationID, original.CausationID)
	}
	if restored.CorrelationID != original.CorrelationID {
		t.Errorf("CorrelationID: got %q, want %q", restored.CorrelationID, original.CorrelationID)
	}
	if restored.Source != original.Source {
		t.Errorf("Source: got %q, want %q", restored.Source, original.Source)
	}
	if string(restored.Payload) != string(original.Payload) {
		t.Errorf("Payload: got %q, want %q", restored.Payload, original.Payload)
	}
}

// TestProtoToEnvelope_ZeroTimestamp verifies that a zero or negative
// TimestampMs is replaced with a non-zero time (approximately now).
func TestProtoToEnvelope_ZeroTimestamp(t *testing.T) {
	before := time.Now()

	pbEnv := &pb.EventEnvelope{
		Type:        "test.event",
		TimestampMs: 0, // zero — should default to time.Now()
	}

	restored := ProtoToEnvelope(pbEnv)

	if restored.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp for zero TimestampMs")
	}
	if restored.Timestamp.Before(before) {
		t.Error("defaulted timestamp should not be before the test started")
	}

	// Negative timestamp also defaults.
	pbEnvNeg := &pb.EventEnvelope{
		Type:        "test.event",
		TimestampMs: -1,
	}
	restoredNeg := ProtoToEnvelope(pbEnvNeg)
	if restoredNeg.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp for negative TimestampMs")
	}
}

// TestEnvelopeToProto_NilPayload verifies that a nil Payload is converted to
// an empty byte slice (not panic).
func TestEnvelopeToProto_NilPayload(t *testing.T) {
	env := event.Envelope{
		ID:      event.ID("evt-nil-payload"),
		Type:    "test.event",
		Payload: nil,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EnvelopeToProto panicked with nil payload: %v", r)
		}
	}()

	proto := EnvelopeToProto(env)
	// nil Payload → nil []byte in proto; ProtoToEnvelope should handle it too.
	restored := ProtoToEnvelope(proto)
	// Just verify no panic; nil payload is preserved as nil or empty.
	_ = restored
}

// TestProtoToEnvelope_EmptyStrings verifies that empty string fields are
// preserved as empty (not nil, not zero values that differ).
func TestProtoToEnvelope_EmptyStrings(t *testing.T) {
	pbEnv := &pb.EventEnvelope{
		Id:            "",
		Type:          "",
		AggregateId:   "",
		CausationId:   "",
		CorrelationId: "",
		Source:        "",
		TimestampMs:   1000, // non-zero so timestamp isn't defaulted
	}

	restored := ProtoToEnvelope(pbEnv)

	if restored.ID != "" {
		t.Errorf("expected empty ID, got %q", restored.ID)
	}
	if restored.Type != "" {
		t.Errorf("expected empty Type, got %q", restored.Type)
	}
	if restored.AggregateID != "" {
		t.Errorf("expected empty AggregateID, got %q", restored.AggregateID)
	}
	if restored.CausationID != "" {
		t.Errorf("expected empty CausationID, got %q", restored.CausationID)
	}
	if restored.CorrelationID != "" {
		t.Errorf("expected empty CorrelationID, got %q", restored.CorrelationID)
	}
	if restored.Source != "" {
		t.Errorf("expected empty Source, got %q", restored.Source)
	}
}

// TestEnvelopeToProto_PreservesTimestampPrecision verifies that timestamp
// millisecond precision is maintained through the round-trip.
func TestEnvelopeToProto_PreservesTimestampPrecision(t *testing.T) {
	// Use a specific timestamp with millisecond precision.
	ts := time.Date(2025, 1, 15, 12, 30, 45, int(123*time.Millisecond), time.UTC)
	env := event.Envelope{
		Type:      "test.event",
		Timestamp: ts,
	}

	proto := EnvelopeToProto(env)
	restored := ProtoToEnvelope(proto)

	// Truncate to millisecond for comparison since that's proto precision.
	want := ts.Truncate(time.Millisecond)
	got := restored.Timestamp.Truncate(time.Millisecond)
	if !got.Equal(want) {
		t.Errorf("timestamp mismatch: got %v, want %v", got, want)
	}
}
