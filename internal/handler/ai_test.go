package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// ---------------------------------------------------------------------------
// Mock backend
// ---------------------------------------------------------------------------

type mockBackend struct {
	name     string
	response *backend.Response
	err      error
	lastReq  backend.Request
	// lastStickyKey + lastRotateOffset capture the backend-context routing
	// hints AIHandler plants before calling Run. Tests assert these so the
	// auto-retry rotation contract (different key/offset on retry) is
	// covered end-to-end, not just at the attempt-counter layer.
	lastStickyKey    string
	lastRotateOffset int
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) Capabilities() backend.Capabilities { return backend.Capabilities{} }

func (m *mockBackend) Run(ctx context.Context, req backend.Request) (*backend.Response, error) {
	m.lastReq = req
	m.lastStickyKey = backend.StickyKeyFromContext(ctx)
	m.lastRotateOffset = backend.RotateOffsetFromContext(ctx)
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

// ---------------------------------------------------------------------------
// Mock store (minimal: LoadByCorrelation + Append/Load to back AIRequestSent
// inline persistence)
// ---------------------------------------------------------------------------

type mockStore struct {
	correlationEvents map[string][]event.Envelope
	aggregateEvents   map[string][]event.Envelope
}

func newMockStore() *mockStore {
	return &mockStore{
		correlationEvents: make(map[string][]event.Envelope),
		aggregateEvents:   make(map[string][]event.Envelope),
	}
}

func (s *mockStore) LoadByCorrelation(_ context.Context, correlationID string) ([]event.Envelope, error) {
	return s.correlationEvents[correlationID], nil
}

// Append records events under the given aggregate ID. Used by AIHandler to
// persist AIRequestSent inline.
func (s *mockStore) Append(_ context.Context, aggregateID string, _ int, events []event.Envelope) error {
	s.aggregateEvents[aggregateID] = append(s.aggregateEvents[aggregateID], events...)
	return nil
}

// Load returns events for an aggregate ID — needed by AIHandler.persistRequestEvent
// to compute the next version.
func (s *mockStore) Load(_ context.Context, aggregateID string) ([]event.Envelope, error) {
	return s.aggregateEvents[aggregateID], nil
}
func (s *mockStore) LoadFrom(context.Context, string, int) ([]event.Envelope, error) { return nil, nil }
func (s *mockStore) LoadAll(context.Context, int64, int) ([]eventstore.PositionedEvent, error) {
	return nil, nil
}
func (s *mockStore) LoadEvent(context.Context, string) (*event.Envelope, error) { return nil, nil }
func (s *mockStore) SaveSnapshot(context.Context, eventstore.Snapshot) error    { return nil }
func (s *mockStore) LoadSnapshot(context.Context, string) (*eventstore.Snapshot, error) {
	return nil, nil
}
func (s *mockStore) RecordDeadLetter(context.Context, eventstore.DeadLetter) error { return nil }
func (s *mockStore) LoadDeadLetters(context.Context) ([]eventstore.DeadLetter, error) {
	return nil, nil
}
func (s *mockStore) DeleteDeadLetter(context.Context, string) error              { return nil }
func (s *mockStore) SaveTags(context.Context, string, map[string]string) error   { return nil }
func (s *mockStore) LoadByTag(context.Context, string, string) ([]string, error) { return nil, nil }
func (s *mockStore) Close() error                                                { return nil }

// ---------------------------------------------------------------------------
// AIHandler construction
// ---------------------------------------------------------------------------

func TestAIHandlerNameAndSubscribes(t *testing.T) {
	h := NewAIHandler(AIHandlerConfig{
		Name:     "researcher",
		Persona:  persona.Researcher,
		Backend:  &mockBackend{name: "claude"},
		Store:    newMockStore(),
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	if h.Name() != "researcher" {
		t.Errorf("want name %q, got %q", "researcher", h.Name())
	}

	subs := h.Subscribes()
	if len(subs) != 0 {
		t.Errorf("want empty (no trigger configured), got %v", subs)
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — happy path
// ---------------------------------------------------------------------------

func TestAIHandlerHandle(t *testing.T) {
	store := newMockStore()

	// Seed the store with workflow context.
	corrID := "corr-123"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build a REST API for users",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:     "Here is the implementation...",
			StopReason: "end_turn",
			Duration:   2 * time.Second,
		},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "researcher",
		Persona:  persona.Researcher,
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "researcher",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("want 3 events (request, started, response), got %d", len(results))
	}

	// Verify AIRequestSent
	if results[0].Type != event.AIRequestSent {
		t.Errorf("event[0]: want AIRequestSent, got %s", results[0].Type)
	}
	var reqPayload event.AIRequestPayload
	if err := json.Unmarshal(results[0].Payload, &reqPayload); err != nil {
		t.Fatalf("unmarshal AIRequestPayload: %v", err)
	}
	if reqPayload.Persona != "researcher" {
		t.Errorf("want persona %q, got %q", "researcher", reqPayload.Persona)
	}
	if reqPayload.Backend != "claude" {
		t.Errorf("want backend %q, got %q", "claude", reqPayload.Backend)
	}
	if reqPayload.PromptHash == "" {
		t.Error("prompt hash should not be empty")
	}

	// Verify AIRequestStarted
	if results[1].Type != event.AIRequestStarted {
		t.Errorf("event[1]: want AIRequestStarted, got %s", results[1].Type)
	}
	var startPayload event.AIRequestStartedPayload
	if err := json.Unmarshal(results[1].Payload, &startPayload); err != nil {
		t.Fatalf("unmarshal AIRequestStartedPayload: %v", err)
	}
	if startPayload.SpawnUnixNano == 0 {
		t.Error("SpawnUnixNano should be set")
	}
	if startPayload.PromptHash != reqPayload.PromptHash {
		t.Errorf("prompt hash mismatch: request=%s started=%s", reqPayload.PromptHash, startPayload.PromptHash)
	}

	// Verify AIResponseReceived
	if results[2].Type != event.AIResponseReceived {
		t.Errorf("event[2]: want AIResponseReceived, got %s", results[2].Type)
	}
	var respPayload event.AIResponsePayload
	if err := json.Unmarshal(results[2].Payload, &respPayload); err != nil {
		t.Fatalf("unmarshal AIResponsePayload: %v", err)
	}
	if respPayload.Persona != "researcher" {
		t.Errorf("want persona %q, got %q", "researcher", respPayload.Persona)
	}
	if respPayload.Backend != "claude" {
		t.Errorf("want backend %q, got %q", "claude", respPayload.Backend)
	}
	if respPayload.DurationMS != 2000 {
		t.Errorf("want duration 2000ms, got %d", respPayload.DurationMS)
	}
	if respPayload.Structured {
		t.Error("expected unstructured output")
	}

	// Verify the backend received the system prompt
	if !strings.Contains(mb.lastReq.SystemPrompt, "Rick") {
		t.Error("system prompt should contain Rick persona")
	}
	if !strings.Contains(mb.lastReq.UserPrompt, "REST API for users") {
		t.Error("user prompt should contain the task")
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — structured output
// ---------------------------------------------------------------------------

func TestAIHandlerStructuredOutput(t *testing.T) {
	store := newMockStore()
	corrID := "corr-structured"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Analyze API",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "gemini",
		response: &backend.Response{
			Output:   "Here is the result:\n```json\n{\"key\": \"value\"}\n```",
			Duration: time.Second,
		},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "researcher",
		Persona:  persona.Researcher,
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "researcher",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var respPayload event.AIResponsePayload
	if err := json.Unmarshal(results[2].Payload, &respPayload); err != nil {
		t.Fatalf("unmarshal AIResponsePayload: %v", err)
	}

	if !respPayload.Structured {
		t.Error("expected structured=true for JSON output")
	}
	if !json.Valid(respPayload.Output) {
		t.Errorf("output should be valid JSON, got %s", respPayload.Output)
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — with previous phase outputs
// ---------------------------------------------------------------------------

func TestAIHandlerWithPreviousOutputs(t *testing.T) {
	store := newMockStore()
	corrID := "corr-chain"

	researchOutput, _ := json.Marshal("Research findings: user entity has CRUD operations.")

	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build user API",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Persona: "researcher",
			Backend: "claude",
			Output:  json.RawMessage(researchOutput),
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "Architecture plan...",
			Duration: 3 * time.Second,
		},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "architect",
		Persona:  persona.Architect,
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "architect",
	})).WithCorrelation(corrID)

	_, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify the user prompt includes research output.
	if !strings.Contains(mb.lastReq.UserPrompt, "CRUD operations") {
		t.Error("user prompt should include previous research output")
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — with feedback
// ---------------------------------------------------------------------------

func TestAIHandlerWithFeedback(t *testing.T) {
	store := newMockStore()
	corrID := "corr-feedback"

	archOutput, _ := json.Marshal("Use chi router.")

	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build API",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Persona: "architect",
			Backend: "claude",
			Output:  json.RawMessage(archOutput),
		})).WithCorrelation(corrID),
		// FeedbackGenerated as the aggregate actually emits it: TargetPersona is
		// the handler name ("developer"), not the legacy phase verb. The 2026-05-05
		// regression had the developer matching this against its phase verb and
		// silently dropping every feedback iteration. This test pins the new
		// contract: TargetPersona == h.Name() routes feedback to the developer
		// prompt.
		event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPersona: "developer",
			SourcePersona: "reviewer",
			Iteration:     1,
			Issues: []event.Issue{
				{Severity: "major", Category: "correctness", Description: "Missing error handling"},
			},
			Summary: "Fix error handling in user handler",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "Fixed implementation...",
			Duration: 5 * time.Second,
		},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Template: "develop",
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPersona: "developer",
		Iteration:     1,
	})).WithCorrelation(corrID)

	_, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify the prompt includes feedback.
	if !strings.Contains(mb.lastReq.UserPrompt, "Missing error handling") {
		t.Errorf("user prompt should include feedback issue text, got:\n%s", mb.lastReq.UserPrompt)
	}
	// Belt-and-suspenders: the prompt must also surface the failing summary.
	if !strings.Contains(mb.lastReq.UserPrompt, "Fix error handling") {
		t.Errorf("user prompt should include feedback summary, got:\n%s", mb.lastReq.UserPrompt)
	}
}

