package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// TestNewPRConsolidatorPinsToHaiku locks in the deterministic backend/model
// choice. The consolidator must not drift back onto the review-phase rotation.
func TestNewPRConsolidatorPinsToHaiku(t *testing.T) {
	h := NewPRConsolidator(testDeps())
	if h.model != ConsolidatorModel {
		t.Errorf("model: want %q, got %q", ConsolidatorModel, h.model)
	}
	if h.backend == nil {
		t.Fatal("backend must not be nil")
	}
	if h.backend.Name() != "claude" {
		t.Errorf("backend: want 'claude' (pinned), got %q", h.backend.Name())
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
			Backend: "claude",
			Output:  json.RawMessage(securityOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-security"),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Backend: "claude",
			Output:  json.RawMessage(testingOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-testing"),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Backend: "claude",
			Output:  json.RawMessage(perfOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-performance"),
	}

	params, handlerOutputs, _, _, _, _ := extractConsolidatorInputs(events)

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

	_, _, workspacePath, workspaceBase, diffTruncated, _ := extractConsolidatorInputs(events)

	if workspacePath != "/var/rick/workspaces/repo-rick-ws-pr27" {
		t.Errorf("workspacePath = %q, want the WorkspaceReady.Path", workspacePath)
	}
	if workspaceBase != "main" {
		t.Errorf("workspaceBase = %q, want 'main'", workspaceBase)
	}
	if diffTruncated {
		t.Error("diffTruncated: want false when no pr-diff-truncated enrichment is present")
	}
}

// TestExtractConsolidatorInputs_DetectsTruncatedDiff locks in the guard that
// fires when pr-workspace emits kind=pr-diff-truncated (PR diff exceeds the
// reviewer prompt budget). Every downstream reviewer only sees the first
// ~512 KB of hunks; an all-pass result on that partial view must not be
// promoted to APPROVE. Regression: hulilabs/huli#802 (2026-04-24) had 358
// files, gh pr diff returned HTTP 406, every reviewer passed on an empty
// diff, and the consolidator posted a false APPROVE.
func TestExtractConsolidatorInputs_DetectsTruncatedDiff(t *testing.T) {
	events := []event.Envelope{
		event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
			Source: "pr-workspace",
			Kind:   "pr-diff-truncated",
		})),
	}

	_, _, _, _, diffTruncated, _ := extractConsolidatorInputs(events)
	if !diffTruncated {
		t.Error("diffTruncated: want true when pr-workspace emitted kind=pr-diff-truncated")
	}
}

func TestExtractConsolidatorInputsFallbackToPersona(t *testing.T) {
	// Events without Source fall back to AIResponsePayload.Persona as key.
	output, _ := json.Marshal("fallback output")
	events := []event.Envelope{
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Persona: "reviewer",
			Backend: "claude",
			Output:  json.RawMessage(output),
		})),
	}

	_, handlerOutputs, _, _, _, _ := extractConsolidatorInputs(events)
	if handlerOutputs["reviewer"] != "fallback output" {
		t.Errorf("fallback: want 'fallback output' under reviewer, got %q", handlerOutputs["reviewer"])
	}
}

// TestExtractConsolidatorInputs_CollectsSkippedReviewers verifies that
// PersonaFailed events for category reviewers are surfaced to the
// consolidator so it can report skipped dimensions in the PR body.
// Regression from hulilabs/huli#802 (2026-04-24) where three reviewers
// context-cancelled with no downstream visibility.
func TestExtractConsolidatorInputs_CollectsSkippedReviewers(t *testing.T) {
	events := []event.Envelope{
		event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona: "pr-idempotency",
			Error:   "simulated",
		})),
		event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona: "pr-concurrency",
			Error:   "simulated",
		})),
		// Non-reviewer failures must NOT show up in the skipped list.
		event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona: "pr-workspace",
			Error:   "pre-reviewer stage",
		})),
	}
	_, _, _, _, _, skipped := extractConsolidatorInputs(events)
	if len(skipped) != 2 {
		t.Fatalf("want 2 skipped reviewers, got %d: %v", len(skipped), skipped)
	}
	// sorted alphabetically by the extractor
	if skipped[0] != "pr-concurrency" || skipped[1] != "pr-idempotency" {
		t.Errorf("skipped reviewers not in alpha order: %v", skipped)
	}
}

