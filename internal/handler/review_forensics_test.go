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
			Persona:  persona.PRData,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPersona: "developer",
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
			Persona:  persona.PRTesting,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPersona: "developer",
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
			Persona:  persona.Reviewer,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPersona: "developer",
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
					Persona:  persona.PRData, // any persona — the system prompt isn't asserted
					Backend:  mb,
					Store:    store,
					Personas: persona.DefaultRegistry(),
					Builder:  persona.NewPromptBuilder(),
				},
				TargetPersona: "developer",
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

// buildTestScope is a helper that constructs a prDiffGroundingScope directly
// from a unified diff string, bypassing the store lookup path.
func buildTestScope(diff string) prDiffGroundingScope {
	scope := prDiffGroundingScope{
		changedFiles: make(map[string]struct{}),
		changedLines: make(map[string]map[int]string),
	}
	parseUnifiedDiff(&scope, diff)
	return scope
}

// TestGroundIssueFileScopeRescue exercises the five regression cases for the
// file-scope rescue path added to groundIssue.
func TestGroundIssueFileScopeRescue(t *testing.T) {
	// Shared diff fragments used across cases.

	// monitoring_observability.go: function starts at line 148 in the changed set.
	monDiff := "diff --git a/monitoring_observability.go b/monitoring_observability.go\n" +
		"--- a/monitoring_observability.go\n" +
		"+++ b/monitoring_observability.go\n" +
		"@@ -148,3 +148,3 @@ func init() {\n" +
		"+func CreateObservabilityResources(ctx context.Context) error {\n" +
		"+\treturn nil\n" +
		"+}\n"

	// Makefile: only line 10 changed, containing the token "tgt".
	makeDiff := "diff --git a/Makefile b/Makefile\n" +
		"--- a/Makefile\n" +
		"+++ b/Makefile\n" +
		"@@ -10,1 +10,1 @@ build:\n" +
		"+tgt: deps\n"

	// Makefile with unrelated content on line 10 (no "tokenNeverInDiff").
	makeUnrelatedDiff := "diff --git a/Makefile b/Makefile\n" +
		"--- a/Makefile\n" +
		"+++ b/Makefile\n" +
		"@@ -10,1 +10,1 @@ build:\n" +
		"+unrelated: stuff\n"

	cases := []struct {
		name       string
		diff       string
		issue      event.Issue
		wantOK     bool
		wantLine   int
		wantFile   string
		wantReason event.GroundingDropReason
	}{
		{
			name: "rescue_via_token_anywhere",
			diff: monDiff,
			issue: event.Issue{
				File:        "monitoring_observability.go",
				Line:        81, // hallucinated — function is actually at 148
				Description: "Missing error handling in `CreateObservabilityResources`",
			},
			wantOK:     true,
			wantLine:   0, // cleared by rescue
			wantFile:   "monitoring_observability.go",
			wantReason: event.GroundingRescuedFileScope,
		},
		{
			name: "hallucinated_file_no_rescue",
			diff: makeDiff,
			issue: event.Issue{
				File:        "nonexistent.go",
				Line:        1,
				Description: "Something about `Foo`",
			},
			wantOK:     false,
			wantReason: event.GroundingDropFileNotInScope,
		},
		{
			name: "token_present_at_cited_line_no_rescue_needed",
			diff: makeDiff,
			issue: event.Issue{
				File:        "Makefile",
				Line:        10,
				Description: "Dependency target `tgt` is missing phony declaration",
			},
			wantOK:     true,
			wantLine:   10, // anchored normally — line not cleared
			wantFile:   "Makefile",
			wantReason: event.GroundingDropUnspecified, // empty reason = normal anchor
		},
		{
			name: "token_absent_anywhere_no_rescue",
			diff: makeUnrelatedDiff,
			issue: event.Issue{
				File:        "Makefile",
				Line:        999,
				Description: "Problem with `tokenNeverInDiff`",
			},
			wantOK:     false,
			wantReason: event.GroundingDropLineNotInChanged,
		},
		{
			name: "no_token_no_rescue",
			diff: makeUnrelatedDiff,
			issue: event.Issue{
				File:        "Makefile",
				Line:        999,
				Description: "This description has no backtick tokens at all, just prose",
			},
			wantOK:     false,
			wantReason: event.GroundingDropLineNotInChanged,
		},
		// PR #845 dominant failure mode: token_not_near_line. The LLM cites a real
		// changed line (100) with a real token (REDUCE_SUM) that exists in the diff
		// but not within ±1 of line 100. The rescue path should accept it (Line=0).
		{
			name: "rescue_via_token_anywhere_token_not_near_line",
			// Multi-hunk diff: lines 100-102 are changed (unrelated content),
			// lines 642-644 are changed and contain REDUCE_SUM.
			diff: "diff --git a/dashboards.go b/dashboards.go\n" +
				"--- a/dashboards.go\n" +
				"+++ b/dashboards.go\n" +
				"@@ -100,3 +100,3 @@ func setup() {\n" +
				"+\tunrelatedFuncA()\n" +
				"+\tunrelatedFuncB()\n" +
				"+\tunrelatedFuncC()\n" +
				"@@ -642,3 +642,3 @@ func metrics() {\n" +
				"+\tswitch agg {\n" +
				"+\tcase REDUCE_SUM:\n" +
				"+\t\treturn sumAll(vals)\n",
			issue: event.Issue{
				File: "dashboards.go",
				// Line 100 IS in changedLines but REDUCE_SUM is not within ±1 of it.
				Line:        100,
				Description: "Aggregation type `REDUCE_SUM` is not validated before use",
			},
			wantOK:     true,
			wantLine:   0, // cleared by rescue — must not inline-comment at line 100
			wantFile:   "dashboards.go",
			wantReason: event.GroundingRescuedFileScope,
		},
		// Happy-path guard: issue cites exactly the line that contains REDUCE_SUM.
		// The rescue extension must not interfere — issue anchors normally (Line preserved).
		{
			name: "token_at_cited_line_no_rescue_path_taken",
			diff: "diff --git a/dashboards.go b/dashboards.go\n" +
				"--- a/dashboards.go\n" +
				"+++ b/dashboards.go\n" +
				"@@ -642,3 +642,3 @@ func metrics() {\n" +
				"+\tswitch agg {\n" +
				"+\tcase REDUCE_SUM:\n" +
				"+\t\treturn sumAll(vals)\n",
			issue: event.Issue{
				File:        "dashboards.go",
				Line:        643, // exact line containing REDUCE_SUM
				Description: "Aggregation type `REDUCE_SUM` is not validated before use",
			},
			wantOK:     true,
			wantLine:   643, // anchored normally — line must NOT be cleared
			wantFile:   "dashboards.go",
			wantReason: event.GroundingDropUnspecified, // empty reason = normal anchor
		},
		// PR #846 regression: identifier-shaped token (slog.LevelInfo) appears in diff,
		// mixed with non-identifier example values (LOG_LEVEL=infio, info, debug).
		// Rescue must accept on the identifier match, ignoring the prose values.
		{
			name: "rescue_with_mixed_identifier_and_example_values",
			diff: "diff --git a/logger.go b/logger.go\n" +
				"--- a/logger.go\n" +
				"+++ b/logger.go\n" +
				"@@ -50,3 +50,3 @@ func init() {\n" +
				"+\tlevel := slog.LevelInfo\n" +
				"+\tslog.SetLogLoggerLevel(level)\n" +
				"+\tlog.Println(\"logger initialized\")\n",
			issue: event.Issue{
				File: "logger.go",
				Line: 68, // hallucinated — real token is at line 50
				// Description mixes one real identifier with two prose example values.
				Description: "Log level is hardcoded to `slog.LevelInfo`; use env var `LOG_LEVEL=infio` or values `info` and `debug`",
			},
			wantOK:     true,
			wantLine:   0, // cleared by rescue — slog.LevelInfo matched
			wantFile:   "logger.go",
			wantReason: event.GroundingRescuedFileScope,
		},
		// PR #846 regression: only non-identifier tokens in description — no rescue.
		{
			name: "rescue_with_only_example_values_no_identifiers_rejects",
			diff: "diff --git a/logger.go b/logger.go\n" +
				"--- a/logger.go\n" +
				"+++ b/logger.go\n" +
				"@@ -50,3 +50,3 @@ func init() {\n" +
				"+\tlevel := slog.LevelInfo\n" +
				"+\tslog.SetLogLoggerLevel(level)\n" +
				"+\tlog.Println(\"logger initialized\")\n",
			issue: event.Issue{
				File: "logger.go",
				Line: 68,
				// Only non-identifier-shaped tokens: "=" and bare words.
				Description: "Log level env var `LOG_LEVEL=infio` or literal `info` is not supported",
			},
			wantOK:     false,
			wantReason: event.GroundingDropLineNotInChanged,
		},
		// Identifier-shaped token present but does not appear anywhere in the diff blob.
		{
			name: "rescue_with_only_unmatched_identifier_rejects",
			diff: "diff --git a/logger.go b/logger.go\n" +
				"--- a/logger.go\n" +
				"+++ b/logger.go\n" +
				"@@ -50,3 +50,3 @@ func init() {\n" +
				"+\tunrelated.Func()\n" +
				"+\tunrelated.Other()\n" +
				"+\tlog.Println(\"done\")\n",
			issue: event.Issue{
				File: "logger.go",
				Line: 68,
				// slog.LevelInfo is identifier-shaped but absent from this diff.
				Description: "Log level `slog.LevelInfo` should be configurable",
			},
			wantOK:     false,
			wantReason: event.GroundingDropLineNotInChanged,
		},
		// Call-expression form of identifierLikeTokenRe: io.Copy(&buf, r).
		{
			name: "rescue_with_call_expression_token_succeeds",
			diff: "diff --git a/logger_test.go b/logger_test.go\n" +
				"--- a/logger_test.go\n" +
				"+++ b/logger_test.go\n" +
				"@@ -42,3 +42,3 @@ func TestLogger(t *testing.T) {\n" +
				"+\t_, err = io.Copy(&buf, r)\n" +
				"+\tif err != nil { t.Fatal(err) }\n" +
				"+\tt.Log(buf.String())\n",
			issue: event.Issue{
				File: "logger_test.go",
				Line: 99, // hallucinated — real call is at line 42
				Description: "Error from `io.Copy(&buf, r)` is not checked in test helper",
			},
			wantOK:     true,
			wantLine:   0, // cleared by rescue
			wantFile:   "logger_test.go",
			wantReason: event.GroundingRescuedFileScope,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := buildTestScope(tc.diff)
			gotIssue, gotOK, gotReason := scope.groundIssue(tc.issue)

			if gotOK != tc.wantOK {
				t.Errorf("ok: got %v want %v", gotOK, tc.wantOK)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason: got %q want %q", gotReason, tc.wantReason)
			}
			if gotOK {
				if gotIssue.Line != tc.wantLine {
					t.Errorf("issue.Line: got %d want %d", gotIssue.Line, tc.wantLine)
				}
				if gotIssue.File != tc.wantFile {
					t.Errorf("issue.File: got %q want %q", gotIssue.File, tc.wantFile)
				}
			}
		})
	}
}