// TestAIHandler_FeedbackTargetPersonaMustMatchHandlerName regression-locks the
// feedback-routing contract: the developer's prompt context only picks up a
// FeedbackGenerated payload whose TargetPersona equals the handler's Name. A
// payload addressed to a different persona must not bleed feedback into this
// handler's prompt. Without this lock, a future rename or a stray legacy event
// could re-introduce the silent feedback drop class.
func TestAIHandler_FeedbackTargetPersonaMustMatchHandlerName(t *testing.T) {
	store := newMockStore()
	corrID := "corr-cross-persona"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "noop",
		})).WithCorrelation(corrID),
		// Feedback addressed to a sibling handler — must NOT reach the developer.
		event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPersona: "reviewer",
			SourcePersona: "qa",
			Iteration:     1,
			Issues: []event.Issue{{
				Severity: "major", Category: "correctness",
				Description: "DO NOT LEAK INTO DEVELOPER PROMPT",
			}},
			Summary: "review-targeted feedback",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{name: "claude", response: &backend.Response{Output: "ok", Duration: time.Millisecond}}
	h := NewAIHandler(AIHandlerConfig{
		Name: "developer", Persona: persona.Developer, Template: "develop",
		Backend: mb, Store: store, Personas: persona.DefaultRegistry(), Builder: persona.NewPromptBuilder(),
	})

	env := event.New(event.WorkflowRequested, 1, []byte(`{}`)).WithCorrelation(corrID)
	if _, err := h.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(mb.lastReq.UserPrompt, "DO NOT LEAK") {
		t.Errorf("developer prompt incorrectly absorbed reviewer-targeted feedback:\n%s", mb.lastReq.UserPrompt)
	}
}

