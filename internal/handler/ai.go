package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// AIHandler is the base handler for AI-powered workflow phases.
// It loads context from the event store, builds prompts via the persona system,
// calls an AI backend, and emits AIRequestSent + AIResponseReceived events.
//
// AIRequestSent is persisted+published BEFORE the backend call so a hung
// subprocess leaves a forensic trail. Without this, an indefinitely-blocked
// backend.Run would make the event log look like the handler was never
// dispatched, masking dispatch-vs-backend bugs (see incident
// 2d8b4b99-f8e8-4af4-917c-9102fa6ca33a).
type AIHandler struct {
	name           string            // handler name, also the persona key used in events and prompt-output reuse
	persona        string            // persona name for system prompt
	template       string            // user-prompt template file name (defaults to handler name when empty)
	backend        backend.Backend   // AI provider (claude, gemini)
	store          eventstore.Store  // for loading workflow context + inline AIRequestSent persist
	bus            eventbus.Bus      // optional: publishes AIRequestSent before backend.Run
	registry       *persona.Registry // for system prompt lookup
	builder        *persona.PromptBuilder
	workDir        string        // working directory for backend execution
	yolo           bool          // skip permission checks
	plainText      bool          // skip structured JSON extraction, store raw text
	backendTimeout time.Duration // hard cap on backend.Run; 0 disables
	effort         string        // Claude --effort override; empty = claude.go default ("high")
	// resumeInFeedbackLoop opts this handler into backend session resume on a
	// feedback-driven re-run: instead of re-sending the full context prompt,
	// it resumes the prior CLI session and sends only the feedback delta. Set
	// only for the developer (the sole code-producing persona that re-runs in
	// the loop) and only when RICK_ENABLE_SESSION_RESUME is set. See
	// resolveResume for the strict eligibility gate.
	resumeInFeedbackLoop bool
}

// AIHandlerConfig configures an AI handler.
type AIHandlerConfig struct {
	Name    string // handler name, also the persona key (e.g., "developer", "reviewer")
	Persona string // persona name for system prompt
	// Template names the user-prompt template to load (e.g., "develop",
	// "review", "pr-category-review"). Empty means use Name. Multiple
	// handlers can share a template — the 12 pr-* category reviewers all
	// resolve to "pr-category-review", and the historical names (develop,
	// research, commit) are kept so the embedded markdown files don't have
	// to be renamed alongside the handler-name collapse.
	Template string
	Backend  backend.Backend  // AI backend to call
	Store    eventstore.Store // event store for context loading + inline AIRequestSent persist
	// Bus is optional. When non-nil, AIRequestSent is persisted to the
	// persona-scoped aggregate AND published on the bus before backend.Run
	// fires, so a hung backend leaves a forensic trail. Tests that don't
	// care about observability can omit Bus and the handler falls back to
	// returning AIRequestSent alongside the response.
	Bus       eventbus.Bus
	Personas  *persona.Registry // persona registry for system prompts
	Builder   *persona.PromptBuilder
	WorkDir   string // working directory for backend execution
	Yolo      bool   // skip permission checks
	PlainText bool   // skip structured JSON extraction, store raw text
	// BackendTimeout caps how long backend.Run may block. Zero means no
	// timeout (legacy behavior). Production should always set this so a
	// wedged claude/gemini subprocess fails fast instead of hanging.
	BackendTimeout time.Duration
	// Effort overrides the Claude CLI --effort reasoning level for this
	// handler. Valid values: low / medium / high / xhigh / max. Empty falls
	// through to claude.go's "high" default. No-op on Gemini / Codex
	// backends — they have no equivalent flag.
	Effort string
	// ResumeInFeedbackLoop opts this handler into backend session resume on a
	// feedback-driven re-run (see AIHandler.resumeInFeedbackLoop). Default
	// false. handlers.go sets it only for the developer and only when
	// RICK_ENABLE_SESSION_RESUME is set.
	ResumeInFeedbackLoop bool
}

// defaultTemplate returns the prompt-template file name for a handler. The
// embedded markdown files are still organized by phase verb (develop.md,
// research.md) and by shared-template kind (pr-category-review.md for the 12
// pr-* reviewers); this map keeps the verb-keyed filenames stable across the
// handler-name collapse so we don't have to rename embedded markdown
// alongside Go-side code. Unknown handler names fall back to the name itself,
// which lets new handlers ship a `phases/<name>.md` template without touching
// this map.
func defaultTemplate(handlerName string) string {
	switch handlerName {
	case "developer":
		return "develop"
	case "researcher":
		return "research"
	case "committer":
		return "commit"
	case "reviewer":
		return "review"
	case "feedback-analyzer":
		return "feedback-analyze"
	case "qa-analyzer":
		return "qa-analyze"
	case "pr-replier":
		return "pr-reply"
	case "pr-security", "pr-concurrency", "pr-error-handling",
		"pr-observability", "pr-api-contract", "pr-idempotency",
		"pr-testing", "pr-integration", "pr-performance",
		"pr-data", "pr-hygiene", "pr-vendor-resilience",
		"pr-docs-concordance":
		return "pr-category-review"
	}
	return handlerName
}