// TestDowngradeApproveOnSkippedReviewers covers the skipped-reviewer
// downgrade: APPROVE flips to COMMENT with a caveat listing skipped
// reviewers; REQUEST_CHANGES keeps its event but still gets the caveat
// (operators need to know which dimensions are missing regardless);
// non-JSON passes through.
func TestDowngradeApproveOnSkippedReviewers(t *testing.T) {
	t.Run("approve becomes comment with skip list", func(t *testing.T) {
		in := `{"summary":"LGTM.","event":"APPROVE","comments":[],"unanchored":[]}`
		out := downgradeApproveOnSkippedReviewers(in, []string{"pr-idempotency", "pr-concurrency"})
		if !strings.Contains(out, `"event":"COMMENT"`) {
			t.Errorf("APPROVE not downgraded: %q", out)
		}
		if !strings.Contains(out, "pr-idempotency") || !strings.Contains(out, "pr-concurrency") {
			t.Errorf("caveat missing skip list: %q", out)
		}
		if !strings.Contains(out, "Partial review") {
			t.Error("caveat marker missing")
		}
	})

	t.Run("request_changes keeps event but adds caveat", func(t *testing.T) {
		in := `{"summary":"Block.","event":"REQUEST_CHANGES","comments":[],"unanchored":[]}`
		out := downgradeApproveOnSkippedReviewers(in, []string{"pr-hygiene"})
		if !strings.Contains(out, `"event":"REQUEST_CHANGES"`) {
			t.Errorf("REQUEST_CHANGES should not be downgraded: %q", out)
		}
		if !strings.Contains(out, "pr-hygiene") {
			t.Error("caveat missing skipped reviewer name")
		}
	})

	t.Run("empty skipped list passthrough", func(t *testing.T) {
		in := `{"summary":"ok","event":"APPROVE","comments":[],"unanchored":[]}`
		out := downgradeApproveOnSkippedReviewers(in, nil)
		if out != in {
			t.Errorf("no skipped → passthrough expected; got %q", out)
		}
	})

	t.Run("non-json passthrough", func(t *testing.T) {
		in := "plain text fallback"
		out := downgradeApproveOnSkippedReviewers(in, []string{"pr-data"})
		if out != in {
			t.Errorf("non-json should be returned unchanged; got %q", out)
		}
	})
}

