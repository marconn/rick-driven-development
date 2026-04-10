package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// PRConsolidatorHandler collects outputs from the 11 dedicated category
// reviewers, calls AI to produce a single consolidated review comment, and
// posts it to the PR via `gh pr comment`. This is the only handler in the
// flow that has an external side-effect (posting the comment).
type PRConsolidatorHandler struct {
	backend  backend.Backend
	store    eventstore.Store
	registry *persona.Registry
	builder  *persona.PromptBuilder
	workDir  string
	yolo     bool
}

// NewPRConsolidator creates a PRConsolidatorHandler from the shared Deps.
func NewPRConsolidator(d Deps) *PRConsolidatorHandler {
	return &PRConsolidatorHandler{
		backend:  d.Backend,
		store:    d.Store,
		registry: d.Personas,
		builder:  d.Builder,
		workDir:  d.WorkDir,
		yolo:     d.Yolo,
	}
}

// Name returns the unique handler identifier.
func (h *PRConsolidatorHandler) Name() string { return "pr-consolidator" }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *PRConsolidatorHandler) Subscribes() []event.Type { return nil }

// Handle loads all AI outputs from the correlation chain, builds a consolidation
// prompt, calls AI, posts the result as a PR comment, and emits ContextEnrichment.
func (h *PRConsolidatorHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: load correlation chain: %w", err)
	}

	params, phaseOutputs := extractConsolidatorInputs(events)

	fullRepo, prNumber, err := parsePRSource(params.Source)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: parse source %q: %w", params.Source, err)
	}

	consolidatedOutput, err := h.callAI(ctx, env, params, phaseOutputs)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: AI call: %w", err)
	}

	if err := postPRComment(ctx, fullRepo, prNumber, consolidatedOutput); err != nil {
		return nil, fmt.Errorf("pr-consolidator: post PR comment: %w", err)
	}

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "pr-consolidator",
		Kind:    "pr-comment",
		Summary: fmt.Sprintf("Posted consolidated review to %s#%s", fullRepo, prNumber),
	})).WithSource("handler:pr-consolidator")

	return []event.Envelope{enrichEvt}, nil
}

// extractConsolidatorInputs scans the correlation chain and returns the
// WorkflowRequestedPayload and a map of handler name → AI output text.
// Keyed by handler name (from event Source "handler:<name>") so that
// multiple handlers sharing the same phase template don't collide.
func extractConsolidatorInputs(events []event.Envelope) (event.WorkflowRequestedPayload, map[string]string) {
	var params event.WorkflowRequestedPayload
	handlerOutputs := make(map[string]string)

	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			_ = json.Unmarshal(e.Payload, &params)

		case event.AIResponseReceived:
			var p event.AIResponsePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				key := strings.TrimPrefix(e.Source, "handler:")
				if key == "" {
					key = p.Phase // fallback for events without Source
				}
				handlerOutputs[key] = unmarshalOutput(p.Output, p.Structured)
			}
		}
	}

	return params, handlerOutputs
}

// callAI builds the consolidation prompt and invokes the AI backend.
func (h *PRConsolidatorHandler) callAI(
	ctx context.Context,
	env event.Envelope,
	params event.WorkflowRequestedPayload,
	phaseOutputs map[string]string,
) (string, error) {
	systemPrompt, err := h.registry.LoadSystemPrompt(persona.PRConsolidator)
	if err != nil {
		return "", fmt.Errorf("load system prompt: %w", err)
	}

	userPrompt := buildConsolidationPrompt(params, phaseOutputs)

	// Use workspace path as working directory when available.
	workDir := h.workDir
	_ = env // env carries correlation context; workDir override happens through workspace payload

	// Yolo is always false here — the consolidator only synthesises text
	// from the three review outputs. Tool access caused a double-post bug
	// where Claude proactively ran `gh pr comment` AND the handler posted
	// the output again.
	resp, err := h.backend.Run(ctx, backend.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		WorkDir:      workDir,
		Yolo:         false,
	})
	if err != nil {
		return "", fmt.Errorf("backend: %w", err)
	}

	return resp.Output, nil
}

// prCategoryReviewerLabels maps handler name → human label for the consolidation prompt.
var prCategoryReviewerLabels = []struct {
	key   string
	label string
}{
	{"pr-security", "Security Review"},
	{"pr-concurrency", "Concurrency Review"},
	{"pr-error-handling", "Error Handling Review"},
	{"pr-observability", "Observability Review"},
	{"pr-api-contract", "API Contract Review"},
	{"pr-idempotency", "Idempotency Review"},
	{"pr-testing", "Testing Review"},
	{"pr-integration", "Integration Review"},
	{"pr-performance", "Performance Review"},
	{"pr-data", "Data Integrity Review"},
	{"pr-hygiene", "Code Hygiene Review"},
}

// buildConsolidationPrompt assembles the user prompt for the consolidator AI call.
func buildConsolidationPrompt(params event.WorkflowRequestedPayload, handlerOutputs map[string]string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## PR Review Consolidation Task\n\n")
	fmt.Fprintf(&b, "Source: %s\n\n", params.Source)

	if params.Prompt != "" {
		fmt.Fprintf(&b, "**PR Description / Task Context**:\n%s\n\n", params.Prompt)
	}

	for _, r := range prCategoryReviewerLabels {
		output := handlerOutputs[r.key]
		if output == "" {
			output = "(no output)"
		}
		fmt.Fprintf(&b, "---\n### %s\n\n%s\n\n", r.label, output)
	}

	b.WriteString("---\n\nProduce a single, consolidated PR review comment based on the 11 independent category analyses above.")

	return b.String()
}

// postPRComment posts a comment to the PR using `gh pr comment`.
func postPRComment(ctx context.Context, fullRepo, prNumber, body string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", prNumber,
		"--repo", fullRepo,
		"--body", body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}