// TestAIHandler_OperatorGuidanceTargetMatchesHandlerName mirrors the feedback
// test for the OperatorGuidance branch — both go through the same handler-name
// match in buildPromptContext. Untargeted guidance applies to every persona;
// guidance addressed to a sibling must be filtered out.
func TestAIHandler_OperatorGuidanceTargetMatchesHandlerName(t *testing.T) {
	store := newMockStore()
	corrID := "corr-guidance"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "noop",
		})).WithCorrelation(corrID),
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Target:  "developer",
			Content: "Focus on error handling",
		})).WithCorrelation(corrID),
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "General guidance for everyone",
		})).WithCorrelation(corrID),
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Target:  "reviewer",
			Content: "Ignore this for developer",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{name: "claude", response: &backend.Response{Output: "ok", Duration: time.Millisecond}}
	h := NewAIHandler(AIHandlerConfig{
		Name: "developer", Persona: persona.Developer, Template: "develop",
		Backend: mb, Store: store, Personas: persona.DefaultRegistry(), Builder: persona.NewPromptBuilder(),
	})

	env := event.New(event.WorkflowRequested, 1, []byte(`{}`)).WithCorrelation(corrID)
	if _, err := h.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	prompt := mb.lastReq.UserPrompt
	if !strings.Contains(prompt, "Focus on error handling") {
		t.Errorf("developer prompt missing targeted guidance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "General guidance for everyone") {
		t.Errorf("developer prompt missing untargeted guidance:\n%s", prompt)
	}
	if strings.Contains(prompt, "Ignore this for developer") {
		t.Errorf("developer prompt incorrectly absorbed reviewer-targeted guidance:\n%s", prompt)
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — backend error
// ---------------------------------------------------------------------------

func TestAIHandlerBackendError(t *testing.T) {
	store := newMockStore()
	corrID := "corr-err"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "test",
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:    "researcher",
		Persona: persona.Researcher,
		Backend: &mockBackend{
			name: "claude",
			err:  context.DeadlineExceeded,
		},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "researcher",
	})).WithCorrelation(corrID)

	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("want error from backend failure")
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("error should mention backend, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Event source labeling
// ---------------------------------------------------------------------------

func TestAIHandlerEventSource(t *testing.T) {
	store := newMockStore()
	corrID := "corr-source"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "test",
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:    "developer",
		Persona: persona.Developer,
		Backend: &mockBackend{
			name:     "gemini",
			response: &backend.Response{Output: "code", Duration: time.Second},
		},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, r := range results {
		if r.Source != "handler:developer" {
			t.Errorf("want source %q, got %q", "handler:developer", r.Source)
		}
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — RoundRobin attribution (task 0001)
// ---------------------------------------------------------------------------

// TestAIHandlerRoundRobinAttribution pins the 0001 contract end-to-end: when
// the handler runs over a RoundRobin rotation, every emitted event records the
// concrete inner backend that actually executed, never the composite
// "round-robin(...)" name. Without it, dwell/telemetry (0003) on review-phase
// handlers is unreadable.
func TestAIHandlerRoundRobinAttribution(t *testing.T) {
	store := newMockStore()
	corrID := "corr-rotation"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Review this change",
		})).WithCorrelation(corrID),
	}

	a := &mockBackend{name: "codex", response: &backend.Response{Output: "ok", Duration: time.Second}}
	b := &mockBackend{name: "claude", response: &backend.Response{Output: "ok", Duration: time.Second}}
	rr, err := backend.NewRoundRobin(a, b)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:      "reviewer",
		Persona:   persona.Reviewer,
		Backend:   rr,
		Store:     store,
		Personas:  persona.DefaultRegistry(),
		Builder:   persona.NewPromptBuilder(),
		PlainText: true,
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// No Bus wired → events bundle with the response: [reqEvt, startEvt, respEvt].
	// The concrete backend the sticky key resolved to must be identical across
	// all three, and must be one of the rotation members (never the composite).
	var reqP event.AIRequestPayload
	if err := json.Unmarshal(results[0].Payload, &reqP); err != nil {
		t.Fatalf("unmarshal AIRequestPayload: %v", err)
	}
	var startP event.AIRequestStartedPayload
	if err := json.Unmarshal(results[1].Payload, &startP); err != nil {
		t.Fatalf("unmarshal AIRequestStartedPayload: %v", err)
	}
	var respP event.AIResponsePayload
	if err := json.Unmarshal(results[2].Payload, &respP); err != nil {
		t.Fatalf("unmarshal AIResponsePayload: %v", err)
	}

	for label, got := range map[string]string{
		"AIRequestSent":      reqP.Backend,
		"AIRequestStarted":   startP.Backend,
		"AIResponseReceived": respP.Backend,
	} {
		if strings.HasPrefix(got, "round-robin(") {
			t.Errorf("%s recorded composite name %q; want concrete inner backend", label, got)
		}
		if got != "codex" && got != "claude" {
			t.Errorf("%s backend %q is not a rotation member", label, got)
		}
	}
	if reqP.Backend != respP.Backend || startP.Backend != respP.Backend {
		t.Errorf("attribution diverged across events: sent=%q started=%q resp=%q",
			reqP.Backend, startP.Backend, respP.Backend)
	}

	// The backend that was actually invoked must match the attributed one (the
	// sticky key resolves deterministically, so exactly one member ran).
	ranName := "codex"
	if b.lastReq.UserPrompt != "" {
		ranName = "claude"
	}
	if a.lastReq.UserPrompt != "" && b.lastReq.UserPrompt != "" {
		t.Fatal("both rotation members ran; sticky selection must pick exactly one")
	}
	if respP.Backend != ranName {
		t.Errorf("attributed %q but %q actually ran", respP.Backend, ranName)
	}
}

// ---------------------------------------------------------------------------
// AIHandler.Handle — token count propagation
// ---------------------------------------------------------------------------

