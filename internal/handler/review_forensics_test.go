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

// TestParseVerdictReportsExplicitVsDefault covers the three VerdictSource
// values that ParseVerdict can produce. The fourth (DowngradedNoGrounded) is
// covered by TestReviewHandlerPRCategoryReviewFiltersUngroundedIssues.
func TestParseVerdictReportsExplicitVsDefault(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantSource event.VerdictSource
	}{
		{"explicit pass", "code looks fine.\n\nVERDICT: PASS", event.VerdictSourceExplicitPass},
		{"explicit fail", "issues found:\n\n1. broken\n\nVERDICT: FAIL", event.VerdictSourceExplicitFail},
		{"default optimistic — no verdict line", "the LLM forgot to emit a verdict", event.VerdictSourceDefaultOptimistic},
		{"default optimistic — empty input", "", event.VerdictSourceDefaultOptimistic},
		{"case insensitive pass", "verdict: pass", event.VerdictSourceExplicitPass},
		{"case insensitive fail", "VeRdIcT: fAiL", event.VerdictSourceExplicitFail},
		{"verdict in middle of line", "Final assessment — VERDICT: PASS — done", event.VerdictSourceExplicitPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseVerdict(tc.text)
			if got.Source != tc.wantSource {
				t.Errorf("ParseVerdict(%q).Source = %q; want %q", tc.text, got.Source, tc.wantSource)
			}
		})
	}
}

// TestRewriteAIResponseTextPreservesRaw asserts that when grounding mutates
// the text, the original LLM bytes survive in OutputRaw and the canonical
// Output holds the rewritten text.
func TestRewriteAIResponseTextPreservesRaw(t *testing.T) {
	rawText := "VERDICT: FAIL\n\n1. Critical: foo bar baz"
	groundedText := "No grounded issues found in the changed lines for this review category.\n\nVERDICT: PASS"

	rawJSON, _ := json.Marshal(rawText)
	events := []event.Envelope{
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:  "pr-category-review",
			Output: rawJSON,
		})),
	}

	rewritten := rewriteAIResponseText(events, groundedText, rawText)
	if len(rewritten) != 1 {
		t.Fatalf("want 1 event, got %d", len(rewritten))
	}

	var got event.AIResponsePayload
	if err := json.Unmarshal(rewritten[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var canonical, raw string
	_ = json.Unmarshal(got.Output, &canonical)
	_ = json.Unmarshal(got.OutputRaw, &raw)
	if canonical != groundedText {
		t.Errorf("Output canonical drift: got %q want %q", canonical, groundedText)
	}
	if raw != rawText {
		t.Errorf("OutputRaw drift: got %q want %q", raw, rawText)
	}
	if canonical == raw {
		t.Errorf("Output and OutputRaw should differ when grounding rewrote")
	}
}

// TestRewriteAIResponseTextOmitsRawWhenUnchanged asserts the omitempty
// contract: when the rewritten text equals the original (no actual rewrite),
// OutputRaw is not populated and the marshaled JSON has no output_raw key.
func TestRewriteAIResponseTextOmitsRawWhenUnchanged(t *testing.T) {
	text := "Nothing to rewrite here.\n\nVERDICT: PASS"
	textJSON, _ := json.Marshal(text)
	events := []event.Envelope{
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:  "pr-category-review",
			Output: textJSON,
		})),
	}

	rewritten := rewriteAIResponseText(events, text, text)
	if strings.Contains(string(rewritten[0].Payload), `"output_raw"`) {
		t.Errorf("output_raw key leaked into payload despite no rewrite: %s", rewritten[0].Payload)
	}

	// And the same when raw is empty (extractResponseText returned "").
	rewritten = rewriteAIResponseText(events, text, "")
	if strings.Contains(string(rewritten[0].Payload), `"output_raw"`) {
		t.Errorf("output_raw key leaked when rawText was empty: %s", rewritten[0].Payload)
	}
}

