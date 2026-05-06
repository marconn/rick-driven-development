package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// TestFeedbackChain_DeveloperPromptCarriesReviewerIssues is the regression
// test for the 2026-05-05 silent-feedback-drop class. The pre-collapse code
// had a contract mismatch between aggregate.decideVerdictRendered (writing
// the resolved handler name into FeedbackGeneratedPayload.TargetPhase) and
// AIHandler.buildPromptContext (matching against h.phase, the verb). The
// developer re-ran on every feedback iteration but its prompt never carried
// the reviewer's issues — burning token budgets blind.
//
// This test wires real Engine + real PersonaRunner + real AIHandler
// (developer) + real ReviewHandler (reviewer) and verifies that after a
// failing review verdict, the developer's iteration-2 backend request
// carries the reviewer's Issue.Description text in its user prompt.
//
// Without this test the contract can drift again: handler-side and
// aggregate-side unit tests are package-bounded and missed it last time.
func TestFeedbackChain_DeveloperPromptCarriesReviewerIssues(t *testing.T) {
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bus := eventbus.NewChannelBus()
	t.Cleanup(func() { _ = bus.Close() })

	logger := slog.New(slog.NewTextHandler(testWriter(t), &slog.HandlerOptions{Level: slog.LevelWarn}))

	def := WorkflowDef{
		ID:            "feedback-chain-test",
		Required:      []string{"developer", "reviewer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"developer": {},
			"reviewer":  {"developer"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
	}

	eng := NewEngine(store, bus, logger)
	eng.RegisterWorkflow(def)
	t.Cleanup(eng.Stop)

	reg := handler.NewRegistry()
	dispatcher := NewLocalDispatcher(reg)
	runner := NewPersonaRunner(store, bus, dispatcher, logger)
	runner.RegisterWorkflow(def)
	t.Cleanup(func() { _ = runner.Close() })

	// Developer backend records every prompt it received so we can inspect
	// what reached iteration 2.
	devBackend := &capturingBackend{name: "claude", out: "developer iteration output"}

	// Reviewer fails on the first iteration with a distinctive issue, passes
	// on the second so the workflow terminates cleanly.
	const distinctiveIssue = "MISSING_NULL_CHECK_AT_LINE_42"
	var reviewIter atomic.Int32
	reviewerBackend := &programmableBackend{
		name: "claude",
		next: func() string {
			n := reviewIter.Add(1)
			if n == 1 {
				return fmt.Sprintf("VERDICT: FAIL\n1. critical/correctness: %s in handler\n", distinctiveIssue)
			}
			return "VERDICT: PASS\nLooks good now."
		},
	}

	personas := persona.DefaultRegistry()
	builder := persona.NewPromptBuilder()

	devH := handler.NewAIHandler(handler.AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Template: "develop",
		Backend:  devBackend,
		Store:    store,
		Bus:      bus,
		Personas: personas,
		Builder:  builder,
	})
	if err := reg.Register(devH); err != nil {
		t.Fatal(err)
	}

	revH := handler.NewReviewHandler(handler.ReviewHandlerConfig{
		AIConfig: handler.AIHandlerConfig{
			Name:      "reviewer",
			Persona:   persona.Reviewer,
			Template:  "review",
			Backend:   reviewerBackend,
			Store:     store,
			Bus:       bus,
			Personas:  personas,
			Builder:   builder,
			PlainText: true,
		},
		TargetPersona: "developer",
	})
	if err := reg.Register(revH); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	eng.Start()
	runner.Start(ctx, reg)

	const wfID = "wf-feedback-chain"
	completed := awaitWorkflowResult(t, bus, wfID)

	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "Implement a feature",
		WorkflowID: def.ID,
	})).
		WithAggregate(wfID, 1).
		WithCorrelation(wfID).
		WithSource("test")
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("append req: %v", err)
	}
	if err := bus.Publish(ctx, reqEvt); err != nil {
		t.Fatalf("publish req: %v", err)
	}

	select {
	case got := <-completed:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout: workflow never completed (devPrompts=%d, reviewIter=%d)",
			devBackend.callCount(), reviewIter.Load())
	}

	// THE LOCKED-DOWN CONTRACT: the developer must have run twice (initial
	// + retrigger after FeedbackGenerated), and the second prompt must
	// carry the distinctive issue text from the reviewer's failing verdict.
	prompts := devBackend.prompts()
	if len(prompts) < 2 {
		t.Fatalf("developer must run at least twice (initial + feedback retrigger); got %d", len(prompts))
	}

	iter2 := prompts[1]
	if !strings.Contains(iter2, distinctiveIssue) {
		t.Errorf("iteration-2 developer prompt does not carry reviewer's issue text.\n"+
			"This is the silent-feedback-drop regression class.\n"+
			"Want substring: %q\nGot prompt:\n%s", distinctiveIssue, iter2)
	}
}

