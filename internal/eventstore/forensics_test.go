package eventstore

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestVerdictGroundingSummaryRoundTripPersistence is the BLOCKER test from the
// forensics-instrumentation plan: it exercises the full
// marshal → Append → LoadByCorrelation → unmarshal → re-marshal cycle for the
// new VerdictGroundingSummaryPayload and asserts byte equality so any future
// store-layer change that strips fields (json.RawMessage aliasing,
// canonicalization, etc.) breaks loudly.
func TestVerdictGroundingSummaryRoundTripPersistence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	original := event.VerdictGroundingSummaryPayload{
		Reviewer:      "pr-data",
		OriginalCount: 4,
		GroundedCount: 1,
		DropReasons: map[event.GroundingDropReason]int{
			event.GroundingDropFileNotInScope:   2,
			event.GroundingDropTokenNotNearLine: 1,
		},
		OriginalOutcome: event.VerdictFail,
		FinalOutcome:    event.VerdictFail,
	}
	originalBytes := event.MustMarshal(original)

	evt := event.Envelope{
		ID:            event.NewID(),
		Type:          event.VerdictGroundingSummary,
		SchemaVersion: 1,
		CorrelationID: "corr-forensics-roundtrip",
		Source:        "handler:pr-data",
		Payload:       originalBytes,
	}

	if err := store.Append(ctx, "agg-forensics", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.LoadByCorrelation(ctx, "corr-forensics-roundtrip")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("want 1 event, got %d", len(loaded))
	}

	var roundTripped event.VerdictGroundingSummaryPayload
	if err := json.Unmarshal(loaded[0].Payload, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if roundTripped.Reviewer != original.Reviewer ||
		roundTripped.OriginalCount != original.OriginalCount ||
		roundTripped.GroundedCount != original.GroundedCount ||
		roundTripped.OriginalOutcome != original.OriginalOutcome ||
		roundTripped.FinalOutcome != original.FinalOutcome {
		t.Errorf("scalar fields drift: got %+v want %+v", roundTripped, original)
	}
	if len(roundTripped.DropReasons) != 2 ||
		roundTripped.DropReasons[event.GroundingDropFileNotInScope] != 2 ||
		roundTripped.DropReasons[event.GroundingDropTokenNotNearLine] != 1 {
		t.Errorf("DropReasons drift: %+v", roundTripped.DropReasons)
	}

	// Re-marshal must produce byte-equivalent JSON (modulo map key order, which
	// encoding/json sorts alphabetically — so this IS deterministic).
	reMarshaled := event.MustMarshal(roundTripped)
	if !bytes.Equal(reMarshaled, originalBytes) {
		t.Errorf("re-marshal byte mismatch:\n got:  %s\n want: %s", reMarshaled, originalBytes)
	}
}

// TestAIResponsePayloadOutputRawRoundTrip locks in that the new OutputRaw field
// survives the full persistence cycle without being stripped or altered.
func TestAIResponsePayloadOutputRawRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	rawText, _ := json.Marshal("LLM said: VERDICT: FAIL\n1. critical: foo")
	groundedText, _ := json.Marshal("No grounded issues found in the changed lines for this review category.\n\nVERDICT: PASS")

	original := event.AIResponsePayload{
		Phase:      "pr-category-review",
		Backend:    "claude",
		DurationMS: 12345,
		Output:     groundedText,
		OutputRaw:  rawText,
	}

	evt := event.Envelope{
		ID:            event.NewID(),
		Type:          event.AIResponseReceived,
		SchemaVersion: 1,
		CorrelationID: "corr-output-raw",
		Source:        "handler:pr-data",
		Payload:       event.MustMarshal(original),
	}

	if err := store.Append(ctx, "agg-output-raw", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}
	loaded, err := store.LoadByCorrelation(ctx, "corr-output-raw")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var got event.AIResponsePayload
	if err := json.Unmarshal(loaded[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(got.Output, groundedText) {
		t.Errorf("Output drift: got %s want %s", got.Output, groundedText)
	}
	if !bytes.Equal(got.OutputRaw, rawText) {
		t.Errorf("OutputRaw drift: got %s want %s", got.OutputRaw, rawText)
	}
}
