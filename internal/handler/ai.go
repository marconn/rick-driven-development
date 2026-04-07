package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// AIHandler is the base handler for AI-powered workflow phases.
// It loads context from the event store, builds prompts via the persona system,
// calls an AI backend, and returns AIRequestSent + AIResponseReceived events.
type AIHandler struct {
	name     string
	phase    string            // workflow phase this handler processes
	persona  string            // persona name for system prompt
	backend  backend.Backend   // AI provider (claude, gemini)
	store    eventstore.Store  // for loading workflow context
	registry *persona.Registry // for system prompt lookup
	builder  *persona.PromptBuilder
	workDir   string   // working directory for backend execution
	yolo      bool     // skip permission checks
	plainText bool     // skip structured JSON extraction, store raw text
}

// AIHandlerConfig configures an AI handler.
type AIHandlerConfig struct {
	Name    string            // handler name (e.g., "researcher", "developer")
	Phase   string            // workflow phase (e.g., "research", "develop")
	Persona string            // persona name for system prompt
	Backend backend.Backend   // AI backend to call
	Store   eventstore.Store  // event store for context loading
	Personas *persona.Registry // persona registry for system prompts
	Builder *persona.PromptBuilder
	WorkDir   string  // working directory for backend execution
	Yolo      bool    // skip permission checks
	PlainText bool    // skip structured JSON extraction, store raw text
}

// NewAIHandler creates an AI handler with the given configuration.
func NewAIHandler(cfg AIHandlerConfig) *AIHandler {
	return &AIHandler{
		name:     cfg.Name,
		phase:    cfg.Phase,
		persona:  cfg.Persona,
		backend:  cfg.Backend,
		store:    cfg.Store,
		registry: cfg.Personas,
		builder:  cfg.Builder,
		workDir:   cfg.WorkDir,
		yolo:      cfg.Yolo,
		plainText: cfg.PlainText,
	}
}

func (h *AIHandler) Name() string  { return h.name }
func (h *AIHandler) Phase() string { return h.phase }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *AIHandler) Subscribes() []event.Type { return nil }

// Handle processes a triggering event by:
// 1. Loading workflow context from the event store (previous outputs, feedback)
// 2. Building system + user prompts via the persona system
// 3. Calling the AI backend
// 4. Returning AIRequestSent + AIResponseReceived events
func (h *AIHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	pctx, err := h.buildPromptContext(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("handler %s: build context: %w", h.name, err)
	}

	systemPrompt, err := h.registry.LoadSystemPrompt(h.persona)
	if err != nil {
		return nil, fmt.Errorf("handler %s: load system prompt: %w", h.name, err)
	}

	userPrompt, err := h.builder.Build(h.phase, pctx)
	if err != nil {
		return nil, fmt.Errorf("handler %s: build prompt: %w", h.name, err)
	}

	// AIRequestSent
	promptHash := sha256Short(userPrompt)
	reqEvt := event.New(event.AIRequestSent, 1, event.MustMarshal(event.AIRequestPayload{
		Phase:      h.phase,
		Backend:    h.backend.Name(),
		Persona:    h.persona,
		PromptHash: promptHash,
	})).WithSource("handler:" + h.name)

	// Use workspace path as working directory when available (overrides static workDir).
	workDir := h.workDir
	if pctx.WorkspacePath != "" {
		workDir = pctx.WorkspacePath
	}

	// Call backend
	resp, err := h.backend.Run(ctx, backend.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		WorkDir:      workDir,
		Yolo:         h.yolo,
	})
	if err != nil {
		return nil, fmt.Errorf("handler %s: backend: %w", h.name, err)
	}

	// Try structured output extraction (skip for plain-text handlers)
	var output json.RawMessage
	var structured bool
	if h.plainText {
		output, _ = json.Marshal(resp.Output)
	} else {
		output, structured = marshalOutput(resp.Output)
	}

	// AIResponseReceived
	respEvt := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Phase:      h.phase,
		Backend:    h.backend.Name(),
		TokensUsed: resp.TokensUsed,
		DurationMS: resp.Duration.Milliseconds(),
		Structured: structured,
		Output:     output,
	})).WithSource("handler:" + h.name)

	return []event.Envelope{reqEvt, respEvt}, nil
}

