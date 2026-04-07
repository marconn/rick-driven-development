package handler

import (
	"context"
	"fmt"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// DeveloperHandler wraps an AIHandler to verify that the developer actually
// produced workspace changes. If the underlying AI run completes without
// leaving any diff, uncommitted file, or branch divergence in the workspace,
// it emits a VerdictRendered{fail, phase=develop} so the feedback loop fires
// before reviewer/committer waste their time on hallucinated output.
//
// This post-Handle check mirrors CommitterHandler's pre-Handle check, catching
// the hallucination case (HULI-33546) at the developer itself rather than
// 15 minutes later when the committer runs workspaceHasChanges.
//
// Prompt-only flows (no workspace set) skip the check and pass through.
type DeveloperHandler struct {
	ai    *AIHandler
	store eventstore.Store
}

// NewDeveloperHandler creates a handler that validates workspace changes
// after the AI has completed its run.
func NewDeveloperHandler(cfg AIHandlerConfig) *DeveloperHandler {
	return &DeveloperHandler{
		ai:    NewAIHandler(cfg),
		store: cfg.Store,
	}
}

func (h *DeveloperHandler) Name() string             { return h.ai.Name() }
func (h *DeveloperHandler) Phase() string            { return h.ai.Phase() }
func (h *DeveloperHandler) Subscribes() []event.Type { return nil }

// Handle delegates to the AI handler and then post-checks the workspace.
// If the AI returned successfully but the workspace has no changes, a
// VerdictRendered{fail, phase=develop} is appended to the returned events
// so the feedback loop fires immediately.
func (h *DeveloperHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	aiEvents, err := h.ai.Handle(ctx, env)
	if err != nil {
		// AI itself failed — propagate as-is so PersonaRunner emits PersonaFailed.
		// No workspace check needed: the AI didn't claim success.
		return aiEvents, err
	}

	ws, err := resolveWorkspace(ctx, h.store, env.CorrelationID)
	if err != nil {
		// Best-effort: if we can't resolve the workspace we trust the AI output.
		// This can happen if the event store is temporarily unavailable; we'd
		// rather emit a false-negative than block legitimate work.
		return aiEvents, nil
	}
	if ws.Path == "" {
		// Prompt-only flow — no workspace to inspect.
		return aiEvents, nil
	}

	hasChanges, err := workspaceHasChanges(ctx, ws.Path, ws.Base)
	if err != nil {
		// Git check failed (e.g., invalid path, git not installed). Treat as
		// best-effort: don't penalise the run for an infrastructure failure.
		return aiEvents, nil
	}
	if hasChanges {
		return aiEvents, nil
	}

	// The developer ran successfully but left no detectable changes in the
	// workspace. This is the hallucination signature: the LLM produced plausible
	// prose ("I created the file…") without invoking file-editing tools.
	verdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: h.ai.phase,
		Outcome:     event.VerdictFail,
		Summary:     "developer produced no workspace changes — likely hallucinated output (no uncommitted files, no unpushed commits, no branch divergence)",
		Issues: []event.Issue{
			{
				Severity: "critical",
				Category: "correctness",
				Description: fmt.Sprintf(
					"The developer phase at workspace %s completed without leaving any detectable changes. "+
						"This usually means the AI described work in prose without actually invoking "+
						"file-editing tools, or the tools failed silently.",
					ws.Path,
				),
			},
		},
	})).WithSource("handler:" + h.ai.name)

	return append(aiEvents, verdict), nil
}
