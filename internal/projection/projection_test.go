package projection

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// --- helpers ---

func makeEnvelope(t event.Type, aggID string, version int, payload any) event.Envelope {
	data, _ := json.Marshal(payload)
	return event.Envelope{
		ID:            event.NewID(),
		Type:          t,
		AggregateID:   aggID,
		Version:       version,
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-1",
		Source:        "test",
		Payload:       data,
	}
}

func newTestStore(t *testing.T) *eventstore.SQLiteStore {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// --- WorkflowStatusProjection ---

func TestWorkflowStatusProjection_Lifecycle(t *testing.T) {
	proj := NewWorkflowStatusProjection()
	ctx := context.Background()

	steps := []event.Envelope{
		makeEnvelope(event.WorkflowRequested, "wf-1", 1,
			event.WorkflowRequestedPayload{Prompt: "do stuff", WorkflowID: "dev", Source: "raw"}),
		makeEnvelope(event.WorkflowStarted, "wf-1", 2,
			event.WorkflowStartedPayload{WorkflowID: "dev", Phases: []string{"research", "develop"}}),
		makeEnvelope(event.WorkflowCompleted, "wf-1", 3,
			event.WorkflowCompletedPayload{Result: "done"}),
	}

	for _, env := range steps {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle %s: %v", env.Type, err)
		}
	}

	ws, ok := proj.Get("wf-1")
	if !ok {
		t.Fatal("workflow not found")
	}
	if ws.Status != "completed" {
		t.Errorf("expected completed, got %s", ws.Status)
	}
	if ws.WorkflowID != "dev" {
		t.Errorf("expected workflow_id dev, got %s", ws.WorkflowID)
	}
	if len(ws.Phases) != 2 {
		t.Errorf("expected 2 phases, got %d", len(ws.Phases))
	}
}

func TestWorkflowStatusProjection_Failed(t *testing.T) {
	proj := NewWorkflowStatusProjection()
	ctx := context.Background()

	steps := []event.Envelope{
		makeEnvelope(event.WorkflowRequested, "wf-2", 1,
			event.WorkflowRequestedPayload{Prompt: "fail", WorkflowID: "bad"}),
		makeEnvelope(event.WorkflowFailed, "wf-2", 2,
			event.WorkflowFailedPayload{Reason: "phase exploded", Phase: "develop"}),
	}

	for _, env := range steps {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle %s: %v", env.Type, err)
		}
	}

	ws, ok := proj.Get("wf-2")
	if !ok {
		t.Fatal("workflow not found")
	}
	if ws.Status != "failed" {
		t.Errorf("expected failed, got %s", ws.Status)
	}
	if ws.FailReason != "phase exploded" {
		t.Errorf("expected 'phase exploded', got %s", ws.FailReason)
	}
}