// NewAIHandler creates an AI handler with the given configuration.
func NewAIHandler(cfg AIHandlerConfig) *AIHandler {
	tmpl := cfg.Template
	if tmpl == "" {
		tmpl = defaultTemplate(cfg.Name)
	}
	return &AIHandler{
		name:                 cfg.Name,
		persona:              cfg.Persona,
		template:             tmpl,
		backend:              cfg.Backend,
		store:                cfg.Store,
		bus:                  cfg.Bus,
		registry:             cfg.Personas,
		builder:              cfg.Builder,
		workDir:              cfg.WorkDir,
		yolo:                 cfg.Yolo,
		plainText:            cfg.PlainText,
		backendTimeout:       cfg.BackendTimeout,
		effort:               cfg.Effort,
		resumeInFeedbackLoop: cfg.ResumeInFeedbackLoop,
	}
}

func (h *AIHandler) Name() string { return h.name }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *AIHandler) Subscribes() []event.Type { return nil }

// Handle processes a triggering event by:
//  1. Loading workflow context from the event store (previous outputs, feedback)
//  2. Building system + user prompts via the persona system
//  3. Persisting+publishing AIRequestSent (so a hung handler still leaves a trail)
//  4. Persisting+publishing AIRequestStarted immediately before backend.Run
//     (distinguishes pre-spawn stalls from subprocess-side hangs)
//  5. Calling the AI backend (with optional timeout)
//  6. Returning AIResponseReceived (or a bundle including request/started events
//     when no Bus is wired)
func (h *AIHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	pctx, autoRetryAttempt, resume, err := h.buildPromptContext(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("handler %s: build context: %w", h.name, err)
	}

	systemPrompt, err := h.registry.LoadSystemPrompt(h.persona)
	if err != nil {
		return nil, fmt.Errorf("handler %s: load system prompt: %w", h.name, err)
	}

	// Session resume: on an eligible feedback re-run, continue the prior CLI
	// session and send only the feedback delta instead of rebuilding the full
	// context prompt. resumeSessionID is "" when not resuming (the common case),
	// which leaves Request.SessionID empty and the prompt path unchanged.
	resumeSessionID := h.resolveResume(pctx, autoRetryAttempt, resume)

	var userPrompt string
	if resumeSessionID != "" {
		userPrompt = buildResumePrompt(pctx)
	} else {
		userPrompt, err = h.builder.Build(h.template, pctx)
		if err != nil {
			return nil, fmt.Errorf("handler %s: build prompt: %w", h.name, err)
		}
	}

	// Build AIRequestSent. When a Bus + Store + correlation are available we
	// persist+publish it inline BEFORE calling backend.Run, so a hung
	// subprocess still leaves a forensic trail in the event log. Otherwise
	// we fall back to bundling it with the response (legacy behavior used
	// by tests that don't wire a bus).
	promptHash := sha256Short(userPrompt)
	reqEvt := event.New(event.AIRequestSent, 1, event.MustMarshal(event.AIRequestPayload{
		Persona:    h.name,
		Backend:    h.backend.Name(),
		PromptHash: promptHash,
	})).WithSource("handler:" + h.name)

	emittedInline := false
	if h.bus != nil && h.store != nil && env.CorrelationID != "" {
		if persisted, ok := h.persistRequestEvent(ctx, env, reqEvt); ok {
			reqEvt = persisted
			emittedInline = true
		}
	}

	// Use workspace path as working directory when available (overrides static workDir).
	workDir := h.workDir
	if pctx.WorkspacePath != "" {
		workDir = pctx.WorkspacePath
	}

	// Wrap with a backend timeout when configured. This is the only escape
	// hatch for a wedged claude/gemini subprocess — without it, cmd.Run()
	// blocks until the per-correlation context is cancelled (i.e., the
	// operator manually cancels the workflow).
	backendCtx := ctx
	if h.backendTimeout > 0 {
		var cancel context.CancelFunc
		backendCtx, cancel = context.WithTimeout(ctx, h.backendTimeout)
		defer cancel()
	}

	// Pin this persona to a specific inner backend when the underlying
	// backend is a RoundRobin. Keeps reviewer/qa on the same CLI across
	// all iterations of a feedback loop so the developer isn't chasing
	// three different backends' opinions on three different runs.
	// Harmless for non-rotating backends — the key is simply ignored.
	//
	// Auto-retry rotation: when the engine has fired
	// WorkflowRetried{FromPhase==h.name, Automatic==true} earlier in this
	// correlation, the retry attempt count is passed as a deterministic
	// rotation offset so RoundRobin picks a strictly different inner
	// backend for the retry. We use WithRotateOffset rather than baking the
	// attempt into the key because FNV % n with n=3 has a 1/3 collision
	// probability — an operator on their last auto-retry budget can't
	// afford to land back on the CLI that just silently stalled. Offset of
	// N shifts to slot (base+N) mod n, guaranteeing a different slot for
	// N ∈ [1, n-1]. Single-backend deployments are unaffected (n=1 absorbs
	// any offset).
	if env.CorrelationID != "" {
		backendCtx = backend.WithStickyKey(backendCtx, env.CorrelationID+":"+h.persona)
		backendCtx = backend.WithRotateOffset(backendCtx, autoRetryAttempt)
	}

	// AIRequestStarted: marks the exact moment before backend.Run is invoked
	// (subprocess exec). Paired with AIRequestSent, the gap between them
	// measures handler pre-flight; the gap between AIRequestStarted and
	// AIResponseReceived measures subprocess runtime. Operators diagnose
	// pre-spawn vs. subprocess-side stalls by which gap dominates.
	startEvt := event.New(event.AIRequestStarted, 1, event.MustMarshal(event.AIRequestStartedPayload{
		Persona:       h.name,
		Backend:       h.backend.Name(),
		PromptHash:    promptHash,
		SpawnUnixNano: time.Now().UnixNano(),
	})).WithSource("handler:" + h.name)
	startEmittedInline := false
	if emittedInline {
		if persisted, ok := h.persistRequestEvent(ctx, env, startEvt); ok {
			startEvt = persisted
			startEmittedInline = true
		}
	}

	// Call backend
	resp, err := h.backend.Run(backendCtx, backend.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		WorkDir:      workDir,
		Yolo:         h.yolo,
		Effort:       h.effort,
		SessionID:    resumeSessionID, // "" = fresh session (the common path)
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
		Persona:    h.name,
		Backend:    h.backend.Name(),
		TokensUsed: resp.TokensUsed,
		DurationMS: resp.Duration.Milliseconds(),
		Structured: structured,
		Output:     output,
		SessionID:  resp.SessionID, // captured for a later feedback-loop resume
	})).WithSource("handler:" + h.name)

	// Return events not already persisted inline. emittedInline implies
	// reqEvt was published; startEmittedInline implies startEvt was published.
	// When the Bus isn't wired (tests), bundle everything with the response.
	if emittedInline {
		if startEmittedInline {
			return []event.Envelope{respEvt}, nil
		}
		return []event.Envelope{startEvt, respEvt}, nil
	}
	return []event.Envelope{reqEvt, startEvt, respEvt}, nil
}

