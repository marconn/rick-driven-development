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
// PRConsolidatorHandler construction
// ---------------------------------------------------------------------------

func TestNewPRConsolidator(t *testing.T) {
	h := NewPRConsolidator(testDeps())
	if h.Name() != "pr-consolidator" {
		t.Errorf("want name 'pr-consolidator', got %q", h.Name())
	}

	// DAG-based dispatch — Subscribes returns nil.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil Subscribes (DAG-based dispatch), got %v", subs)
	}
}

// ---------------------------------------------------------------------------
// extractConsolidatorInputs
// ---------------------------------------------------------------------------

func TestExtractConsolidatorInputs(t *testing.T) {
	corrID := "corr-consolidate-1"

	securityOutput, _ := json.Marshal("Security review output")
	testingOutput, _ := json.Marshal("Testing review output")
	perfOutput, _ := json.Marshal("Performance review output")

	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "Review this PR",
			WorkflowID: "pr-review",
			Source:     "gh:owner/repo#42",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "pr-category-review",
			Backend: "claude",
			Output:  json.RawMessage(securityOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-security"),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "pr-category-review",
			Backend: "claude",
			Output:  json.RawMessage(testingOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-testing"),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "pr-category-review",
			Backend: "claude",
			Output:  json.RawMessage(perfOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-performance"),
	}

	params, handlerOutputs, _ := extractConsolidatorInputs(events)

	if params.Source != "gh:owner/repo#42" {
		t.Errorf("params.Source: want 'gh:owner/repo#42', got %q", params.Source)
	}
	if params.Prompt != "Review this PR" {
		t.Errorf("params.Prompt: want 'Review this PR', got %q", params.Prompt)
	}
	if handlerOutputs["pr-security"] != "Security review output" {
		t.Errorf("pr-security output: want 'Security review output', got %q", handlerOutputs["pr-security"])
	}
	if handlerOutputs["pr-testing"] != "Testing review output" {
		t.Errorf("pr-testing output: want 'Testing review output', got %q", handlerOutputs["pr-testing"])
	}
	if handlerOutputs["pr-performance"] != "Performance review output" {
		t.Errorf("pr-performance output: want 'Performance review output', got %q", handlerOutputs["pr-performance"])
	}
}

// TestExtractConsolidatorInputs_CapturesWorkspacePath locks in the fix for the
// PR #27 crash: the consolidator must run its backend inside the PR workspace
// (a git repo), otherwise codex exits fast with "not inside a trusted
// directory". The workspace path comes from WorkspaceReady, emitted by
// pr-workspace earlier in the DAG.
func TestExtractConsolidatorInputs_CapturesWorkspacePath(t *testing.T) {
	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Source: "gh:owner/repo#27",
		})),
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path:   "/var/rick/workspaces/repo-rick-ws-pr27",
			Branch: "feature/fix",
			Base:   "main",
		})),
	}

	_, _, workspacePath := extractConsolidatorInputs(events)

	if workspacePath != "/var/rick/workspaces/repo-rick-ws-pr27" {
		t.Errorf("workspacePath = %q, want the WorkspaceReady.Path", workspacePath)
	}
}

func TestExtractConsolidatorInputsFallbackToPhase(t *testing.T) {
	// Events without Source fall back to Phase as key.
	output, _ := json.Marshal("fallback output")
	events := []event.Envelope{
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "review",
			Backend: "claude",
			Output:  json.RawMessage(output),
		})),
	}

	_, handlerOutputs, _ := extractConsolidatorInputs(events)
	if handlerOutputs["review"] != "fallback output" {
		t.Errorf("fallback: want 'fallback output', got %q", handlerOutputs["review"])
	}
}

// ---------------------------------------------------------------------------
// buildConsolidationPrompt
// ---------------------------------------------------------------------------