// TestDowngradeApproveOnTruncatedDiff covers the three cases:
//   (1) APPROVE → COMMENT with caveat prepended
//   (2) COMMENT / REQUEST_CHANGES left untouched (already not an approval)
//   (3) non-JSON output returned unchanged (fallback path for malformed AI)
func TestDowngradeApproveOnTruncatedDiff(t *testing.T) {
	t.Run("approve becomes comment with caveat", func(t *testing.T) {
		in := `{"summary":"Looks fine.","event":"APPROVE","comments":[],"unanchored":[]}`
		out := downgradeApproveOnTruncatedDiff(in)
		if !strings.Contains(out, `"event":"COMMENT"`) {
			t.Errorf("event was not downgraded to COMMENT: %q", out)
		}
		if !strings.Contains(out, "Partial review") {
			t.Error("caveat marker missing from summary")
		}
	})

	t.Run("comment left untouched", func(t *testing.T) {
		in := `{"summary":"One finding.","event":"COMMENT","comments":[],"unanchored":["x"]}`
		out := downgradeApproveOnTruncatedDiff(in)
		if out != in {
			t.Errorf("COMMENT event should be left untouched; got %q", out)
		}
	})

	t.Run("request_changes left untouched", func(t *testing.T) {
		in := `{"summary":"Blockers.","event":"REQUEST_CHANGES","comments":[],"unanchored":[]}`
		out := downgradeApproveOnTruncatedDiff(in)
		if out != in {
			t.Errorf("REQUEST_CHANGES should be left untouched; got %q", out)
		}
	})

	t.Run("non-json passthrough", func(t *testing.T) {
		in := "not json, plain text"
		out := downgradeApproveOnTruncatedDiff(in)
		if out != in {
			t.Errorf("non-json should be returned unchanged; got %q", out)
		}
	})
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
		model:    ConsolidatorModel,
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
	// Model must propagate — pins rotation-free behaviour.
	if mb.lastReq.Model != ConsolidatorModel {
		t.Errorf("Request.Model: want %q, got %q", ConsolidatorModel, mb.lastReq.Model)
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

// ---------------------------------------------------------------------------
// isSelfAuthorRejection
// ---------------------------------------------------------------------------

func TestIsSelfAuthorRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"approve-self", errors.New(`gh api reviews: {"message":"Validation Failed","errors":[{"message":"Can not approve your own pull request"}]}`), true},
		{"request-changes-self", errors.New(`gh api reviews: {"errors":[{"message":"Can not request changes on your own pull request"}]}`), true},
		{"case-folded", errors.New(`CAN NOT APPROVE YOUR OWN PULL REQUEST`), true},
		{"bad-anchor-422", errors.New(`gh api reviews: pull_request_review_thread.line must be part of the diff`), false},
		{"network-error", errors.New(`gh api reviews: connection refused`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSelfAuthorRejection(tc.err); got != tc.want {
				t.Errorf("isSelfAuthorRejection(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// postConsolidatedReview tiered guards
// ---------------------------------------------------------------------------

// recordedReview captures what the fake runner received so tests can assert on it.
type recordedReview struct {
	calls []reviewPayload
}

// newFakeHandler constructs a PRConsolidatorHandler with every gh side-effect
// replaced by a test double, so postConsolidatedReview can be exercised
// end-to-end without shelling out.
func newFakeHandler() (*PRConsolidatorHandler, *recordedReview) {
	rec := &recordedReview{}
	return &PRConsolidatorHandler{
		postReview: func(_ context.Context, _, _ string, p reviewPayload) error {
			rec.calls = append(rec.calls, p)
			return nil
		},
		postComment:     func(context.Context, string, string, string) error { return nil },
		fetchHeadSHA:    func(context.Context, string, string) (string, error) { return "deadbeef", nil },
		fetchRawDiff:    func(context.Context, string, string, string, string) string { return "" },
		viewerDidAuthor: func(context.Context, string, string) (bool, error) { return false, nil },
	}, rec
}

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("load fixture %s: %v", name, err)
	}
	return string(b)
}

func approveReviewJSON() string {
	return `{"summary":"looks good","event":"APPROVE","comments":[],"unanchored":[]}`
}

func requestChangesReviewJSON() string {
	return `{"summary":"nope","event":"REQUEST_CHANGES","comments":[],"unanchored":[]}`
}

// Tier 1: preflight downgrade — viewerDidAuthor=true + APPROVE → posted as COMMENT.
func TestPostConsolidatedReview_Preflight_DowngradesApproveWhenViewerAuthored(t *testing.T) {
	h, rec := newFakeHandler()
	h.viewerDidAuthor = func(context.Context, string, string) (bool, error) { return true, nil }

	summary, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", approveReviewJSON())
	if err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("want exactly one post, got %d", len(rec.calls))
	}
	if rec.calls[0].Event != "COMMENT" {
		t.Errorf("want event COMMENT (preflight downgrade), got %q", rec.calls[0].Event)
	}
	if !strings.Contains(summary, "downgraded from APPROVE") {
		t.Errorf("summary should mention the downgrade: %q", summary)
	}
}

// Tier 1: same downgrade for REQUEST_CHANGES — GitHub also blocks this for authors.
func TestPostConsolidatedReview_Preflight_DowngradesRequestChangesWhenViewerAuthored(t *testing.T) {
	h, rec := newFakeHandler()
	h.viewerDidAuthor = func(context.Context, string, string) (bool, error) { return true, nil }

	if _, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", requestChangesReviewJSON()); err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if rec.calls[0].Event != "COMMENT" {
		t.Errorf("REQUEST_CHANGES on self-PR must downgrade to COMMENT, got %q", rec.calls[0].Event)
	}
}

// Happy path: not author → post as-is.
func TestPostConsolidatedReview_Preflight_PreservesApproveWhenNotAuthor(t *testing.T) {
	h, rec := newFakeHandler() // viewerDidAuthor returns false by default

	if _, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", approveReviewJSON()); err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if rec.calls[0].Event != "APPROVE" {
		t.Errorf("non-author should post APPROVE unchanged, got %q", rec.calls[0].Event)
	}
}

