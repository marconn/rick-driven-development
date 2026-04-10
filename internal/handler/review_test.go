package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// ---------------------------------------------------------------------------
// ReviewHandler — pass verdict
// ---------------------------------------------------------------------------

func TestReviewHandlerPassVerdict(t *testing.T) {
	store := newMockStore()
	corrID := "corr-review-pass"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build REST API",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:  "develop",
			Output: marshalText("implementation code..."),
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "Code looks good. Well-structured error handling.\n\nVERDICT: PASS",
			Duration: 3 * time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "reviewer",
			Phase:    "review",
			Persona:  persona.Reviewer,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Expect: AIRequestSent, AIResponseReceived, VerdictRendered
	if len(results) != 3 {
		t.Fatalf("want 3 events, got %d", len(results))
	}
	if results[0].Type != event.AIRequestSent {
		t.Errorf("event[0]: want AIRequestSent, got %s", results[0].Type)
	}
	if results[1].Type != event.AIResponseReceived {
		t.Errorf("event[1]: want AIResponseReceived, got %s", results[1].Type)
	}
	if results[2].Type != event.VerdictRendered {
		t.Errorf("event[2]: want VerdictRendered, got %s", results[2].Type)
	}

	var verdict event.VerdictPayload
	if err := json.Unmarshal(results[2].Payload, &verdict); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if verdict.Outcome != event.VerdictPass {
		t.Errorf("want pass, got %s", verdict.Outcome)
	}
	if verdict.Phase != "develop" {
		t.Errorf("want target phase 'develop', got %q", verdict.Phase)
	}
	if verdict.SourcePhase != "review" {
		t.Errorf("want source phase 'review', got %q", verdict.SourcePhase)
	}
}

// ---------------------------------------------------------------------------
// ReviewHandler — fail verdict
// ---------------------------------------------------------------------------