func TestWorkflowStatusProjection_All(t *testing.T) {
	proj := NewWorkflowStatusProjection()
	ctx := context.Background()

	for _, id := range []string{"wf-1", "wf-2", "wf-3"} {
		env := makeEnvelope(event.WorkflowRequested, id, 1,
			event.WorkflowRequestedPayload{Prompt: "test", WorkflowID: "w"})
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	all := proj.All()
	if len(all) != 3 {
		t.Errorf("expected 3 workflows, got %d", len(all))
	}
}

func TestWorkflowStatusProjection_IgnoresUnrelatedEvents(t *testing.T) {
	proj := NewWorkflowStatusProjection()
	ctx := context.Background()

	env := makeEnvelope(event.PersonaCompleted, "wf-1:persona:researcher", 1,
		event.PersonaCompletedPayload{Persona: "researcher", DurationMS: 1000})
	env.CorrelationID = "wf-1"
	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	_, ok := proj.Get("wf-1")
	if ok {
		t.Error("should not track workflow from PersonaCompleted event alone")
	}
}

// --- TokenUsageProjection ---

func TestTokenUsageProjection_AggregatesTokens(t *testing.T) {
	proj := NewTokenUsageProjection()
	ctx := context.Background()

	events := []event.Envelope{
		makeEnvelope(event.AIResponseReceived, "wf-1", 1,
			event.AIResponsePayload{Phase: "research", Backend: "claude", TokensUsed: 1000, DurationMS: 500}),
		makeEnvelope(event.AIResponseReceived, "wf-1", 2,
			event.AIResponsePayload{Phase: "develop", Backend: "gemini", TokensUsed: 2000, DurationMS: 1000}),
		makeEnvelope(event.AIResponseReceived, "wf-1", 3,
			event.AIResponsePayload{Phase: "research", Backend: "claude", TokensUsed: 500, DurationMS: 300}),
	}

	for _, env := range events {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	tu, ok := proj.Get("wf-1")
	if !ok {
		t.Fatal("token usage not found")
	}
	if tu.Total != 3500 {
		t.Errorf("expected total 3500, got %d", tu.Total)
	}
	if tu.ByPhase["research"] != 1500 {
		t.Errorf("expected research 1500, got %d", tu.ByPhase["research"])
	}
	if tu.ByPhase["develop"] != 2000 {
		t.Errorf("expected develop 2000, got %d", tu.ByPhase["develop"])
	}
	if tu.ByBackend["claude"] != 1500 {
		t.Errorf("expected claude 1500, got %d", tu.ByBackend["claude"])
	}
	if tu.ByBackend["gemini"] != 2000 {
		t.Errorf("expected gemini 2000, got %d", tu.ByBackend["gemini"])
	}
}

func TestTokenUsageProjection_IgnoresZeroTokens(t *testing.T) {
	proj := NewTokenUsageProjection()
	ctx := context.Background()

	env := makeEnvelope(event.AIResponseReceived, "wf-1", 1,
		event.AIResponsePayload{Phase: "research", Backend: "claude", TokensUsed: 0, DurationMS: 100})
	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	_, ok := proj.Get("wf-1")
	if ok {
		t.Error("should not create entry for zero-token response")
	}
}

func TestTokenUsageProjection_IgnoresNonAIEvents(t *testing.T) {
	proj := NewTokenUsageProjection()
	ctx := context.Background()

	env := makeEnvelope(event.WorkflowStarted, "wf-1", 1,
		event.WorkflowStartedPayload{WorkflowID: "w", Phases: []string{"a"}})
	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	_, ok := proj.Get("wf-1")
	if ok {
		t.Error("should not track from non-AI events")
	}
}

func TestTokenUsageProjection_GetReturnsCopy(t *testing.T) {
	proj := NewTokenUsageProjection()
	ctx := context.Background()

	env := makeEnvelope(event.AIResponseReceived, "wf-1", 1,
		event.AIResponsePayload{Phase: "research", Backend: "claude", TokensUsed: 100, DurationMS: 50})
	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	tu, _ := proj.Get("wf-1")
	tu.ByPhase["research"] = 999 // mutate the copy

	tu2, _ := proj.Get("wf-1")
	if tu2.ByPhase["research"] != 100 {
		t.Error("Get should return a copy, not a reference")
	}
}

// --- PhaseTimelineProjection ---

func TestPhaseTimelineProjection_TracksLifecycle(t *testing.T) {
	proj := NewPhaseTimelineProjection()
	ctx := context.Background()

	env := makeEnvelope(event.PersonaCompleted, "wf-1:persona:researcher", 1,
		event.PersonaCompletedPayload{Persona: "researcher", DurationMS: 5000})
	env.CorrelationID = "wf-1"

	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle %s: %v", env.Type, err)
	}

	pt, ok := proj.Get("wf-1", "researcher")
	if !ok {
		t.Fatal("timeline not found")
	}
	if pt.Status != "done" {
		t.Errorf("expected done, got %s", pt.Status)
	}
	if pt.Duration != 5*time.Second {
		t.Errorf("expected 5s duration, got %v", pt.Duration)
	}
	if pt.Iterations != 1 {
		t.Errorf("expected iteration 1, got %d", pt.Iterations)
	}
}

func TestPhaseTimelineProjection_TracksFailed(t *testing.T) {
	proj := NewPhaseTimelineProjection()
	ctx := context.Background()

	env := makeEnvelope(event.PersonaFailed, "wf-1:persona:developer", 1,
		event.PersonaFailedPayload{Persona: "developer", Error: "compile error", DurationMS: 2000})
	env.CorrelationID = "wf-1"

	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	pt, ok := proj.Get("wf-1", "developer")
	if !ok {
		t.Fatal("timeline not found")
	}
	if pt.Status != "failed" {
		t.Errorf("expected failed, got %s", pt.Status)
	}
	if pt.Duration != 2*time.Second {
		t.Errorf("expected 2s duration, got %v", pt.Duration)
	}
}

func TestPhaseTimelineProjection_MultipleIterations(t *testing.T) {
	proj := NewPhaseTimelineProjection()
	ctx := context.Background()

	// Two PersonaCompleted events for the same persona simulate two iterations.
	iter1 := makeEnvelope(event.PersonaCompleted, "wf-1:persona:reviewer", 1,
		event.PersonaCompletedPayload{Persona: "reviewer", DurationMS: 10000})
	iter1.CorrelationID = "wf-1"

	iter2 := makeEnvelope(event.PersonaCompleted, "wf-1:persona:reviewer", 2,
		event.PersonaCompletedPayload{Persona: "reviewer", DurationMS: 10000})
	iter2.CorrelationID = "wf-1"

	for _, env := range []event.Envelope{iter1, iter2} {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	pt, ok := proj.Get("wf-1", "reviewer")
	if !ok {
		t.Fatal("timeline not found")
	}
	if pt.Iterations != 2 {
		t.Errorf("expected 2 iterations, got %d", pt.Iterations)
	}
}

func TestPhaseTimelineProjection_ForWorkflow(t *testing.T) {
	proj := NewPhaseTimelineProjection()
	ctx := context.Background()

	personas := []string{"researcher", "developer", "reviewer"}
	for i, persona := range personas {
		env := makeEnvelope(event.PersonaCompleted, "wf-1:persona:"+persona, i+1,
			event.PersonaCompletedPayload{Persona: persona, DurationMS: 1000})
		env.CorrelationID = "wf-1"
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	result := proj.ForWorkflow("wf-1")
	if len(result) != 3 {
		t.Errorf("expected 3 timelines, got %d", len(result))
	}
}

// --- Runner ---

func TestRunner_CatchUpAndLive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Pre-populate store with historical events
	events := []event.Envelope{
		makeEnvelope(event.WorkflowRequested, "wf-1", 1,
			event.WorkflowRequestedPayload{Prompt: "test", WorkflowID: "dev", Source: "raw"}),
		makeEnvelope(event.WorkflowStarted, "wf-1", 2,
			event.WorkflowStartedPayload{WorkflowID: "dev", Phases: []string{"a", "b"}}),
	}
	if err := store.Append(ctx, "wf-1", 0, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Set up projections
	bus := eventbus.NewChannelBus()
	t.Cleanup(func() { _ = bus.Close() })

	wsProj := NewWorkflowStatusProjection()
	runner := NewRunner(store, bus, slog.Default())
	runner.Register(wsProj)

	// Start catches up from store
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runner.Stop()

	// Verify catch-up processed historical events
	ws, ok := wsProj.Get("wf-1")
	if !ok {
		t.Fatal("workflow not found after catch-up")
	}
	if ws.Status != "running" {
		t.Errorf("expected running after catch-up, got %s", ws.Status)
	}

	// Publish a live event
	completeEvt := makeEnvelope(event.WorkflowCompleted, "wf-1", 3,
		event.WorkflowCompletedPayload{Result: "success"})
	if err := bus.Publish(ctx, completeEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for async dispatch
	_ = bus.Close()

	ws, _ = wsProj.Get("wf-1")
	if ws.Status != "completed" {
		t.Errorf("expected completed after live event, got %s", ws.Status)
	}
}

func TestRunner_Position(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := []event.Envelope{
		makeEnvelope(event.WorkflowRequested, "wf-1", 1,
			event.WorkflowRequestedPayload{Prompt: "test", WorkflowID: "w"}),
	}
	if err := store.Append(ctx, "wf-1", 0, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	bus := eventbus.NewChannelBus()
	t.Cleanup(func() { _ = bus.Close() })

	runner := NewRunner(store, bus, slog.Default())
	runner.Register(NewWorkflowStatusProjection())

	if err := runner.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runner.Stop()

	if runner.Position() == 0 {
		t.Error("position should be > 0 after catch-up")
	}
}

// --- VerdictProjection ---

func TestVerdictProjection_AccumulatesVerdicts(t *testing.T) {
	proj := NewVerdictProjection()
	ctx := context.Background()

	events := []event.Envelope{
		makeEnvelope(event.VerdictRendered, "wf-1:persona:reviewer", 1,
			event.VerdictPayload{
				Phase:       "develop",
				SourcePhase: "reviewer",
				Outcome:     event.VerdictFail,
				Summary:     "Missing error handling",
				Issues: []event.Issue{
					{Severity: "major", Category: "correctness", Description: "unchecked error", File: "main.go", Line: 42},
				},
			}),
		makeEnvelope(event.VerdictRendered, "wf-1:persona:qa", 2,
			event.VerdictPayload{
				Phase:       "develop",
				SourcePhase: "qa",
				Outcome:     event.VerdictPass,
				Summary:     "All tests pass",
			}),
	}

	for _, env := range events {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	records := proj.ForWorkflow("corr-1")
	if len(records) != 2 {
		t.Fatalf("expected 2 verdicts, got %d", len(records))
	}

	// First verdict: reviewer fail.
	if records[0].SourcePhase != "reviewer" {
		t.Errorf("expected source_phase reviewer, got %s", records[0].SourcePhase)
	}
	if records[0].Outcome != "fail" {
		t.Errorf("expected outcome fail, got %s", records[0].Outcome)
	}
	if len(records[0].Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(records[0].Issues))
	}
	if records[0].Issues[0].File != "main.go" {
		t.Errorf("expected file main.go, got %s", records[0].Issues[0].File)
	}
	if records[0].Issues[0].Line != 42 {
		t.Errorf("expected line 42, got %d", records[0].Issues[0].Line)
	}

	// Second verdict: qa pass.
	if records[1].SourcePhase != "qa" {
		t.Errorf("expected source_phase qa, got %s", records[1].SourcePhase)
	}
	if records[1].Outcome != "pass" {
		t.Errorf("expected outcome pass, got %s", records[1].Outcome)
	}
}

func TestVerdictProjection_IgnoresNonVerdictEvents(t *testing.T) {
	proj := NewVerdictProjection()
	ctx := context.Background()

	env := makeEnvelope(event.PersonaCompleted, "wf-1:persona:researcher", 1,
		event.PersonaCompletedPayload{Persona: "researcher", DurationMS: 1000})
	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	records := proj.ForWorkflow("corr-1")
	if len(records) != 0 {
		t.Errorf("expected no verdicts from non-verdict event, got %d", len(records))
	}
}

func TestVerdictProjection_ForWorkflowReturnsCopy(t *testing.T) {
	proj := NewVerdictProjection()
	ctx := context.Background()

	env := makeEnvelope(event.VerdictRendered, "wf-1:persona:reviewer", 1,
		event.VerdictPayload{
			Phase:       "develop",
			SourcePhase: "reviewer",
			Outcome:     event.VerdictPass,
			Summary:     "looks good",
			Issues: []event.Issue{
				{Severity: "minor", Category: "style", Description: "nit"},
			},
		})
	if err := proj.Handle(ctx, env); err != nil {
		t.Fatalf("handle: %v", err)
	}

	records := proj.ForWorkflow("corr-1")
	records[0].Summary = "mutated"
	records[0].Issues[0].Severity = "mutated"

	original := proj.ForWorkflow("corr-1")
	if original[0].Summary != "looks good" {
		t.Error("ForWorkflow should return a deep copy — summary was mutated")
	}
	if original[0].Issues[0].Severity != "minor" {
		t.Error("ForWorkflow should return a deep copy — issue was mutated")
	}
}

func TestVerdictProjection_ForWorkflowEmptyReturnsNil(t *testing.T) {
	proj := NewVerdictProjection()

	records := proj.ForWorkflow("nonexistent")
	if records != nil {
		t.Errorf("expected nil for unknown correlation, got %v", records)
	}
}

func TestVerdictProjection_MultipleCorrelations(t *testing.T) {
	proj := NewVerdictProjection()
	ctx := context.Background()

	env1 := makeEnvelope(event.VerdictRendered, "wf-a:persona:reviewer", 1,
		event.VerdictPayload{Phase: "develop", SourcePhase: "reviewer", Outcome: event.VerdictPass, Summary: "ok"})
	env1.CorrelationID = "wf-a"

	env2 := makeEnvelope(event.VerdictRendered, "wf-b:persona:qa", 1,
		event.VerdictPayload{Phase: "develop", SourcePhase: "qa", Outcome: event.VerdictFail, Summary: "bad"})
	env2.CorrelationID = "wf-b"

	for _, env := range []event.Envelope{env1, env2} {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	a := proj.ForWorkflow("wf-a")
	b := proj.ForWorkflow("wf-b")

	if len(a) != 1 || a[0].Outcome != "pass" {
		t.Errorf("wf-a: expected 1 pass verdict, got %v", a)
	}
	if len(b) != 1 || b[0].Outcome != "fail" {
		t.Errorf("wf-b: expected 1 fail verdict, got %v", b)
	}
}

func TestVerdictProjection_AccumulatesRetries(t *testing.T) {
	proj := NewVerdictProjection()
	ctx := context.Background()

	// Simulate a retry loop: reviewer fails twice then passes.
	for i, outcome := range []event.VerdictOutcome{event.VerdictFail, event.VerdictFail, event.VerdictPass} {
		env := makeEnvelope(event.VerdictRendered, "wf-1:persona:reviewer", i+1,
			event.VerdictPayload{Phase: "develop", SourcePhase: "reviewer", Outcome: outcome, Summary: "iter"})
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	records := proj.ForWorkflow("corr-1")
	if len(records) != 3 {
		t.Fatalf("expected 3 verdicts (all iterations kept), got %d", len(records))
	}
	if records[0].Outcome != "fail" || records[1].Outcome != "fail" || records[2].Outcome != "pass" {
		t.Errorf("unexpected outcome sequence: %s, %s, %s",
			records[0].Outcome, records[1].Outcome, records[2].Outcome)
	}
}

// --- WorkflowStatusProjection Paused/Resumed ---

func TestWorkflowStatusProjection_PausedAndResumed(t *testing.T) {
	proj := NewWorkflowStatusProjection()
	ctx := context.Background()

	steps := []event.Envelope{
		makeEnvelope(event.WorkflowRequested, "wf-p", 1,
			event.WorkflowRequestedPayload{Prompt: "test", WorkflowID: "plan-btu"}),
		makeEnvelope(event.WorkflowStarted, "wf-p", 2,
			event.WorkflowStartedPayload{WorkflowID: "plan-btu", Phases: []string{"architect", "estimator"}}),
		makeEnvelope(event.WorkflowPaused, "wf-p", 3,
			event.WorkflowPausedPayload{Reason: "hint from architect: confidence=0.60", Source: "engine:hint-review"}),
	}

	for _, env := range steps {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle %s: %v", env.Type, err)
		}
	}

	ws, ok := proj.Get("wf-p")
	if !ok {
		t.Fatal("workflow not found")
	}
	if ws.Status != "paused" {
		t.Errorf("expected paused, got %s", ws.Status)
	}

	// Resume the workflow.
	resumeEnv := makeEnvelope(event.WorkflowResumed, "wf-p", 4,
		event.WorkflowResumedPayload{Reason: "hint approved for architect"})
	if err := proj.Handle(ctx, resumeEnv); err != nil {
		t.Fatalf("handle resumed: %v", err)
	}

	ws, _ = proj.Get("wf-p")
	if ws.Status != "running" {
		t.Errorf("expected running after resume, got %s", ws.Status)
	}
}

func TestWorkflowStatusProjection_PausedThenCancelled(t *testing.T) {
	proj := NewWorkflowStatusProjection()
	ctx := context.Background()

	steps := []event.Envelope{
		makeEnvelope(event.WorkflowRequested, "wf-pc", 1,
			event.WorkflowRequestedPayload{Prompt: "test", WorkflowID: "workspace-dev"}),
		makeEnvelope(event.WorkflowStarted, "wf-pc", 2,
			event.WorkflowStartedPayload{WorkflowID: "workspace-dev", Phases: []string{"researcher"}}),
		makeEnvelope(event.WorkflowPaused, "wf-pc", 3,
			event.WorkflowPausedPayload{Reason: "operator requested", Source: "operator"}),
		makeEnvelope(event.WorkflowCancelled, "wf-pc", 4,
			event.WorkflowCancelledPayload{Reason: "no longer needed"}),
	}

	for _, env := range steps {
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle %s: %v", env.Type, err)
		}
	}

	ws, ok := proj.Get("wf-pc")
	if !ok {
		t.Fatal("workflow not found")
	}
	if ws.Status != "cancelled" {
		t.Errorf("expected cancelled, got %s", ws.Status)
	}
}

// --- TokenUsageProjection.ForWorkflow ---

func TestTokenUsageProjection_ForWorkflow_AggregatesPersonaScopes(t *testing.T) {
	proj := NewTokenUsageProjection()
	ctx := context.Background()

	// Three different persona-scoped aggregates for the same correlation.
	personaEvents := []struct {
		aggID  string
		phase  string
		tokens int
	}{
		{"corr1:persona:researcher", "research", 1000},
		{"corr1:persona:developer", "develop", 2000},
		{"corr1:persona:reviewer", "review", 500},
	}

	for i, pe := range personaEvents {
		env := event.Envelope{
			ID:            event.NewID(),
			Type:          event.AIResponseReceived,
			AggregateID:   pe.aggID,
			Version:       i + 1,
			SchemaVersion: 1,
			Timestamp:     makeEnvelope(event.AIResponseReceived, pe.aggID, i+1, nil).Timestamp,
			CorrelationID: "corr1",
			Source:        "test",
		}
		data, _ := json.Marshal(event.AIResponsePayload{
			Phase:      pe.phase,
			Backend:    "claude",
			TokensUsed: pe.tokens,
			DurationMS: 100,
		})
		env.Payload = data

		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	tu, ok := proj.ForWorkflow("corr1")
	if !ok {
		t.Fatal("ForWorkflow should return true when persona aggregates exist")
	}
	if tu.Total != 3500 {
		t.Errorf("expected total 3500 across all personas, got %d", tu.Total)
	}
	if tu.ByPhase["research"] != 1000 {
		t.Errorf("expected research=1000, got %d", tu.ByPhase["research"])
	}
	if tu.ByPhase["develop"] != 2000 {
		t.Errorf("expected develop=2000, got %d", tu.ByPhase["develop"])
	}
	if tu.ByPhase["review"] != 500 {
		t.Errorf("expected review=500, got %d", tu.ByPhase["review"])
	}
	if tu.ByBackend["claude"] != 3500 {
		t.Errorf("expected claude=3500, got %d", tu.ByBackend["claude"])
	}
}

func TestTokenUsageProjection_ForWorkflow_NonExistent(t *testing.T) {
	proj := NewTokenUsageProjection()

	tu, ok := proj.ForWorkflow("corr-nonexistent")
	if ok {
		t.Error("ForWorkflow should return false for unknown correlation")
	}
	if tu.Total != 0 {
		t.Errorf("expected total 0 for non-existent workflow, got %d", tu.Total)
	}
}

func TestTokenUsageProjection_ForWorkflow_MultipleCorrelations(t *testing.T) {
	proj := NewTokenUsageProjection()
	ctx := context.Background()

	for _, tc := range []struct {
		aggID string
		corr  string
	}{
		{"corrA:persona:developer", "corrA"},
		{"corrB:persona:developer", "corrB"},
	} {
		env := event.Envelope{
			ID:            event.NewID(),
			Type:          event.AIResponseReceived,
			AggregateID:   tc.aggID,
			Version:       1,
			SchemaVersion: 1,
			CorrelationID: tc.corr,
			Source:        "test",
		}
		data, _ := json.Marshal(event.AIResponsePayload{
			Phase:      "develop",
			Backend:    "claude",
			TokensUsed: 100,
			DurationMS: 50,
		})
		env.Payload = data
		if err := proj.Handle(ctx, env); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	tuA, okA := proj.ForWorkflow("corrA")
	tuB, okB := proj.ForWorkflow("corrB")

	if !okA || tuA.Total != 100 {
		t.Errorf("corrA: expected 100, got %d (ok=%v)", tuA.Total, okA)
	}
	if !okB || tuB.Total != 100 {
		t.Errorf("corrB: expected 100, got %d (ok=%v)", tuB.Total, okB)
	}
}

// --- Interface compliance ---

func TestWorkflowStatusProjection_ImplementsProjector(t *testing.T) {
	var _ Projector = (*WorkflowStatusProjection)(nil)
}

func TestTokenUsageProjection_ImplementsProjector(t *testing.T) {
	var _ Projector = (*TokenUsageProjection)(nil)
}

func TestPhaseTimelineProjection_ImplementsProjector(t *testing.T) {
	var _ Projector = (*PhaseTimelineProjection)(nil)
}

func TestVerdictProjection_ImplementsProjector(t *testing.T) {
	var _ Projector = (*VerdictProjection)(nil)
}