// Probe failure must not block the happy path — fall through to post attempt.
func TestPostConsolidatedReview_Preflight_ProbeFailureFallsThrough(t *testing.T) {
	h, rec := newFakeHandler()
	h.viewerDidAuthor = func(context.Context, string, string) (bool, error) {
		return false, errors.New("installation token: 403 on /user")
	}

	if _, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", approveReviewJSON()); err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if rec.calls[0].Event != "APPROVE" {
		t.Errorf("probe failure should not downgrade pre-emptively; got %q", rec.calls[0].Event)
	}
}

// Tier 2: reactive self-author rejection — first post returns the real 422
// body, retry downgrades to COMMENT and succeeds.
func TestPostConsolidatedReview_Reactive_SelfAuthorRejection_RetriesAsComment(t *testing.T) {
	fixture := loadFixture(t, "gh_422_self_approve.json")

	rec := &recordedReview{}
	h := &PRConsolidatorHandler{
		postReview: func(_ context.Context, _, _ string, p reviewPayload) error {
			rec.calls = append(rec.calls, p)
			if len(rec.calls) == 1 {
				return fmt.Errorf("gh api reviews: %s (exit 1)", fixture)
			}
			return nil
		},
		postComment:     func(context.Context, string, string, string) error { return nil },
		fetchHeadSHA:    func(context.Context, string, string) (string, error) { return "", nil },
		fetchRawDiff:    func(context.Context, string, string, string, string) string { return "" },
		viewerDidAuthor: func(context.Context, string, string) (bool, error) { return false, errors.New("probe unavailable") },
	}

	summary, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", approveReviewJSON())
	if err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("want 2 post attempts (initial + self-author retry), got %d", len(rec.calls))
	}
	if rec.calls[0].Event != "APPROVE" {
		t.Errorf("first attempt should be APPROVE (probe failed open), got %q", rec.calls[0].Event)
	}
	if rec.calls[1].Event != "COMMENT" {
		t.Errorf("retry should downgrade to COMMENT, got %q", rec.calls[1].Event)
	}
	if !strings.Contains(summary, "self-author rejection") {
		t.Errorf("summary should mention the reactive downgrade: %q", summary)
	}
}

// Tier 2 for REQUEST_CHANGES — same 422 fixture shape, same retry path.
func TestPostConsolidatedReview_Reactive_SelfRequestChangesRejection_RetriesAsComment(t *testing.T) {
	fixture := loadFixture(t, "gh_422_self_request_changes.json")

	rec := &recordedReview{}
	h := &PRConsolidatorHandler{
		postReview: func(_ context.Context, _, _ string, p reviewPayload) error {
			rec.calls = append(rec.calls, p)
			if len(rec.calls) == 1 {
				return fmt.Errorf("gh api reviews: %s (exit 1)", fixture)
			}
			return nil
		},
		postComment:     func(context.Context, string, string, string) error { return nil },
		fetchHeadSHA:    func(context.Context, string, string) (string, error) { return "", nil },
		fetchRawDiff:    func(context.Context, string, string, string, string) string { return "" },
		viewerDidAuthor: func(context.Context, string, string) (bool, error) { return false, errors.New("probe unavailable") },
	}

	if _, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", requestChangesReviewJSON()); err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if rec.calls[1].Event != "COMMENT" {
		t.Errorf("retry must downgrade REQUEST_CHANGES → COMMENT, got %q", rec.calls[1].Event)
	}
}

// Tier 3: unrelated 422 (bad anchor) — strip inline comments, keep the event
// verb, retry. This preserves the pre-existing fallback behaviour.
func TestPostConsolidatedReview_Reactive_BadAnchor_StripsInlineComments(t *testing.T) {
	fixture := loadFixture(t, "gh_422_bad_anchor.json")

	rec := &recordedReview{}
	h := &PRConsolidatorHandler{
		postReview: func(_ context.Context, _, _ string, p reviewPayload) error {
			rec.calls = append(rec.calls, p)
			if len(rec.calls) == 1 {
				return fmt.Errorf("gh api reviews: %s (exit 1)", fixture)
			}
			return nil
		},
		postComment:     func(context.Context, string, string, string) error { return nil },
		fetchHeadSHA:    func(context.Context, string, string) (string, error) { return "", nil },
		fetchRawDiff:    func(context.Context, string, string, string, string) string { return "" },
		viewerDidAuthor: func(context.Context, string, string) (bool, error) { return false, nil },
	}

	// Review with one inline comment so the strip-and-retry path actually fires.
	review := `{"summary":"ok","event":"COMMENT","comments":[{"path":"a.go","line":1,"side":"RIGHT","body":"nit"}],"unanchored":[]}`
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n line\n"

	summary, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", diff, review)
	if err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("want 2 post attempts, got %d", len(rec.calls))
	}
	if len(rec.calls[0].Comments) == 0 {
		t.Error("first attempt should include inline comments")
	}
	if len(rec.calls[1].Comments) != 0 {
		t.Errorf("retry should strip inline comments, got %d", len(rec.calls[1].Comments))
	}
	if !strings.Contains(summary, "inline comments dropped") {
		t.Errorf("summary should mention the anchor fallback: %q", summary)
	}
}