func TestAIHandlerTokensUsedPropagated(t *testing.T) {
	// Verify that Response.TokensUsed flows through to AIResponsePayload.TokensUsed.
	store := newMockStore()
	corrID := "corr-tokens"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Write a feature",
		})).WithCorrelation(corrID),
	}

	const wantTokens = 12345
	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:     "Implementation complete.",
			StopReason: "end_turn",
			Duration:   3 * time.Second,
			TokensUsed: wantTokens,
		},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 events, got %d", len(results))
	}

	// AIResponseReceived is the third event (after AIRequestSent, AIRequestStarted).
	if results[2].Type != event.AIResponseReceived {
		t.Fatalf("event[2]: want AIResponseReceived, got %s", results[2].Type)
	}
	var respPayload event.AIResponsePayload
	if err := json.Unmarshal(results[2].Payload, &respPayload); err != nil {
		t.Fatalf("unmarshal AIResponsePayload: %v", err)
	}
	if respPayload.TokensUsed != wantTokens {
		t.Errorf("TokensUsed: want %d, got %d", wantTokens, respPayload.TokensUsed)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestSha256Short(t *testing.T) {
	hash := sha256Short("hello world")
	if len(hash) != 12 {
		t.Errorf("want 12 hex chars, got %d: %s", len(hash), hash)
	}
	// Deterministic.
	if sha256Short("hello world") != hash {
		t.Error("hash should be deterministic")
	}
}

func TestFormatFeedback(t *testing.T) {
	p := event.FeedbackGeneratedPayload{
		Summary: "Fix these issues",
		Issues: []event.Issue{
			{Severity: "critical", Category: "security", Description: "SQL injection", File: "handler.go", Line: 42},
			{Severity: "minor", Category: "style", Description: "Naming convention"},
		},
	}
	result := formatFeedback(p)
	if !strings.Contains(result, "Fix these issues") {
		t.Error("should contain summary")
	}
	if !strings.Contains(result, "[critical/security] SQL injection") {
		t.Error("should contain formatted issue")
	}
	if !strings.Contains(result, "(handler.go:42)") {
		t.Error("should contain file:line reference")
	}
	if !strings.Contains(result, "[minor/style] Naming convention") {
		t.Error("should contain second issue")
	}
}

func TestMarshalOutputUnstructured(t *testing.T) {
	output, structured := marshalOutput("plain text response")
	if structured {
		t.Error("expected unstructured")
	}
	var text string
	if err := json.Unmarshal(output, &text); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if text != "plain text response" {
		t.Errorf("want original text, got %q", text)
	}
}

func TestMarshalOutputStructured(t *testing.T) {
	output, structured := marshalOutput("```json\n{\"key\": \"val\"}\n```")
	if !structured {
		t.Error("expected structured")
	}
	if !json.Valid(output) {
		t.Errorf("output should be valid JSON, got %s", output)
	}
}

func TestUnmarshalOutputText(t *testing.T) {
	raw, _ := json.Marshal("hello world")
	text := unmarshalOutput(raw, false)
	if text != "hello world" {
		t.Errorf("want %q, got %q", "hello world", text)
	}
}

func TestUnmarshalOutputStructured(t *testing.T) {
	raw := json.RawMessage(`{"key":"val"}`)
	text := unmarshalOutput(raw, true)
	if text != `{"key":"val"}` {
		t.Errorf("want raw JSON string, got %q", text)
	}
}

// ---------------------------------------------------------------------------
// buildPromptContext — untested event type branches
// ---------------------------------------------------------------------------

func TestBuildPromptContextWorkspaceReady(t *testing.T) {
	store := newMockStore()
	corrID := "corr-workspace-ready"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "implement feature",
		})).WithCorrelation(corrID),
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path:   "/tmp/myrepo-workspace",
			Branch: "feature/PROJ-42",
			Base:   "main",
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, _, _, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	if pctx.WorkspacePath != "/tmp/myrepo-workspace" {
		t.Errorf("want WorkspacePath '/tmp/myrepo-workspace', got %q", pctx.WorkspacePath)
	}
	if pctx.Ticket != "feature/PROJ-42" {
		t.Errorf("want Ticket 'feature/PROJ-42', got %q", pctx.Ticket)
	}
	if pctx.BaseBranch != "main" {
		t.Errorf("want BaseBranch 'main', got %q", pctx.BaseBranch)
	}
}

func TestBuildPromptContextCodebase(t *testing.T) {
	store := newMockStore()
	corrID := "corr-codebase"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "analyze codebase",
		})).WithCorrelation(corrID),
		event.New(event.ContextCodebase, 1, event.MustMarshal(event.ContextCodebasePayload{
			Language:  "go",
			Framework: "go-grpc",
			Tree: []event.FileEntry{
				{Path: "main.go", Size: 512},
			},
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, _, _, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	if pctx.Codebase == "" {
		t.Error("Codebase should be non-empty when ContextCodebase event is present")
	}
	if !strings.Contains(pctx.Codebase, "go") {
		t.Errorf("Codebase should mention language 'go', got: %q", pctx.Codebase)
	}
}

func TestBuildPromptContextSchema(t *testing.T) {
	store := newMockStore()
	corrID := "corr-schema"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "implement proto",
		})).WithCorrelation(corrID),
		event.New(event.ContextSchema, 1, event.MustMarshal(event.ContextSchemaPayload{
			Proto: []event.FileSnap{
				{Path: "api/service.proto", Content: "syntax = \"proto3\";"},
			},
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, _, _, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	if pctx.Schema == "" {
		t.Error("Schema should be non-empty when ContextSchema event is present")
	}
	if !strings.Contains(pctx.Schema, "proto3") {
		t.Errorf("Schema should contain proto content, got: %q", pctx.Schema)
	}
}

func TestBuildPromptContextGit(t *testing.T) {
	store := newMockStore()
	corrID := "corr-git"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "fix bug",
		})).WithCorrelation(corrID),
		event.New(event.ContextGit, 1, event.MustMarshal(event.ContextGitPayload{
			HEAD:      "abc1234",
			Branch:    "feature/my-branch",
			RecentLog: []string{"abc1234 fix: resolve nil pointer"},
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, _, _, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	if pctx.GitContext == "" {
		t.Error("GitContext should be non-empty when ContextGit event is present")
	}
	if !strings.Contains(pctx.GitContext, "abc1234") {
		t.Errorf("GitContext should contain HEAD, got: %q", pctx.GitContext)
	}
	if !strings.Contains(pctx.GitContext, "feature/my-branch") {
		t.Errorf("GitContext should contain branch name, got: %q", pctx.GitContext)
	}
}

func TestBuildPromptContextEnrichment(t *testing.T) {
	store := newMockStore()
	corrID := "corr-enrichment"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "build component",
		})).WithCorrelation(corrID),
		event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
			Source:  "frontend-enricher",
			Kind:    "libraries",
			Summary: "Use shadcn/ui for components",
			Items: []event.EnrichmentItem{
				{Name: "shadcn/ui", Reason: "recommended component library"},
			},
		})).WithCorrelation(corrID),
		event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
			Source:  "jira-context",
			Kind:    "ticket",
			Summary: "PROJ-99 ticket context",
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, _, _, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	if len(pctx.Enrichments) != 2 {
		t.Errorf("want 2 enrichments, got %d", len(pctx.Enrichments))
	}
	if pctx.Enrichments[0].Source != "frontend-enricher" {
		t.Errorf("enrichment[0].Source: want 'frontend-enricher', got %q", pctx.Enrichments[0].Source)
	}
}