func TestBuildConsolidationPrompt(t *testing.T) {
	params := event.WorkflowRequestedPayload{
		Prompt: "Review PR for security changes",
		Source: "gh:owner/repo#10",
	}
	handlerOutputs := map[string]string{
		"pr-security":    "No security issues found.",
		"pr-testing":     "Missing unit tests.",
		"pr-performance": "N+1 query detected.",
	}

	diff := "--- a/foo.go\n+++ b/foo.go\n@@ -1,2 +1,3 @@\n func f() {}\n+newline\n"
	prompt := buildConsolidationPrompt(params, handlerOutputs, diff)

	if !strings.Contains(prompt, "Review PR for security changes") {
		t.Error("prompt should contain task description")
	}
	if !strings.Contains(prompt, "gh:owner/repo#10") {
		t.Error("prompt should contain source reference")
	}
	if !strings.Contains(prompt, "No security issues found") {
		t.Error("prompt should contain security review output")
	}
	if !strings.Contains(prompt, "Missing unit tests") {
		t.Error("prompt should contain testing review output")
	}
	if !strings.Contains(prompt, "N+1 query detected") {
		t.Error("prompt should contain performance review output")
	}
	if !strings.Contains(prompt, "Security Review") {
		t.Error("prompt should label each section")
	}
	if !strings.Contains(prompt, "## PR Diff") {
		t.Error("prompt should include a PR Diff section")
	}
	if !strings.Contains(prompt, "newline") {
		t.Error("prompt should embed the raw diff so the AI can anchor comments")
	}
	if !strings.Contains(prompt, "JSON") {
		t.Error("prompt should instruct the AI to emit JSON")
	}
}

func TestBuildConsolidationPromptMissingOutputs(t *testing.T) {
	params := event.WorkflowRequestedPayload{
		Source: "gh:owner/repo#5",
	}
	// Simulate most personas not yet having output.
	handlerOutputs := map[string]string{
		"pr-security": "Looks clean.",
	}

	prompt := buildConsolidationPrompt(params, handlerOutputs, "")

	if !strings.Contains(prompt, "(no output)") {
		t.Error("prompt should show '(no output)' for missing handlers")
	}
	if !strings.Contains(prompt, "diff unavailable") {
		t.Error("prompt should signal missing diff so AI prefers unanchored entries")
	}
}

// ---------------------------------------------------------------------------
// PRConsolidatorHandler.callAI — uses mock backend
// ---------------------------------------------------------------------------

func TestPRConsolidatorCallAI(t *testing.T) {
	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "## Consolidated Review\n\nREQUEST CHANGES",
			Duration: time.Second,
		},
	}
	reg := persona.DefaultRegistry()
	h := &PRConsolidatorHandler{
		backend:  mb,
		store:    newMockStore(),
		registry: reg,
		builder:  persona.NewPromptBuilder(),
	}

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-ai-test")
	params := event.WorkflowRequestedPayload{
		Source: "gh:owner/repo#1",
		Prompt: "test",
	}
	handlerOutputs := map[string]string{
		"pr-security": "ok",
		"pr-testing":  "ok",
	}

	output, err := h.callAI(context.Background(), env, params, handlerOutputs, "", "--- a/x\n+++ b/x\n@@ -0,0 +1 @@\n+added\n")
	if err != nil {
		t.Fatalf("callAI: %v", err)
	}
	if !strings.Contains(output, "Consolidated Review") {
		t.Errorf("want consolidated review in output, got %q", output)
	}
	// Verify the prompt was built and sent.
	if !strings.Contains(mb.lastReq.UserPrompt, "gh:owner/repo#1") {
		t.Error("user prompt should contain source reference")
	}
	if !strings.Contains(mb.lastReq.UserPrompt, "## PR Diff") {
		t.Error("user prompt should include the PR diff section")
	}
}

// ---------------------------------------------------------------------------
// JSON extraction + parsing
// ---------------------------------------------------------------------------

func TestParseConsolidatorJSON_Strict(t *testing.T) {
	raw := `{"summary":"ok","event":"COMMENT","comments":[{"path":"a.go","line":3,"side":"RIGHT","body":"fix this"}],"unanchored":["global note"]}`
	r, err := parseConsolidatorJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Event != "COMMENT" || r.Summary != "ok" {
		t.Errorf("header fields: %+v", r)
	}
	if len(r.Comments) != 1 || r.Comments[0].Path != "a.go" || r.Comments[0].Line != 3 {
		t.Errorf("comments: %+v", r.Comments)
	}
	if len(r.Unanchored) != 1 || r.Unanchored[0] != "global note" {
		t.Errorf("unanchored: %+v", r.Unanchored)
	}
}

func TestParseConsolidatorJSON_WithProseAndFence(t *testing.T) {
	raw := "Here you go:\n```json\n{\"summary\":\"s\",\"event\":\"APPROVE\",\"comments\":[],\"unanchored\":[]}\n```\nThanks!"
	r, err := parseConsolidatorJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Event != "APPROVE" {
		t.Errorf("want APPROVE, got %q", r.Event)
	}
}

