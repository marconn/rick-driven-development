package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
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
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) Run(_ context.Context, req backend.Request) (*backend.Response, error) {
	m.lastReq = req
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

// ---------------------------------------------------------------------------
// Mock store (minimal: only LoadByCorrelation is needed by AIHandler)
// ---------------------------------------------------------------------------

type mockStore struct {
	correlationEvents map[string][]event.Envelope
}

func newMockStore() *mockStore {
	return &mockStore{correlationEvents: make(map[string][]event.Envelope)}
}

func (s *mockStore) LoadByCorrelation(_ context.Context, correlationID string) ([]event.Envelope, error) {
	return s.correlationEvents[correlationID], nil
}

// Unused Store interface methods — stub implementations.
func (s *mockStore) Append(context.Context, string, int, []event.Envelope) error              { return nil }
func (s *mockStore) Load(context.Context, string) ([]event.Envelope, error)                   { return nil, nil }
func (s *mockStore) LoadFrom(context.Context, string, int) ([]event.Envelope, error)          { return nil, nil }
func (s *mockStore) LoadAll(context.Context, int64, int) ([]eventstore.PositionedEvent, error) { return nil, nil }
func (s *mockStore) LoadEvent(context.Context, string) (*event.Envelope, error)               { return nil, nil }
func (s *mockStore) SaveSnapshot(context.Context, eventstore.Snapshot) error                  { return nil }
func (s *mockStore) LoadSnapshot(context.Context, string) (*eventstore.Snapshot, error)       { return nil, nil }
func (s *mockStore) RecordDeadLetter(context.Context, eventstore.DeadLetter) error            { return nil }
func (s *mockStore) LoadDeadLetters(context.Context) ([]eventstore.DeadLetter, error)         { return nil, nil }
func (s *mockStore) DeleteDeadLetter(context.Context, string) error                           { return nil }
func (s *mockStore) SaveTags(context.Context, string, map[string]string) error                { return nil }
func (s *mockStore) LoadByTag(context.Context, string, string) ([]string, error)              { return nil, nil }
func (s *mockStore) Close() error                                                             { return nil }

// ---------------------------------------------------------------------------
// AIHandler construction
// ---------------------------------------------------------------------------

func TestAIHandlerNameAndSubscribes(t *testing.T) {
	h := NewAIHandler(AIHandlerConfig{
		Name:     "researcher",
		Phase:    "research",
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
		Phase:    "research",
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

	if len(results) != 2 {
		t.Fatalf("want 2 events, got %d", len(results))
	}

	// Verify AIRequestSent
	if results[0].Type != event.AIRequestSent {
		t.Errorf("event[0]: want AIRequestSent, got %s", results[0].Type)
	}
	var reqPayload event.AIRequestPayload
	if err := json.Unmarshal(results[0].Payload, &reqPayload); err != nil {
		t.Fatalf("unmarshal AIRequestPayload: %v", err)
	}
	if reqPayload.Phase != "research" {
		t.Errorf("want phase %q, got %q", "research", reqPayload.Phase)
	}
	if reqPayload.Backend != "claude" {
		t.Errorf("want backend %q, got %q", "claude", reqPayload.Backend)
	}
	if reqPayload.Persona != persona.Researcher {
		t.Errorf("want persona %q, got %q", persona.Researcher, reqPayload.Persona)
	}
	if reqPayload.PromptHash == "" {
		t.Error("prompt hash should not be empty")
	}

	// Verify AIResponseReceived
	if results[1].Type != event.AIResponseReceived {
		t.Errorf("event[1]: want AIResponseReceived, got %s", results[1].Type)
	}
	var respPayload event.AIResponsePayload
	if err := json.Unmarshal(results[1].Payload, &respPayload); err != nil {
		t.Fatalf("unmarshal AIResponsePayload: %v", err)
	}
	if respPayload.Phase != "research" {
		t.Errorf("want phase %q, got %q", "research", respPayload.Phase)
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
		Phase:    "research",
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
	if err := json.Unmarshal(results[1].Payload, &respPayload); err != nil {
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
			Phase:   "research",
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
		Phase:    "architect",
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
			Phase:   "architect",
			Backend: "claude",
			Output:  json.RawMessage(archOutput),
		})).WithCorrelation(corrID),
		event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPhase: "develop",
			Iteration:   1,
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
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  mb,
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	env := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPhase: "develop",
		Iteration:   1,
	})).WithCorrelation(corrID)

	_, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify the prompt includes feedback.
	if !strings.Contains(mb.lastReq.UserPrompt, "Missing error handling") {
		t.Error("user prompt should include feedback")
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
		Phase:   "research",
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
		Phase:   "develop",
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
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
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
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
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
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
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
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
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
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
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
		// Guidance targeted at "develop" — should be included.
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "Focus on error handling",
			Target:  "develop",
		})).WithCorrelation(corrID),
		// Guidance targeted at "review" — should NOT be included for developer.
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "Ignore this for developer",
			Target:  "review",
		})).WithCorrelation(corrID),
		// Untargeted guidance — should be included for all.
		event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
			Content: "General guidance for everyone",
			Target:  "",
		})).WithCorrelation(corrID),
	}

	h := NewAIHandler(AIHandlerConfig{
		Name:     "developer",
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "done"}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	pctx, err := h.buildPromptContext(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID))
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
		Name:    "developer",
		Phase:   "develop",
		Persona: persona.Developer,
		Backend: mb,
		Store:   store,
		WorkDir: "/static/work/dir", // static workDir
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
		Phase:     "qa-analyze",
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
	if err := json.Unmarshal(results[1].Payload, &respPayload); err != nil {
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