func TestBuildPromptContextOperatorGuidanceMatching(t *testing.T) {
	store := newMockStore()
	corrID := "corr-guidance"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "build feature",
		})).WithCorrelation(corrID),
		// Guidance targeted at "developer" — should be included.
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "Focus on error handling",
			Target:  "developer",
		})).WithCorrelation(corrID),
		// Guidance targeted at "reviewer" — should NOT be included for developer.
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "Ignore this for developer",
			Target:  "reviewer",
		})).WithCorrelation(corrID),
		// Untargeted guidance — should be included for all.
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "General guidance for everyone",
			Target:  "",
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, _, _, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	// Should have "Focus on error handling" and "General guidance for everyone".
	if !strings.Contains(pctx.Feedback, "Focus on error handling") {
		t.Errorf("Feedback should contain targeted guidance, got: %q", pctx.Feedback)
	}
	if !strings.Contains(pctx.Feedback, "General guidance for everyone") {
		t.Errorf("Feedback should contain untargeted guidance, got: %q", pctx.Feedback)
	}
	// Should NOT have the review-targeted guidance.
	if strings.Contains(pctx.Feedback, "Ignore this for developer") {
		t.Errorf("Feedback should NOT contain review-targeted guidance, got: %q", pctx.Feedback)
	}
}

// TestBuildPromptContextAutoRetryAttempt verifies that buildPromptContext
// counts automatic WorkflowRetried events targeting this handler so the
// caller can fold it into the backend sticky key and flip RoundRobin
// selection on retry. Operator-initiated retries (Automatic=false) and
// retries targeting other handlers are both ignored.
func TestBuildPromptContextAutoRetryAttempt(t *testing.T) {
	store := newMockStore()
	corrID := "corr-auto-retry"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "implement",
		})).WithCorrelation(corrID),
		// First silent stall → engine emits automatic retry for developer.
		event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "developer",
			Automatic: true,
		})).WithCorrelation(corrID),
		// Operator-initiated retry for reviewer — must NOT count.
		event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "reviewer",
			Automatic: false,
		})).WithCorrelation(corrID),
		// Automatic retry for reviewer — different persona, must NOT count.
		event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "reviewer",
			Automatic: true,
		})).WithCorrelation(corrID),
		// Second automatic retry for developer — counted.
		event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "developer",
			Automatic: true,
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "ok"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	_, attempt, _, err := h.buildPromptContext(context.Background(),
		event.New(event.WorkflowRetried, 1, nil).WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("buildPromptContext: %v", err)
	}
	if attempt != 2 {
		t.Errorf("autoRetryAttempt = %d; want 2 (only Automatic=true + FromPhase=developer count)", attempt)
	}
}

// TestAIHandler_AutoRetryFlipsBackendStickyOffset is the end-to-end
// regression guard for the 2026-04-20 docs-only silent-stall auto-retry
// rotation fix. The attempt-counter alone is not load-bearing — what
// matters is the backend context the handler plants before Run. A
// future change that removes WithRotateOffset (or folds the counter
// into the key via FNV re-hash, which can collide with 1/n probability)
// would regress into the original bug without failing any of the
// pure-counter tests. This test proves the key is stable AND the offset
// tracks the retry attempt.
func TestAIHandler_AutoRetryFlipsBackendStickyOffset(t *testing.T) {
	store := newMockStore()
	corrID := "corr-flip"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "ship",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{name: "claude", response: &backend.Response{Output: "ok"}}
	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	// Attempt 0: no WorkflowRetried yet.
	_, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).
		WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	wantKey := corrID + ":" + persona.Developer
	if mb.lastStickyKey != wantKey {
		t.Errorf("first call sticky key = %q; want %q (must pin per-persona without attempt suffix)",
			mb.lastStickyKey, wantKey)
	}
	if mb.lastRotateOffset != 0 {
		t.Errorf("first call rotate offset = %d; want 0 (no auto-retry recorded)", mb.lastRotateOffset)
	}

	// Record an automatic retry for the developer persona; attempt 1 should
	// now apply offset=1 while keeping the same sticky key.
	store.correlationEvents[corrID] = append(store.correlationEvents[corrID],
		event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "developer",
			Automatic: true,
		})).WithCorrelation(corrID),
	)

	_, err = h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).
		WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("retry Handle: %v", err)
	}
	if mb.lastStickyKey != wantKey {
		t.Errorf("retry sticky key = %q; want %q (key must be stable — rotation is via offset, not key suffix)",
			mb.lastStickyKey, wantKey)
	}
	if mb.lastRotateOffset != 1 {
		t.Errorf("retry rotate offset = %d; want 1 (auto-retry attempt must flip RoundRobin slot deterministically)",
			mb.lastRotateOffset)
	}

	// Operator-initiated retry must NOT bump the offset.
	store.correlationEvents[corrID] = append(store.correlationEvents[corrID],
		event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "developer",
			Automatic: false, // operator-initiated
		})).WithCorrelation(corrID),
	)
	_, err = h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).
		WithCorrelation(corrID))
	if err != nil {
		t.Fatalf("operator-retry Handle: %v", err)
	}
	if mb.lastRotateOffset != 1 {
		t.Errorf("operator retry rotate offset = %d; want 1 (only Automatic=true retries bump the offset)",
			mb.lastRotateOffset)
	}
}

func TestAIHandlerWorkspacePathOverridesWorkDir(t *testing.T) {
	// When WorkspacePath is set in pctx, it should override the static workDir.
	store := newMockStore()
	corrID := "corr-workdir-override"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "implement",
		})).WithCorrelation(corrID),
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path:   "/dynamic/workspace/path",
			Branch: "my-branch",
			Base:   "main",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name:     "claude",
		response: &backend.Response{Output: "done"},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  mb,
		Store:    store,
		WorkDir:  "/static/work/dir", // static workDir
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)
	_, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Backend should have been called with the dynamic workspace path, not the static one.
	if mb.lastReq.WorkDir != "/dynamic/workspace/path" {
		t.Errorf("want WorkDir '/dynamic/workspace/path', got %q", mb.lastReq.WorkDir)
	}
}