// persistRequestEvent appends AIRequestSent to the persona-scoped aggregate
// and publishes it on the bus before backend.Run is called. Uses the same
// load-current-version-then-append loop as engine.resultPersister so it
// composes safely with feedback-loop iterations (where the same persona
// aggregate already has events from prior cycles). Returns the versioned
// envelope and true on success; on failure logs nothing and returns ok=false
// so Handle falls back to the legacy "return alongside response" path —
// observability is best-effort, never fail the handler over it.
func (h *AIHandler) persistRequestEvent(ctx context.Context, env event.Envelope, reqEvt event.Envelope) (event.Envelope, bool) {
	aggregateID := env.CorrelationID + ":persona:" + h.name
	staged := reqEvt.WithCorrelation(env.CorrelationID).WithCausation(env.ID)

	const maxAttempts = 3
	for range maxAttempts {
		currentVersion := 0
		if existing, loadErr := h.store.Load(ctx, aggregateID); loadErr == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		versioned := staged.WithAggregate(aggregateID, currentVersion+1)
		if appendErr := h.store.Append(ctx, aggregateID, currentVersion, []event.Envelope{versioned}); appendErr == nil {
			_ = h.bus.Publish(ctx, versioned)
			return versioned, true
		}
	}
	return reqEvt, false
}

// resumeInfo carries the latest backend session a persona produced earlier in
// this correlation. The developer feedback loop uses it to continue that
// session instead of re-sending the full context prompt. Zero value (empty
// sessionID) means "no resumable session".
type resumeInfo struct {
	sessionID string
	backend   string // the CLI that created the session; a session id is only valid there
}