// Body-guard: when every finding is anchored inline and no unanchored entries
// remain, the top-level body must collapse to the canned one-liner — prevents
// the review page from duplicating the inline comment content.
func TestPostConsolidatedReview_InlineOnly_CollapsesBody(t *testing.T) {
	h, rec := newFakeHandler()

	// AI emits a verbose summary that restates the inline finding — the
	// very failure mode we're guarding against.
	review := `{"summary":"### Code review\n\nFound 1 issue:\n\n1. ` +
		"`a.go` line 2 is wrong — details....\",\"event\":\"COMMENT\"," +
		`"comments":[{"path":"a.go","line":2,"side":"RIGHT","body":"nit: fix"}],"unanchored":[]}`
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,2 @@\n ctx\n+added\n"

	if _, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", diff, review); err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("want exactly one post, got %d", len(rec.calls))
	}
	if rec.calls[0].Body != inlineOnlyReviewBody {
		t.Errorf("body: want %q (inline-only guard), got %q", inlineOnlyReviewBody, rec.calls[0].Body)
	}
	if len(rec.calls[0].Comments) != 1 {
		t.Errorf("want 1 inline comment preserved, got %d", len(rec.calls[0].Comments))
	}
}

// Body-guard negative: when there are unanchored findings, the summary must
// still render — the guard kicks in only when all findings are inline.
func TestPostConsolidatedReview_MixedFindings_KeepsSummary(t *testing.T) {
	h, rec := newFakeHandler()

	review := `{"summary":"Overall looks fine.","event":"COMMENT",` +
		`"comments":[{"path":"a.go","line":2,"side":"RIGHT","body":"nit"}],` +
		`"unanchored":["module-wide: add retry logic"]}`
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,2 @@\n ctx\n+added\n"

	if _, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", diff, review); err != nil {
		t.Fatalf("postConsolidatedReview: %v", err)
	}
	body := rec.calls[0].Body
	if body == inlineOnlyReviewBody {
		t.Fatal("guard should NOT fire when unanchored findings exist")
	}
	if !strings.Contains(body, "Overall looks fine.") {
		t.Errorf("body should retain summary, got %q", body)
	}
	if !strings.Contains(body, "Additional findings") {
		t.Errorf("body should include Additional findings section, got %q", body)
	}
}

// Non-422 errors propagate without retry.
func TestPostConsolidatedReview_Reactive_UnrelatedErrorPropagates(t *testing.T) {
	rec := &recordedReview{}
	h := &PRConsolidatorHandler{
		postReview: func(_ context.Context, _, _ string, p reviewPayload) error {
			rec.calls = append(rec.calls, p)
			return errors.New("gh api reviews: 503 Service Unavailable (exit 1)")
		},
		postComment:     func(context.Context, string, string, string) error { return nil },
		fetchHeadSHA:    func(context.Context, string, string) (string, error) { return "", nil },
		fetchRawDiff:    func(context.Context, string, string, string, string) string { return "" },
		viewerDidAuthor: func(context.Context, string, string) (bool, error) { return false, nil },
	}

	// No inline comments → tier-3 fallback is also skipped, so the error bubbles.
	_, err := h.postConsolidatedReview(context.Background(), "owner/repo", "42", "", approveReviewJSON())
	if err == nil {
		t.Fatal("want error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("want original error preserved, got %v", err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("no retry for unrelated errors without inline comments; got %d calls", len(rec.calls))
	}
}