func TestAIHandlerPlainTextOutput(t *testing.T) {
	// PlainText=true: raw text should be marshaled as a JSON string, not extracted as JSON.
	store := newMockStore()
	corrID := "corr-plaintext"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "analyze",
		})).WithCorrelation(corrID),
	}

	rawOutput := "These are QA steps:\n1. Step one\n2. Step two"
	mb := &mockBackend{
		name:     "claude",
		response: &backend.Response{Output: rawOutput},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:      "qa-analyzer",
		Persona:   persona.Developer,
		Backend:   mb,
		Store:     store,
		PlainText: true,
		Personas:  persona.DefaultRegistry(),
		Builder:   persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)
	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var respPayload event.AIResponsePayload
	if err := json.Unmarshal(results[2].Payload, &respPayload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// PlainText mode: structured must be false (raw text stored as JSON string).
	if respPayload.Structured {
		t.Error("PlainText=true should produce structured=false")
	}

	// The output should unmarshal back to the original text.
	var decoded string
	if err := json.Unmarshal(respPayload.Output, &decoded); err != nil {
		t.Fatalf("output should be a JSON string, got: %s, error: %v", respPayload.Output, err)
	}
	if decoded != rawOutput {
		t.Errorf("want decoded output %q, got %q", rawOutput, decoded)
	}
}

// ---------------------------------------------------------------------------
// Bug 1 regression: AIRequestSent must be persisted+published BEFORE
// backend.Run so a hung backend still leaves a forensic trail.
//
// Incident: 2d8b4b99-f8e8-4af4-917c-9102fa6ca33a — the developer claude
// subprocess hung for 17 minutes; because Handle returned both events at
// the end, the events table looked like the handler was never dispatched.
// ---------------------------------------------------------------------------

// recordingBus captures every Publish call so tests can assert ordering of
// AIRequestSent vs the backend invocation.
type recordingBus struct {
	mu        sync.Mutex
	published []event.Envelope
}

func (b *recordingBus) Publish(_ context.Context, env event.Envelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, env)
	return nil
}

func (b *recordingBus) Subscribe(_ event.Type, _ eventbus.HandlerFunc, _ ...eventbus.SubscribeOption) func() {
	return func() {}
}

func (b *recordingBus) SubscribeAll(_ eventbus.HandlerFunc, _ ...eventbus.SubscribeOption) func() {
	return func() {}
}

func (b *recordingBus) Close() error { return nil }

func (b *recordingBus) snapshot() []event.Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]event.Envelope, len(b.published))
	copy(out, b.published)
	return out
}

// hangingBackend blocks Run until ctx is cancelled, simulating the wedged
// claude subprocess from the incident.
type hangingBackend struct {
	name      string
	gotPrompt chan struct{} // closed once Run is reached
}

func (b *hangingBackend) Name() string                       { return b.name }
func (b *hangingBackend) Capabilities() backend.Capabilities { return backend.Capabilities{} }

func (b *hangingBackend) Run(ctx context.Context, _ backend.Request) (*backend.Response, error) {
	close(b.gotPrompt)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAIHandlerEmitsRequestBeforeBackend(t *testing.T) {
	// When Bus is wired, AIRequestSent must hit the bus + store BEFORE
	// backend.Run is even entered. This is the load-bearing fix for the
	// 2d8b4b99 observability gap.
	store := newMockStore()
	corrID := "corr-trace"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "build something",
		})).WithCorrelation(corrID),
	}

	bus := &recordingBus{}
	be := &hangingBackend{name: "claude", gotPrompt: make(chan struct{})}

	h := NewAIHandler(AIHandlerConfig{
		Name:           "developer",
		Persona:        persona.Developer,
		Backend:        be,
		Store:          store,
		Bus:            bus,
		Personas:       persona.DefaultRegistry(),
		Builder:        persona.NewPromptBuilder(),
		BackendTimeout: 100 * time.Millisecond, // force a quick fail so the test exits
	})

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)

	// Run Handle in a goroutine because the hanging backend will block
	// until the timeout fires.
	done := make(chan error, 1)
	go func() {
		_, err := h.Handle(context.Background(), env)
		done <- err
	}()

	// Wait for Run to be entered. By this point AIRequestSent must
	// already be on the bus and in the store — that's the whole fix.
	select {
	case <-be.gotPrompt:
	case <-time.After(2 * time.Second):
		t.Fatal("backend.Run never reached")
	}

	// Snapshot the bus + store BEFORE the backend timeout fires.
	// Both AIRequestSent AND AIRequestStarted must be published before Run —
	// AIRequestSent for the pre-flight trail, AIRequestStarted for the exact
	// subprocess-spawn moment. Operators distinguish handler-pre-flight
	// stalls from subprocess-side hangs via this pair.
	pubs := bus.snapshot()
	if len(pubs) != 2 {
		t.Fatalf("want 2 published events before backend.Run (sent+started), got %d", len(pubs))
	}
	if pubs[0].Type != event.AIRequestSent {
		t.Errorf("want AIRequestSent published first, got %s", pubs[0].Type)
	}
	if pubs[1].Type != event.AIRequestStarted {
		t.Errorf("want AIRequestStarted published second, got %s", pubs[1].Type)
	}

	aggregateID := corrID + ":persona:developer"
	stored := store.aggregateEvents[aggregateID]
	if len(stored) != 2 {
		t.Fatalf("want 2 events persisted to %s (sent+started), got %d", aggregateID, len(stored))
	}
	if stored[0].Type != event.AIRequestSent {
		t.Errorf("want AIRequestSent persisted first, got %s", stored[0].Type)
	}
	if stored[1].Type != event.AIRequestStarted {
		t.Errorf("want AIRequestStarted persisted second, got %s", stored[1].Type)
	}
	if stored[0].Version != 1 {
		t.Errorf("want AIRequestSent version 1, got %d", stored[0].Version)
	}
	if stored[1].Version != 2 {
		t.Errorf("want AIRequestStarted version 2, got %d", stored[1].Version)
	}
	if stored[0].CorrelationID != corrID {
		t.Errorf("want correlation %q, got %q", corrID, stored[0].CorrelationID)
	}

	// Now wait for Handle to return — it should fail with the wrapped
	// backend timeout error.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected backend timeout error, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
			t.Errorf("want deadline exceeded error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return after backend timeout")
	}
}