// TestReviewHandlerPRCategoryReviewEmitsGroundingSummary feeds three issues
// (one good, one with wrong line, one with wrong file) and asserts the
// summary payload counts and DropReasons taxonomy populate correctly. Also
// asserts the verdict is demoted with VerdictSourceDowngradedNoGrounded.
func TestReviewHandlerPRCategoryReviewEmitsGroundingSummary(t *testing.T) {
	store := newMockStore()
	corrID := "corr-grounding-summary"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "review PR",
		})).WithCorrelation(corrID),
		event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
			Source: "pr-jira-context",
			Kind:   "pr-diff",
			Summary: "## PR Changed Files\n\n- `Makefile`\n\n## PR Diff\n\n```diff\n" +
				"diff --git a/Makefile b/Makefile\n" +
				"--- a/Makefile\n" +
				"+++ b/Makefile\n" +
				"@@ -10,1 +10,1 @@ check:\n" +
				"+\tif mise doctor >/dev/null 2>&1; then \\\n" +
				"```\n",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "gemini",
		response: &backend.Response{
			// Three issues:
			//   1. Wrong file (not in scope).
			//   2. Wrong line (file in scope but line not changed).
			//   3. Right file+line but no codespan token in description (still grounds).
			Output: "VERDICT: FAIL\n\n" +
				"1. **major**: `nonexistent.go` line 1 fake claim\n" +
				"2. **major**: `Makefile` line 999 fake claim about line\n" +
				"3. **minor**: `Makefile` line 10 some unrelated comment",
			Duration: time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "pr-data",
			Phase:    "pr-category-review",
			Persona:  persona.PRData,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-jira-context",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Locate the grounding summary event — it's appended last.
	var summaryPayload event.VerdictGroundingSummaryPayload
	found := false
	for _, e := range results {
		if e.Type == event.VerdictGroundingSummary {
			if err := json.Unmarshal(e.Payload, &summaryPayload); err != nil {
				t.Fatalf("unmarshal summary: %v", err)
			}
			found = true
			if e.Source != "handler:pr-data" {
				t.Errorf("summary Source: got %q want handler:pr-data", e.Source)
			}
		}
	}
	if !found {
		t.Fatalf("VerdictGroundingSummary event not emitted; got types: %v", typesOf(results))
	}

	if summaryPayload.Reviewer != "pr-data" {
		t.Errorf("summary reviewer: got %q want pr-data", summaryPayload.Reviewer)
	}
	if summaryPayload.OriginalCount != 3 {
		t.Errorf("OriginalCount: got %d want 3", summaryPayload.OriginalCount)
	}
	if summaryPayload.OriginalOutcome != event.VerdictFail {
		t.Errorf("OriginalOutcome: got %s want fail", summaryPayload.OriginalOutcome)
	}
	// Sum of DropReasons must equal OriginalCount - GroundedCount.
	totalDropped := 0
	for _, n := range summaryPayload.DropReasons {
		totalDropped += n
	}
	if totalDropped != summaryPayload.OriginalCount-summaryPayload.GroundedCount {
		t.Errorf("DropReasons accounting drift: total=%d want %d (OriginalCount %d - GroundedCount %d)",
			totalDropped, summaryPayload.OriginalCount-summaryPayload.GroundedCount,
			summaryPayload.OriginalCount, summaryPayload.GroundedCount)
	}
	// Wrong-file issue must show up under file_not_in_scope.
	if summaryPayload.DropReasons[event.GroundingDropFileNotInScope] < 1 {
		t.Errorf("expected at least one file_not_in_scope drop, got %v", summaryPayload.DropReasons)
	}

	// Verdict reflects the result of grounding.
	verdictEvt := findVerdict(t, results)
	var verdict event.VerdictPayload
	if err := json.Unmarshal(verdictEvt.Payload, &verdict); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if verdict.Source == "" {
		t.Errorf("verdict Source must be populated, got empty")
	}
}

// TestReviewHandlerPRCategoryReviewDowngradeStampsSource extends the existing
// FiltersUngroundedIssues coverage: when ALL issues fail grounding, the verdict
// must carry VerdictSourceDowngradedNoGrounded so operators can spot the
// demoted path without reading OutputRaw.
func TestReviewHandlerPRCategoryReviewDowngradeStampsSource(t *testing.T) {
	store := newMockStore()
	corrID := "corr-downgrade-stamp"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "review",
		})).WithCorrelation(corrID),
		event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
			Source: "pr-jira-context",
			Kind:   "pr-diff",
			Summary: "## PR Changed Files\n\n- `Makefile`\n\n## PR Diff\n\n```diff\n" +
				"diff --git a/Makefile b/Makefile\n" +
				"--- a/Makefile\n" +
				"+++ b/Makefile\n" +
				"@@ -10,1 +10,1 @@ install:\n" +
				"+\t@mise install\n" +
				"```\n",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "gemini",
		response: &backend.Response{
			Output:   "VERDICT: FAIL\n\n1. **Critical**: `Makefile` line 10 references `mise.toml` instead of `mise`",
			Duration: time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "pr-testing",
			Phase:    "pr-category-review",
			Persona:  persona.PRTesting,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPhase: "develop",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-jira-context",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	verdictEvt := findVerdict(t, results)
	var verdict event.VerdictPayload
	if err := json.Unmarshal(verdictEvt.Payload, &verdict); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if verdict.Outcome != event.VerdictPass {
		t.Errorf("want demoted PASS, got %s", verdict.Outcome)
	}
	if verdict.Source != event.VerdictSourceDowngradedNoGrounded {
		t.Errorf("Source: got %q want downgraded_no_grounded", verdict.Source)
	}

	// AIResponseReceived must carry OutputRaw with the original LLM text so
	// the operator can recover what was filtered.
	for _, e := range results {
		if e.Type != event.AIResponseReceived {
			continue
		}
		var p event.AIResponsePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal ai response: %v", err)
		}
		if p.OutputRaw == nil {
			t.Errorf("OutputRaw should be populated for downgraded verdict, got nil")
		}
		var raw string
		_ = json.Unmarshal(p.OutputRaw, &raw)
		if !strings.Contains(raw, "VERDICT: FAIL") {
			t.Errorf("OutputRaw should preserve original FAIL verdict, got %q", raw)
		}
	}
}