func TestReviewHandlerFailVerdict(t *testing.T) {
	store := newMockStore()
	corrID := "corr-review-fail"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build API",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output: `The implementation has several issues.

Missing error handling in critical paths.

VERDICT: FAIL

1. Missing error handling in handler.go:42
2. SQL injection risk in db.go:15
3. No input validation for user endpoints`,
			Duration: 2 * time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "reviewer",
			Phase:    "review",
			Persona:  persona.Reviewer,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
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

	var verdict event.VerdictPayload
	if err := json.Unmarshal(results[2].Payload, &verdict); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if verdict.Outcome != event.VerdictFail {
		t.Errorf("want fail, got %s", verdict.Outcome)
	}
	if len(verdict.Issues) != 3 {
		t.Fatalf("want 3 issues, got %d", len(verdict.Issues))
	}

	// Verify first issue has file:line
	if verdict.Issues[0].File != "handler.go" {
		t.Errorf("issue[0] file: want handler.go, got %q", verdict.Issues[0].File)
	}
	if verdict.Issues[0].Line != 42 {
		t.Errorf("issue[0] line: want 42, got %d", verdict.Issues[0].Line)
	}

	// Verify severity classification
	if verdict.Issues[1].Severity != "critical" {
		t.Errorf("issue[1] (SQL injection): want critical severity, got %q", verdict.Issues[1].Severity)
	}
	if verdict.Issues[1].Category != "security" {
		t.Errorf("issue[1] (SQL injection): want security category, got %q", verdict.Issues[1].Category)
	}
}

// ---------------------------------------------------------------------------
// ReviewHandler — QA handler with fail
// ---------------------------------------------------------------------------

func TestQAHandlerFailVerdict(t *testing.T) {
	store := newMockStore()
	corrID := "corr-qa-fail"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "Build API",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "Test coverage insufficient.\n\nVERDICT: FAIL\n\n1. No tests for error paths\n2. Missing integration tests",
			Duration: time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "qa",
			Phase:    "qa",
			Persona:  persona.QA,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var verdict event.VerdictPayload
	_ = json.Unmarshal(results[2].Payload, &verdict)
	if verdict.SourcePhase != "qa" {
		t.Errorf("want source phase 'qa', got %q", verdict.SourcePhase)
	}
	if verdict.Phase != "develop" {
		t.Errorf("want target phase 'develop', got %q", verdict.Phase)
	}
	if len(verdict.Issues) != 2 {
		t.Errorf("want 2 issues, got %d", len(verdict.Issues))
	}
}

// ---------------------------------------------------------------------------
// ReviewHandler — backend error propagation
// ---------------------------------------------------------------------------

func TestReviewHandlerBackendError(t *testing.T) {
	store := newMockStore()
	corrID := "corr-review-err"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "test",
		})).WithCorrelation(corrID),
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:    "reviewer",
			Phase:   "review",
			Persona: persona.Reviewer,
			Backend: &mockBackend{name: "claude", err: context.DeadlineExceeded},
			Store:   store, Personas: persona.DefaultRegistry(), Builder: persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error from backend failure")
	}
}

// ---------------------------------------------------------------------------
// ReviewHandler — event source labeling
// ---------------------------------------------------------------------------

func TestReviewHandlerEventSource(t *testing.T) {
	store := newMockStore()
	corrID := "corr-review-source"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "test",
		})).WithCorrelation(corrID),
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:    "reviewer",
			Phase:   "review",
			Persona: persona.Reviewer,
			Backend: &mockBackend{
				name:     "claude",
				response: &backend.Response{Output: "VERDICT: PASS", Duration: time.Second},
			},
			Store: store, Personas: persona.DefaultRegistry(), Builder: persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// VerdictRendered should have handler:reviewer source
	verdictEvt := results[2]
	if verdictEvt.Source != "handler:reviewer" {
		t.Errorf("want source 'handler:reviewer', got %q", verdictEvt.Source)
	}
}

// ---------------------------------------------------------------------------
// extractResponseText — no AIResponseReceived event
// ---------------------------------------------------------------------------

func TestExtractResponseTextEmpty(t *testing.T) {
	h := &ReviewHandler{}
	// Empty event list — no AIResponseReceived.
	text := h.extractResponseText(nil)
	if text != "" {
		t.Errorf("want empty string for nil events, got %q", text)
	}
}

func TestExtractResponseTextNoAIResponse(t *testing.T) {
	h := &ReviewHandler{}
	// Only AIRequestSent — no AIResponseReceived.
	events := []event.Envelope{
		event.New(event.AIRequestSent, 1, event.MustMarshal(event.AIRequestPayload{
			Phase:   "review",
			Backend: "claude",
		})),
	}
	text := h.extractResponseText(events)
	if text != "" {
		t.Errorf("want empty string when no AIResponseReceived, got %q", text)
	}
}

// TestReviewHandlerEmptyAIResponse documents the known risk: when extractResponseText
// returns "" (e.g., no AIResponseReceived in the event list), ParseVerdict defaults
// to PASS. This means a broken AI response pipeline silently produces a pass verdict.
func TestReviewHandlerEmptyAIResponseDefaultsToPass(t *testing.T) {
	// Known risk: empty response text → ParseVerdict("") → VerdictPass (default).
	// This behavior is intentional (optimistic default) but could mask failures.
	v := ParseVerdict("")
	if v.Outcome != event.VerdictPass {
		t.Errorf("want VerdictPass for empty response, got %s", v.Outcome)
	}
	if v.Summary == "" {
		t.Error("summary should not be empty even for default pass")
	}
	if !strings.Contains(v.Summary, "defaulting") {
		t.Errorf("summary should mention defaulting, got: %q", v.Summary)
	}
}

// ---------------------------------------------------------------------------
// ParseVerdict
// ---------------------------------------------------------------------------

func TestParseVerdictPass(t *testing.T) {
	v := ParseVerdict("Everything looks good.\n\nVERDICT: PASS")
	if v.Outcome != event.VerdictPass {
		t.Errorf("want pass, got %s", v.Outcome)
	}
}

func TestParseVerdictFail(t *testing.T) {
	v := ParseVerdict("Issues found:\n\nNeed to fix error handling.\n\nVERDICT: FAIL\n\n1. Fix X\n2. Fix Y")
	if v.Outcome != event.VerdictFail {
		t.Errorf("want fail, got %s", v.Outcome)
	}
	if v.Summary == "" {
		t.Error("summary should not be empty for fail verdict")
	}
}

func TestParseVerdictCaseInsensitive(t *testing.T) {
	v := ParseVerdict("verdict: fail\n\n1. Issue")
	if v.Outcome != event.VerdictFail {
		t.Errorf("want fail (case insensitive), got %s", v.Outcome)
	}
}

func TestParseVerdictNoVerdict(t *testing.T) {
	v := ParseVerdict("This is just some text with no verdict.")
	if v.Outcome != event.VerdictPass {
		t.Errorf("want pass (default when no verdict found), got %s", v.Outcome)
	}
	if !strings.Contains(v.Summary, "defaulting") {
		t.Errorf("summary should mention defaulting: %q", v.Summary)
	}
}

func TestParseVerdictInCodeBlock(t *testing.T) {
	text := "Review complete.\n\n```\nVERDICT: PASS\n```"
	v := ParseVerdict(text)
	if v.Outcome != event.VerdictPass {
		t.Errorf("want pass (from code block), got %s", v.Outcome)
	}
}

func TestParseVerdictLastOccurrenceWins(t *testing.T) {
	text := "VERDICT: PASS\n\nActually...\n\nVERDICT: FAIL\n\n1. Found issue"
	v := ParseVerdict(text)
	// We scan from bottom, so FAIL should be found first
	if v.Outcome != event.VerdictFail {
		t.Errorf("want fail (last verdict wins), got %s", v.Outcome)
	}
}

// ---------------------------------------------------------------------------
// ParseIssues
// ---------------------------------------------------------------------------

func TestParseIssuesFromFail(t *testing.T) {
	text := "Review.\n\nVERDICT: FAIL\n\n1. Missing error handling in handler.go:42\n2. SQL injection risk\n3. Naming convention violation"
	issues := ParseIssues(text, event.VerdictFail)
	if len(issues) != 3 {
		t.Fatalf("want 3 issues, got %d", len(issues))
	}

	// File reference extraction
	if issues[0].File != "handler.go" {
		t.Errorf("issue[0] file: want handler.go, got %q", issues[0].File)
	}
	if issues[0].Line != 42 {
		t.Errorf("issue[0] line: want 42, got %d", issues[0].Line)
	}

	// Severity classification
	if issues[1].Severity != "critical" {
		t.Errorf("issue[1] (injection): want critical, got %q", issues[1].Severity)
	}

	// Category classification
	if issues[2].Category != "good_hygiene" {
		t.Errorf("issue[2] (naming): want good_hygiene, got %q", issues[2].Category)
	}
}

func TestParseIssuesPassReturnsNil(t *testing.T) {
	issues := ParseIssues("VERDICT: PASS", event.VerdictPass)
	if issues != nil {
		t.Errorf("want nil for pass verdict, got %v", issues)
	}
}

func TestParseIssuesBulletedList(t *testing.T) {
	text := "VERDICT: FAIL\n\n- Missing tests for edge cases\n- Performance regression"
	issues := ParseIssues(text, event.VerdictFail)
	if len(issues) != 2 {
		t.Fatalf("want 2 issues from bulleted list, got %d", len(issues))
	}
}

func TestParseIssuesNoIssueLines(t *testing.T) {
	text := "VERDICT: FAIL\n\nSome prose without numbered items."
	issues := ParseIssues(text, event.VerdictFail)
	if len(issues) != 0 {
		t.Errorf("want 0 issues (no numbered/bulleted lines), got %d", len(issues))
	}
}

// ---------------------------------------------------------------------------
// classifySeverity / classifyCategory
// ---------------------------------------------------------------------------

func TestClassifySeverity(t *testing.T) {
	tests := []struct {
		desc string
		want string
	}{
		{"SQL injection in user input", "critical"},
		{"Security vulnerability in auth", "critical"},
		{"Hardcoded credential in config", "critical"},
		{"XSS in template output", "critical"},
		{"Deadlock between goroutines", "critical"},
		{"Data loss on failed transaction", "critical"},
		{"Missing error handling", "major"},
		{"Race condition in cache", "major"},
		{"Goroutine leak in worker pool", "major"},
		{"Breaking change to API contract", "major"},
		{"Silent failure in background job", "major"},
		{"Partial write on multi-table update", "major"},
		{"Rename variable for clarity", "minor"},
	}
	for _, tc := range tests {
		got := classifySeverity(tc.desc)
		if got != tc.want {
			t.Errorf("classifySeverity(%q) = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

func TestClassifyCategory(t *testing.T) {
	tests := []struct {
		desc string
		want string
	}{
		// security
		{"SQL injection vulnerability", "security"},
		{"Auth bypass risk", "security"},
		{"Hardcoded credential in config", "security"},
		{"XSS vulnerability in template", "security"},
		{"CSRF token missing", "security"},
		// concurrency
		{"Race condition in cache update", "concurrency"},
		{"Deadlock between mutex acquisitions", "concurrency"},
		{"Goroutine leak in worker pool", "concurrency"},
		{"Channel misuse causes panic", "concurrency"},
		{"Concurrent map access without lock", "concurrency"},
		{"TOCTOU bug in file creation", "concurrency"},
		// error_handling
		{"Error handling missing in handler", "error_handling"},
		{"Swallowed error in database call", "error_handling"},
		{"Naked return after err != nil check", "error_handling"},
		{"Bare log without error context", "error_handling"},
		// observability
		{"Missing logging on failure path", "observability"},
		{"Dropped tracing context in middleware", "observability"},
		{"Silent failure in background job", "observability"},
		{"No metric for new endpoint latency", "observability"},
		// api_contract
		{"Breaking change to user response", "api_contract"},
		{"Removed field from API contract", "api_contract"},
		{"Changed status code for error response", "api_contract"},
		{"Proto field removal breaks clients", "api_contract"},
		// idempotency
		{"Non-idempotent write endpoint", "idempotency"},
		{"Missing dedup guard on event handler", "idempotency"},
		{"Retry-unsafe database operation", "idempotency"},
		// integration
		{"Missing integration test for API boundary", "integration"},
		{"No contract test for gRPC service", "integration"},
		{"E2E coverage gap in checkout flow", "integration"},
		// data
		{"Unsafe data migration without rollback", "data"},
		{"Partial write on multi-table update", "data"},
		{"Orphaned records after cascade delete", "data"},
		{"Data integrity risk in schema migration", "data"},
		// testing
		{"No tests for error paths", "testing"},
		{"Test coverage below threshold", "testing"},
		// performance
		{"N+1 query in list endpoint", "performance"},
		{"Unbounded SELECT on large table", "performance"},
		{"Missing index on frequently queried column", "performance"},
		// good_hygiene
		{"Code smell in deeply nested function", "good_hygiene"},
		{"Dead code in legacy handler", "good_hygiene"},
		{"Magic number used for timeout", "good_hygiene"},
		{"Anti-pattern in repository layer", "good_hygiene"},
		// correctness (default)
		{"Missing null check", "correctness"},
		{"Logic error in discount calculation", "correctness"},
	}
	for _, tc := range tests {
		got := classifyCategory(tc.desc)
		if got != tc.want {
			t.Errorf("classifyCategory(%q) = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// extractFileRef
// ---------------------------------------------------------------------------

func TestExtractFileRef(t *testing.T) {
	tests := []struct {
		desc     string
		wantFile string
		wantLine int
	}{
		{"Missing check in handler.go:42", "handler.go", 42},
		{"Issue in db.go:15 is critical", "db.go", 15},
		{"See internal/api/server.go:100", "internal/api/server.go", 100},
		{"No file reference here", "", 0},
		{"File without line handler.go", "handler.go", 0},
	}
	for _, tc := range tests {
		file, line := extractFileRef(tc.desc)
		if file != tc.wantFile || line != tc.wantLine {
			t.Errorf("extractFileRef(%q) = (%q, %d), want (%q, %d)",
				tc.desc, file, line, tc.wantFile, tc.wantLine)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func marshalText(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}