// TestAIRequestStartedEmittedEvenOnBackendError is the core observability
// regression: when the backend returns an error (idle_timeout, wall_timeout,
// or handler error), AIRequestStarted must still be persisted + published so
// operators can distinguish "handler never reached backend.Run" from
// "backend was invoked and then hung". Without this pair, a 6-minute
// subprocess stall looks identical to a pre-flight stall in the event log.
func TestAIRequestStartedEmittedEvenOnBackendError(t *testing.T) {
	store := newMockStore()
	corrID := "corr-err-started"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "trigger backend error",
		})).WithCorrelation(corrID),
	}

	bus := &recordingBus{}
	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", err: errors.New("simulated idle_timeout")},
		Store:    store,
		Bus:      bus,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)
	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("want backend error, got nil")
	}

	// Both events must have reached the bus + store despite the backend failure.
	pubs := bus.snapshot()
	if len(pubs) != 2 {
		t.Fatalf("want 2 published events (sent+started), got %d", len(pubs))
	}
	if pubs[0].Type != event.AIRequestSent || pubs[1].Type != event.AIRequestStarted {
		t.Errorf("want [AIRequestSent, AIRequestStarted], got [%s, %s]", pubs[0].Type, pubs[1].Type)
	}

	aggregateID := corrID + ":persona:developer"
	stored := store.aggregateEvents[aggregateID]
	if len(stored) != 2 {
		t.Fatalf("want 2 stored events (sent+started), got %d", len(stored))
	}
	if stored[0].Type != event.AIRequestSent || stored[1].Type != event.AIRequestStarted {
		t.Errorf("want [AIRequestSent, AIRequestStarted] stored, got [%s, %s]", stored[0].Type, stored[1].Type)
	}

	// SpawnUnixNano must be non-zero — it's the diagnostic timestamp.
	var sp event.AIRequestStartedPayload
	if err := json.Unmarshal(stored[1].Payload, &sp); err != nil {
		t.Fatalf("unmarshal AIRequestStartedPayload: %v", err)
	}
	if sp.SpawnUnixNano == 0 {
		t.Error("SpawnUnixNano must be set on AIRequestStarted")
	}
}

// TestPlainTextPreservesVerdictLineWhenJSONFragmentPresent is the regression
// guard for the PR #845 bug: pr-observability and pr-vendor-resilience cited
// a JSON snippet inline (e.g. ["metric.label.target"]), ExtractJSON captured
// only that fragment and discarded the VERDICT line, causing ParseVerdict to
// default to VerdictSourceDefaultOptimistic.  With PlainText=true the full
// prose — including VERDICT: FAIL — must survive unmodified.
func TestPlainTextPreservesVerdictLineWhenJSONFragmentPresent(t *testing.T) {
	const rawLLMOutput = `["metric.label.target"]

The dashboard filter only checks one label and misses target groups beyond what's listed.

VERDICT: FAIL
1. ` + "`major`" + ` ` + "`dashboards.go:42`" + ` ` + "`target_label`" + ` — Filter is too narrow.`

	store := newMockStore()
	corrID := "corr-plaintext-verdict"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "review PR",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name:     "gemini",
		response: &backend.Response{Output: rawLLMOutput, Duration: time.Second},
	}

	// PlainText=true path: full output must be preserved.
	h := NewAIHandler(AIHandlerConfig{
		Name:      "pr-observability",
		Persona:   persona.PRObservability,
		Backend:   mb,
		Store:     store,
		PlainText: true,
		Personas:  persona.DefaultRegistry(),
		Builder:   persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-workspace",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Locate AIResponseReceived — it's the last event returned.
	var respPayload event.AIResponsePayload
	found := false
	for _, e := range results {
		if e.Type == event.AIResponseReceived {
			if err := json.Unmarshal(e.Payload, &respPayload); err != nil {
				t.Fatalf("unmarshal AIResponsePayload: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("AIResponseReceived not found in results: %v", typesOf(results))
	}

	// Structured must be false — PlainText never sets the structured flag.
	if respPayload.Structured {
		t.Error("PlainText=true must produce Structured=false")
	}

	// The decoded output must be the verbatim LLM text, not just the JSON fragment.
	var decoded string
	if err := json.Unmarshal(respPayload.Output, &decoded); err != nil {
		t.Fatalf("output must be a JSON string, got %s: %v", respPayload.Output, err)
	}
	if decoded != rawLLMOutput {
		t.Errorf("PlainText=true: output mismatch\ngot:  %q\nwant: %q", decoded, rawLLMOutput)
	}
	if !strings.Contains(decoded, "VERDICT: FAIL") {
		t.Error("PlainText=true: VERDICT line must survive — ExtractJSON must not have run")
	}

	// Prove the bug: PlainText=false (default) truncates the output to the JSON
	// fragment, discarding the VERDICT line.
	hDefault := NewAIHandler(AIHandlerConfig{
		Name:      "pr-observability",
		Persona:   persona.PRObservability,
		Backend:   mb,
		Store:     store,
		PlainText: false, // default — triggers ExtractJSON
		Personas:  persona.DefaultRegistry(),
		Builder:   persona.NewPromptBuilder(),
	})

	resultsDefault, err := hDefault.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle (default): %v", err)
	}

	var defaultPayload event.AIResponsePayload
	for _, e := range resultsDefault {
		if e.Type == event.AIResponseReceived {
			if err := json.Unmarshal(e.Payload, &defaultPayload); err != nil {
				t.Fatalf("unmarshal default AIResponsePayload: %v", err)
			}
		}
	}

	// With PlainText=false, ExtractJSON grabs the first JSON literal and the
	// VERDICT line is lost.  The decoded output must NOT contain "VERDICT: FAIL".
	defaultDecoded := unmarshalOutput(defaultPayload.Output, defaultPayload.Structured)
	if strings.Contains(defaultDecoded, "VERDICT: FAIL") {
		t.Logf("NOTE: ExtractJSON behaviour may have changed — the bug may already be fixed upstream")
	} else {
		// Confirm the truncation: only the JSON fragment survives.
		if !strings.Contains(defaultDecoded, `"metric.label.target"`) {
			t.Errorf("PlainText=false: expected JSON fragment in output, got %q", defaultDecoded)
		}
	}
}

func TestAIHandlerOmitsRequestEventWhenBusWired(t *testing.T) {
	// Counterpart to the legacy [reqEvt, respEvt] test: with Bus wired,
	// Handle returns ONLY AIResponseReceived because the request event
	// has already been emitted inline.
	store := newMockStore()
	corrID := "corr-bus-wired"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "x",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name:     "claude",
		response: &backend.Response{Output: "done", Duration: time.Second},
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "researcher",
		Persona:  persona.Researcher,
		Backend:  mb,
		Store:    store,
		Bus:      &recordingBus{},
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)
	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 returned event (response only), got %d", len(results))
	}
	if results[0].Type != event.AIResponseReceived {
		t.Errorf("want AIResponseReceived, got %s", results[0].Type)
	}
}

// ---------------------------------------------------------------------------
// Session resume (developer feedback loop)
// ---------------------------------------------------------------------------

// seedResumeCorrelation builds a correlation chain for a developer that has
// already run once (producing session "S1" on the given backend) and then
// received review feedback targeting it. extra events are appended (e.g. an
// auto-retry marker) to exercise the eligibility gate.
func seedResumeCorrelation(priorBackend string, extra ...event.Envelope) (*mockStore, string) {
	store := newMockStore()
	corrID := "corr-resume"
	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build a REST API for users",
		})).WithCorrelation(corrID),
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: "/tmp/ws", Branch: "PROJ-1", Base: "main",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Persona:   "developer",
			Backend:   priorBackend,
			SessionID: "S1",
			Output:    event.MustMarshal("first attempt"),
		})).WithCorrelation(corrID),
		event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPersona: "developer",
			SourcePersona: "reviewer",
			Summary:       "fix the null deref on line 42",
			Iteration:     1,
		})).WithCorrelation(corrID),
	}
	events = append(events, extra...)
	store.correlationEvents[corrID] = events
	return store, corrID
}