// buildPromptContext loads workflow state from the event store and constructs
// a PromptContext for prompt building. It reads the correlation chain to find
// previous phase outputs and any feedback for the current phase.
func (h *AIHandler) buildPromptContext(ctx context.Context, env event.Envelope) (persona.PromptContext, error) {
	if env.CorrelationID == "" {
		return persona.PromptContext{}, nil
	}

	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return persona.PromptContext{}, fmt.Errorf("load correlation chain: %w", err)
	}

	pctx := persona.PromptContext{
		Outputs: make(map[string]string),
	}

	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			var p event.WorkflowRequestedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.Task = p.Prompt
				pctx.Source = p.Source
			}

		case event.AIResponseReceived:
			var p event.AIResponsePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.Outputs[p.Phase] = unmarshalOutput(p.Output, p.Structured)
			}

		case event.FeedbackGenerated:
			var p event.FeedbackGeneratedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.TargetPhase == h.phase {
				pctx.Feedback = formatFeedback(p)
				pctx.Iteration = p.Iteration
			}

		case event.WorkspaceReady:
			var p event.WorkspaceReadyPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.WorkspacePath = p.Path
				if p.Branch != "" {
					pctx.Ticket = p.Branch
				}
				if p.Base != "" {
					pctx.BaseBranch = p.Base
				}
			}

		case event.ContextCodebase:
			var p event.ContextCodebasePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.Codebase = formatCodebaseContext(p)
			}

		case event.ContextSchema:
			var p event.ContextSchemaPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.Schema = formatSchemaContext(p)
			}

		case event.ContextGit:
			var p event.ContextGitPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.GitContext = formatGitContext(p)
			}

		case event.ContextEnrichment:
			var p event.ContextEnrichmentPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pctx.Enrichments = append(pctx.Enrichments, p)
			}

		case event.OperatorGuidance:
			var p event.OperatorGuidancePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				if p.Target == "" || p.Target == h.phase {
					if pctx.Feedback != "" {
						pctx.Feedback += "\n\n"
					}
					pctx.Feedback += "## Operator Guidance\n" + p.Content
				}
			}
		}
	}

	return pctx, nil
}

// marshalOutput converts LLM text output to JSON for AIResponsePayload.Output.
// If ExtractJSON finds structured JSON, returns that with structured=true.
// Otherwise, marshals the raw text as a JSON string.
func marshalOutput(text string) (json.RawMessage, bool) {
	if extracted, ok := backend.ExtractJSON(text); ok {
		return extracted, true
	}
	raw, _ := json.Marshal(text)
	return raw, false
}

// unmarshalOutput extracts the text content from AIResponsePayload.Output.
func unmarshalOutput(output json.RawMessage, structured bool) string {
	if structured {
		return string(output)
	}
	var text string
	if err := json.Unmarshal(output, &text); err == nil {
		return text
	}
	return string(output)
}

// formatFeedback converts a FeedbackGeneratedPayload into a readable string
// for inclusion in prompt templates.
func formatFeedback(p event.FeedbackGeneratedPayload) string {
	var b strings.Builder
	if p.Summary != "" {
		b.WriteString(p.Summary)
		b.WriteString("\n\n")
	}
	for _, issue := range p.Issues {
		fmt.Fprintf(&b, "- [%s/%s] %s", issue.Severity, issue.Category, issue.Description)
		if issue.File != "" {
			fmt.Fprintf(&b, " (%s", issue.File)
			if issue.Line > 0 {
				fmt.Fprintf(&b, ":%d", issue.Line)
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// sha256Short returns the first 12 hex chars of the SHA-256 hash.
func sha256Short(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// formatCodebaseContext renders a codebase snapshot as human-readable text.
func formatCodebaseContext(p event.ContextCodebasePayload) string {
	var b strings.Builder
	if p.Language != "" {
		fmt.Fprintf(&b, "Language: %s", p.Language)
		if p.Framework != "" {
			fmt.Fprintf(&b, " (%s)", p.Framework)
		}
		b.WriteString("\n\n")
	}

	if len(p.Tree) > 0 {
		b.WriteString("File tree:\n")
		for _, e := range p.Tree {
			fmt.Fprintf(&b, "  %s (%d bytes)\n", e.Path, e.Size)
		}
		b.WriteString("\n")
	}

	for _, f := range p.Files {
		fmt.Fprintf(&b, "--- %s ---\n%s\n\n", f.Path, f.Content)
	}

	return strings.TrimSpace(b.String())
}

// formatSchemaContext renders schema definitions as human-readable text.
func formatSchemaContext(p event.ContextSchemaPayload) string {
	var b strings.Builder
	writeSnaps := func(label string, snaps []event.FileSnap) {
		if len(snaps) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n\n", label)
		for _, f := range snaps {
			fmt.Fprintf(&b, "--- %s ---\n%s\n\n", f.Path, f.Content)
		}
	}
	writeSnaps("Protocol Buffers", p.Proto)
	writeSnaps("SQL Schemas", p.SQL)
	writeSnaps("GraphQL Schemas", p.GraphQL)
	return strings.TrimSpace(b.String())
}

// formatGitContext renders git state as human-readable text.
func formatGitContext(p event.ContextGitPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Branch: %s (HEAD: %s)\n", p.Branch, p.HEAD)

	if len(p.RecentLog) > 0 {
		b.WriteString("\nRecent commits:\n")
		for _, l := range p.RecentLog {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}

	if p.DiffStat != "" {
		fmt.Fprintf(&b, "\nDiff from base:\n%s\n", p.DiffStat)
	}

	if len(p.ModifiedFiles) > 0 {
		b.WriteString("\nModified files:\n")
		for _, f := range p.ModifiedFiles {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}

	return strings.TrimSpace(b.String())
}