// resolveResume decides whether this run should resume a prior backend session
// rather than start fresh, and returns the session id to resume ("" = don't).
//
// Resume trades a smaller prompt (feedback delta only — the session already
// holds the task + codebase context) for a dependency on the CLI's persisted
// session. It is gated hard because a wrong resume silently feeds the model
// stale context. ALL must hold:
//
//   - the handler opted in (developer only, behind RICK_ENABLE_SESSION_RESUME);
//   - a prior session exists for this persona;
//   - it was created by the SAME backend we're about to call (a codex thread id
//     is meaningless to claude, and vice versa);
//   - no auto-retry has fired for this persona — auto-retry deliberately
//     ROTATES the backend (WithRotateOffset), so the recorded session id no
//     longer matches the CLI that will run. This also gives a free fallback:
//     if a resumed run fails transiently, the engine's auto-retry re-runs it
//     with rotation on → resume disabled → full context is rebuilt;
//   - this is a feedback-driven re-run (feedback present for this persona) —
//     the only case where the prior session is the right thing to continue;
//   - the backend is a single concrete CLI, not a RoundRobin composite, whose
//     per-call backend choice (and thus session attribution) isn't pinned here.
func (h *AIHandler) resolveResume(pctx persona.PromptContext, autoRetryAttempt int, r resumeInfo) string {
	if !h.resumeInFeedbackLoop {
		return ""
	}
	if r.sessionID == "" || r.backend != h.backend.Name() {
		return ""
	}
	if autoRetryAttempt != 0 || pctx.Feedback == "" {
		return ""
	}
	if _, isRoundRobin := h.backend.(*backend.RoundRobin); isRoundRobin {
		return ""
	}
	return r.sessionID
}

// buildResumePrompt produces the minimal user prompt for a resumed session:
// only the new feedback delta, since the session already carries the task,
// codebase context, and the persona's own prior work. This is where the token
// saving comes from — the full context prompt (codebase snapshot, schema, git,
// prior outputs) is NOT re-sent.
func buildResumePrompt(pctx persona.PromptContext) string {
	var b strings.Builder
	b.WriteString("Continue from your previous session in this same workspace. ")
	b.WriteString("Your earlier work is already applied on disk; do not redo it from scratch. ")
	b.WriteString("Address the following review feedback and update the code accordingly, ")
	b.WriteString("then report your changes in the same format as before.\n\n")
	b.WriteString(pctx.Feedback)
	return b.String()
}

// buildPromptContext loads workflow state from the event store and constructs
// a PromptContext for prompt building. It reads the correlation chain to find
// previous phase outputs and any feedback for the current phase. The second
// return value is the number of automatic WorkflowRetried events that have
// already targeted this handler earlier in the correlation — callers fold it
// into the backend sticky key so RoundRobin rotations pick a fresh CLI on
// retry instead of re-running the same one that just silently stalled. The
// third carries the latest resumable session for this persona (see resumeInfo).
func (h *AIHandler) buildPromptContext(ctx context.Context, env event.Envelope) (persona.PromptContext, int, resumeInfo, error) {
	if env.CorrelationID == "" {
		return persona.PromptContext{}, 0, resumeInfo{}, nil
	}

	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return persona.PromptContext{}, 0, resumeInfo{}, fmt.Errorf("load correlation chain: %w", err)
	}

	pctx := persona.PromptContext{
		Outputs: make(map[string]string),
	}
	autoRetryAttempt := 0
	var resume resumeInfo

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
				pctx.Outputs[p.Persona] = unmarshalOutput(p.Output, p.Structured)
				// Track the latest session THIS persona opened so a feedback
				// re-run can resume it. Events are in causal order, so the last
				// write wins. Backend is recorded alongside because a session
				// id is only valid on the CLI that created it.
				if p.Persona == h.name && p.SessionID != "" {
					resume = resumeInfo{sessionID: p.SessionID, backend: p.Backend}
				}
			}

		case event.FeedbackGenerated:
			var p event.FeedbackGeneratedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.TargetPersona == h.name {
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
				if p.Target == "" || p.Target == h.name {
					if pctx.Feedback != "" {
						pctx.Feedback += "\n\n"
					}
					pctx.Feedback += "## Operator Guidance\n" + p.Content
				}
			}

		case event.WorkflowRetried:
			var p event.WorkflowRetriedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				if p.Automatic && p.FromPhase == h.name {
					autoRetryAttempt++
				}
			}
		}
	}

	return pctx, autoRetryAttempt, resume, nil
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
	// Append the unfiltered diagnostic tail when the upstream verdict carried
	// one. The 2026-04-29 incident motivated this: when filterDockerNoise (or
	// any other human-readability filter) collapses Issue.Description to a
	// near-empty body, the developer still needs the raw test/lint output to
	// act on. RawDiagnostics is forensics-grade: untouched bytes from the
	// failure stream.
	if rd := strings.TrimSpace(p.RawDiagnostics); rd != "" {
		b.WriteString("\n### Raw diagnostics\n")
		b.WriteString(rd)
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