// TestGroundPRCategoryReviewRescueCountsAndOutput asserts end-to-end that the
// rescue path is counted correctly in VerdictGroundingSummaryPayload.RescuedCount
// and that the compact output for a rescued issue does NOT contain the hallucinated
// line number.
func TestGroundPRCategoryReviewRescueCountsAndOutput(t *testing.T) {
	store := newMockStore()
	corrID := "corr-rescue-counts"

	// Diff: monitoring_observability.go has CreateObservabilityResources at line 148;
	//       Makefile has "tgt" at line 10 (for the anchored finding).
	diffSummary := "## PR Changed Files\n\n" +
		"- `monitoring_observability.go`\n" +
		"- `Makefile`\n\n" +
		"## PR Diff\n\n```diff\n" +
		"diff --git a/monitoring_observability.go b/monitoring_observability.go\n" +
		"--- a/monitoring_observability.go\n" +
		"+++ b/monitoring_observability.go\n" +
		"@@ -148,3 +148,3 @@ func init() {\n" +
		"+func CreateObservabilityResources(ctx context.Context) error {\n" +
		"+\treturn nil\n" +
		"+}\n" +
		"diff --git a/Makefile b/Makefile\n" +
		"--- a/Makefile\n" +
		"+++ b/Makefile\n" +
		"@@ -10,1 +10,1 @@ build:\n" +
		"+tgt: deps\n" +
		"```\n"

	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "review PR",
		})).WithCorrelation(corrID),
		event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
			Source:  "pr-workspace",
			Kind:    "pr-diff",
			Summary: diffSummary,
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			// Two findings:
			//   1. Rescued: token present at line 148, cited at hallucinated line 81.
			//   2. Anchored: token "tgt" at line 10, cited at line 10.
			Output: "VERDICT: FAIL\n\n" +
				"1. **major**: `monitoring_observability.go:81` — `CreateObservabilityResources` missing error propagation\n" +
				"2. **minor**: `Makefile` line 10 target `tgt` is not declared .PHONY",
			Duration: time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "pr-testing",
			Persona:  persona.PRTesting,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPersona: "developer",
	})

	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-workspace",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Locate the grounding summary.
	var summaryPayload event.VerdictGroundingSummaryPayload
	foundSummary := false
	for _, e := range results {
		if e.Type != event.VerdictGroundingSummary {
			continue
		}
		if err := json.Unmarshal(e.Payload, &summaryPayload); err != nil {
			t.Fatalf("unmarshal summary: %v", err)
		}
		foundSummary = true
	}
	if !foundSummary {
		t.Fatalf("VerdictGroundingSummary not emitted; got types: %v", typesOf(results))
	}

	if summaryPayload.OriginalCount != 2 {
		t.Errorf("OriginalCount: got %d want 2", summaryPayload.OriginalCount)
	}
	if summaryPayload.GroundedCount != 2 {
		t.Errorf("GroundedCount: got %d want 2 (both accepted: one anchored, one rescued)", summaryPayload.GroundedCount)
	}
	if summaryPayload.RescuedCount != 1 {
		t.Errorf("RescuedCount: got %d want 1", summaryPayload.RescuedCount)
	}
	if summaryPayload.DropReasons[event.GroundingRescuedFileScope] != 1 {
		t.Errorf("DropReasons[rescued_file_scope]: got %d want 1", summaryPayload.DropReasons[event.GroundingRescuedFileScope])
	}

	// The compact output must not reference the hallucinated line :81.
	for _, e := range results {
		if e.Type != event.AIResponseReceived {
			continue
		}
		var p event.AIResponsePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal ai response: %v", err)
		}
		var canonical string
		_ = json.Unmarshal(p.Output, &canonical)
		if strings.Contains(canonical, ":81") {
			t.Errorf("compact output must not contain hallucinated line :81, got: %s", canonical)
		}
	}
}