func TestParseConsolidatorJSON_NoJSON(t *testing.T) {
	if _, err := parseConsolidatorJSON("nothing structured here"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Diff-anchor parsing + comment validation
// ---------------------------------------------------------------------------

func TestParseDiffAnchors_MultiFile(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -10,3 +10,4 @@
 ctx
-old
+new
+extra
diff --git a/bar.go b/bar.go
--- a/bar.go
+++ b/bar.go
@@ -1,1 +1,2 @@
 unchanged
+brand new
`
	a := parseDiffAnchors(diff)

	// foo.go: right side = 10 (context), 11 (+new), 12 (+extra)
	for _, ln := range []int{10, 11, 12} {
		if !a.allows("foo.go", ln, "RIGHT") {
			t.Errorf("foo.go RIGHT line %d should be allowed", ln)
		}
	}
	if a.allows("foo.go", 99, "RIGHT") {
		t.Error("line outside hunks must not anchor")
	}
	// foo.go: left side = 10 (context), 11 (-old)
	if !a.allows("foo.go", 10, "LEFT") || !a.allows("foo.go", 11, "LEFT") {
		t.Error("LEFT anchors for foo.go wrong")
	}
	// bar.go: right side = 1 (context), 2 (+brand new)
	if !a.allows("bar.go", 2, "RIGHT") {
		t.Error("bar.go:2 RIGHT should be allowed")
	}
	// Unknown file must not anchor.
	if a.allows("unknown.go", 1, "RIGHT") {
		t.Error("unknown file must not anchor")
	}
}

func TestSplitComments_FiltersInvalidAnchors(t *testing.T) {
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,2 @@\n ctx\n+added\n"
	anchors := parseDiffAnchors(diff)

	review := consolidatorReview{
		Comments: []consolidatorComment{
			{Path: "a.go", Line: 2, Side: "RIGHT", Body: "good anchor"},
			{Path: "a.go", Line: 99, Side: "RIGHT", Body: "bad line"},
			{Path: "missing.go", Line: 1, Side: "RIGHT", Body: "bad path"},
			{Path: "a.go", Line: 2, Side: "", Body: "missing side defaults RIGHT"},
			{Path: "a.go", Line: 2, Side: "RIGHT", Body: "   "}, // dropped (empty body)
		},
		Unanchored: []string{"seed"},
	}

	valid, unanchored := splitComments(review, anchors)
	if len(valid) != 2 {
		t.Fatalf("want 2 valid comments, got %d: %+v", len(valid), valid)
	}
	for _, c := range valid {
		if c.Side != "RIGHT" {
			t.Errorf("side should default to RIGHT, got %q", c.Side)
		}
	}
	// seed + bad line + bad path = 3 unanchored entries
	if len(unanchored) != 3 {
		t.Fatalf("want 3 unanchored entries, got %d: %+v", len(unanchored), unanchored)
	}
	if unanchored[0] != "seed" {
		t.Errorf("first unanchored should be the AI-provided seed, got %q", unanchored[0])
	}
}

func TestRenderReviewBody(t *testing.T) {
	body := renderReviewBody("Overall looks fine.", []string{"module-wide: add retry logic"})
	if !strings.Contains(body, "Overall looks fine.") {
		t.Error("body should include summary")
	}
	if !strings.Contains(body, "### Additional findings") {
		t.Error("body should include the Additional findings header when unanchored present")
	}
	if !strings.Contains(body, "- module-wide: add retry logic") {
		t.Error("body should list unanchored findings as bullets")
	}

	empty := renderReviewBody("", nil)
	if empty == "" {
		t.Error("fallback body should be non-empty when summary missing")
	}
}

func TestNormalizeReviewEvent(t *testing.T) {
	cases := map[string]string{
		"APPROVE":          "APPROVE",
		"approve":          "APPROVE",
		"REQUEST_CHANGES":  "REQUEST_CHANGES",
		"request changes":  "REQUEST_CHANGES",
		"REQUESTCHANGES":   "REQUEST_CHANGES",
		"COMMENT":          "COMMENT",
		"":                 "COMMENT",
		"anything-else":    "COMMENT",
	}
	for input, want := range cases {
		if got := normalizeReviewEvent(input); got != want {
			t.Errorf("normalizeReviewEvent(%q) = %q, want %q", input, got, want)
		}
	}
}