// TestFeedbackChain_AggregateEmitsHandlerNameInTargetPersona unit-tests the
// aggregate side of the same contract: a VerdictRendered with Persona =
// handler-name must produce FeedbackGenerated whose TargetPersona equals
// that same handler-name. Pinning this prevents a future change to
// decideVerdictRendered from re-introducing a verb-vs-name translation
// step.
func TestFeedbackChain_AggregateEmitsHandlerNameInTargetPersona(t *testing.T) {
	agg := NewWorkflowAggregate("wf-contract")
	agg.Status = StatusRunning
	agg.MaxIterations = 3
	agg.WorkflowDef = &WorkflowDef{
		Required:      []string{"developer", "reviewer"},
		MaxIterations: 3,
	}

	verdict := event.Envelope{
		ID:            "v-1",
		Type:          event.VerdictRendered,
		AggregateID:   "wf-contract",
		CorrelationID: "wf-contract",
		Payload: event.MustMarshal(event.VerdictPayload{
			Persona:       "developer",
			SourcePersona: "reviewer",
			Outcome:       event.VerdictFail,
			Summary:       "needs work",
			Issues: []event.Issue{{
				Severity: "major", Category: "correctness",
				Description: "fix this",
			}},
		}),
	}

	out, err := agg.Decide(verdict)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.FeedbackGenerated {
		t.Fatalf("expected single FeedbackGenerated, got %v", out)
	}

	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(out[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fb.TargetPersona != "developer" {
		t.Errorf("TargetPersona = %q; want handler name %q (no verb translation)", fb.TargetPersona, "developer")
	}
	if fb.SourcePersona != "reviewer" {
		t.Errorf("SourcePersona = %q; want handler name %q (no verb translation)", fb.SourcePersona, "reviewer")
	}
	// And the issue text must round-trip into the feedback so the developer's
	// next iteration prompt has something to act on.
	if len(fb.Issues) != 1 || !strings.Contains(fb.Issues[0].Description, "fix this") {
		t.Errorf("issue text lost in feedback round-trip: %+v", fb.Issues)
	}
}

// --- test backends ---

// capturingBackend records every prompt it sees and returns a fixed output.
type capturingBackend struct {
	name string
	out  string

	mu       sync.Mutex
	captured []string
}

func (b *capturingBackend) Name() string { return b.name }
func (b *capturingBackend) Run(_ context.Context, req backend.Request) (*backend.Response, error) {
	b.mu.Lock()
	b.captured = append(b.captured, req.UserPrompt)
	b.mu.Unlock()
	return &backend.Response{Output: b.out, Duration: time.Millisecond}, nil
}
func (b *capturingBackend) prompts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.captured))
	copy(out, b.captured)
	return out
}
func (b *capturingBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.captured)
}

// programmableBackend returns whatever its next() func produces on each call.
type programmableBackend struct {
	name string
	next func() string
}

func (b *programmableBackend) Name() string { return b.name }
func (b *programmableBackend) Run(_ context.Context, _ backend.Request) (*backend.Response, error) {
	return &backend.Response{Output: b.next(), Duration: time.Millisecond}, nil
}

// testWriter routes slog output to t.Log so failures show context but
// successes stay quiet.
func testWriter(t *testing.T) testLogWriter { return testLogWriter{t: t} }

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