// TestNonPRReviewerOmitsOutputRaw asserts a non-pr-category-review reviewer
// does NOT touch OutputRaw — the field stays absent on standard reviewer
// responses.
func TestNonPRReviewerOmitsOutputRaw(t *testing.T) {
	store := newMockStore()
	corrID := "corr-non-pr-reviewer"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "build a thing",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "Looks good.\n\nVERDICT: PASS",
			Duration: time.Second,
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

	for _, e := range results {
		if e.Type == event.AIResponseReceived {
			if strings.Contains(string(e.Payload), `"output_raw"`) {
				t.Errorf("output_raw leaked into non-pr-review payload: %s", e.Payload)
			}
		}
		if e.Type == event.VerdictGroundingSummary {
			t.Errorf("non-pr-review handler must not emit VerdictGroundingSummary")
		}
	}
}

// TestRegressionFC2DE5A1Pattern locks in the diagnostic intent: synthesize the
// fc2de5a1 pattern (12 fake reviewers each returning text WITHOUT a VERDICT:
// line) and assert all 12 verdicts carry VerdictSourceDefaultOptimistic so
// future operators see the bail-out pattern in the event log.
func TestRegressionFC2DE5A1Pattern(t *testing.T) {
	reviewerNames := []string{
		"pr-security", "pr-concurrency", "pr-error-handling", "pr-observability",
		"pr-api-contract", "pr-idempotency", "pr-testing", "pr-integration",
		"pr-performance", "pr-data", "pr-hygiene", "pr-vendor-resilience",
	}

	for _, name := range reviewerNames {
		t.Run(name, func(t *testing.T) {
			store := newMockStore()
			corrID := "corr-fc2de5a1-" + name
			store.correlationEvents[corrID] = []event.Envelope{
				event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
					Prompt: "review",
				})).WithCorrelation(corrID),
				event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
					Source: "pr-workspace",
					Kind:   "pr-diff",
					Summary: "## PR Changed Files\n\n- `foo.go`\n\n## PR Diff\n\n```diff\n" +
						"diff --git a/foo.go b/foo.go\n" +
						"--- a/foo.go\n" +
						"+++ b/foo.go\n" +
						"@@ -1,1 +1,1 @@\n" +
						"+package foo\n" +
						"```\n",
				})).WithCorrelation(corrID),
			}

			mb := &mockBackend{
				name: "claude",
				response: &backend.Response{
					// Pure chatter — no VERDICT: line. ParseVerdict will default
					// optimistically; new telemetry must surface this.
					Output:   "I looked at the diff and have nothing to add for this review category.",
					Duration: 11 * time.Second,
				},
			}

			h := NewReviewHandler(ReviewHandlerConfig{
				AIConfig: AIHandlerConfig{
					Name:     name,
					Phase:    "pr-category-review",
					Persona:  persona.PRData, // any persona — the system prompt isn't asserted
					Backend:  mb,
					Store:    store,
					Personas: persona.DefaultRegistry(),
					Builder:  persona.NewPromptBuilder(),
				},
				TargetPhase: "develop",
			})

			env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
				Persona: "pr-workspace",
			})).WithCorrelation(corrID)

			results, err := h.Handle(context.Background(), env)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}

			verdictEvt := findVerdict(t, results)
			var verdict event.VerdictPayload
			if err := json.Unmarshal(verdictEvt.Payload, &verdict); err != nil {
				t.Fatalf("unmarshal verdict: %v", err)
			}
			if verdict.Source != event.VerdictSourceDefaultOptimistic {
				t.Errorf("want VerdictSourceDefaultOptimistic for chatter-only LLM output, got %q", verdict.Source)
			}
		})
	}
}

// findVerdict locates the VerdictRendered event in a result slice.
func findVerdict(t *testing.T, events []event.Envelope) event.Envelope {
	t.Helper()
	for _, e := range events {
		if e.Type == event.VerdictRendered {
			return e
		}
	}
	t.Fatalf("VerdictRendered event not found; got types: %v", typesOf(events))
	return event.Envelope{}
}

func typesOf(events []event.Envelope) []event.Type {
	out := make([]event.Type, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}