func newDeveloperResumeHandler(store *mockStore, mb *mockBackend, enable bool) *AIHandler {
	return NewAIHandler(AIHandlerConfig{
		Name:                 "developer",
		Persona:              persona.Developer,
		Backend:              mb,
		Store:                store,
		Personas:             persona.DefaultRegistry(),
		Builder:              persona.NewPromptBuilder(),
		ResumeInFeedbackLoop: enable,
	})
}

func TestAIHandlerSessionResume(t *testing.T) {
	devTrigger := func(corrID string) event.Envelope {
		// reviewer's failing verdict drives the developer re-run.
		return event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPersona: "developer",
		})).WithCorrelation(corrID)
	}

	t.Run("resumes_with_minimal_prompt", func(t *testing.T) {
		store, corrID := seedResumeCorrelation("claude")
		mb := &mockBackend{name: "claude", response: &backend.Response{Output: "fixed", SessionID: "S1"}}
		h := newDeveloperResumeHandler(store, mb, true)

		if _, err := h.Handle(context.Background(), devTrigger(corrID)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if mb.lastReq.SessionID != "S1" {
			t.Errorf("want resume of session S1, got %q", mb.lastReq.SessionID)
		}
		// Minimal prompt: carries the feedback, not the full context template.
		if !strings.Contains(mb.lastReq.UserPrompt, "Continue from your previous session") {
			t.Errorf("want resume prompt, got %q", mb.lastReq.UserPrompt)
		}
		if !strings.Contains(mb.lastReq.UserPrompt, "null deref on line 42") {
			t.Errorf("resume prompt missing feedback: %q", mb.lastReq.UserPrompt)
		}
	})

	t.Run("no_resume_when_flag_off", func(t *testing.T) {
		store, corrID := seedResumeCorrelation("claude")
		mb := &mockBackend{name: "claude", response: &backend.Response{Output: "fixed"}}
		h := newDeveloperResumeHandler(store, mb, false)

		if _, err := h.Handle(context.Background(), devTrigger(corrID)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if mb.lastReq.SessionID != "" {
			t.Errorf("want no resume (flag off), got %q", mb.lastReq.SessionID)
		}
	})

	t.Run("no_resume_on_backend_mismatch", func(t *testing.T) {
		// Prior session was opened on codex; the developer backend is claude.
		store, corrID := seedResumeCorrelation("codex")
		mb := &mockBackend{name: "claude", response: &backend.Response{Output: "fixed"}}
		h := newDeveloperResumeHandler(store, mb, true)

		if _, err := h.Handle(context.Background(), devTrigger(corrID)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if mb.lastReq.SessionID != "" {
			t.Errorf("want no cross-backend resume, got %q", mb.lastReq.SessionID)
		}
	})

	t.Run("no_resume_on_auto_retry_rotation", func(t *testing.T) {
		// An automatic retry rotates the backend, so the recorded session id no
		// longer matches the CLI that will run — resume must be disabled.
		retry := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
			FromPhase: "developer",
			Automatic: true,
		})).WithCorrelation("corr-resume")
		store, corrID := seedResumeCorrelation("claude", retry)
		mb := &mockBackend{name: "claude", response: &backend.Response{Output: "fixed"}}
		h := newDeveloperResumeHandler(store, mb, true)

		if _, err := h.Handle(context.Background(), devTrigger(corrID)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if mb.lastReq.SessionID != "" {
			t.Errorf("want no resume under rotation, got %q", mb.lastReq.SessionID)
		}
	})

	t.Run("response_carries_session_id", func(t *testing.T) {
		store := newMockStore()
		corrID := "corr-fresh"
		store.correlationEvents[corrID] = []event.Envelope{
			event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
				Prompt: "Build it",
			})).WithCorrelation(corrID),
			event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
				Path: "/tmp/ws", Branch: "PROJ-1", Base: "main",
			})).WithCorrelation(corrID),
		}
		mb := &mockBackend{name: "claude", response: &backend.Response{Output: "done", SessionID: "S-new"}}
		h := newDeveloperResumeHandler(store, mb, true)

		results, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1,
			event.MustMarshal(event.PersonaCompletedPayload{Persona: "context-snapshot"})).WithCorrelation(corrID))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		// First run (no feedback) must not resume...
		if mb.lastReq.SessionID != "" {
			t.Errorf("first run should not resume, got %q", mb.lastReq.SessionID)
		}
		// ...but it must record the new session id for a later resume.
		var resp event.AIResponsePayload
		if err := json.Unmarshal(results[len(results)-1].Payload, &resp); err != nil {
			t.Fatalf("unmarshal AIResponsePayload: %v", err)
		}
		if resp.SessionID != "S-new" {
			t.Errorf("want recorded session id %q, got %q", "S-new", resp.SessionID)
		}
	})
}
