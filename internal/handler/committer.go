package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// CommitterHandler wraps an AIHandler to validate that code changes exist in the
// workspace before delegating to the AI for commit/push. If no changes are
// detected (no uncommitted files AND no unpushed commits), it emits a
// VerdictFail targeting the "develop" phase to trigger the feedback loop —
// forcing the developer to retry instead of silently completing the workflow.
type CommitterHandler struct {
	ai    *AIHandler
	store eventstore.Store
}

// NewCommitterHandler creates a handler that validates workspace changes
// before committing.
func NewCommitterHandler(cfg AIHandlerConfig) *CommitterHandler {
	return &CommitterHandler{
		ai:    NewAIHandler(cfg),
		store: cfg.Store,
	}
}

func (h *CommitterHandler) Name() string             { return h.ai.Name() }
func (h *CommitterHandler) Phase() string             { return h.ai.Phase() }
func (h *CommitterHandler) Subscribes() []event.Type { return nil }

// Handle checks workspace for changes, then delegates to the AI handler.
// Returns VerdictFail if no code changes are detected.
func (h *CommitterHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wsPath, err := resolveWorkspacePath(ctx, h.store, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("committer: resolve workspace: %w", err)
	}

	// Workspace-less workflows (prompt-only) skip the change check.
	if wsPath != "" {
		hasChanges, err := workspaceHasChanges(ctx, wsPath)
		if err != nil {
			return nil, fmt.Errorf("committer: check workspace changes: %w", err)
		}
		if !hasChanges {
			return h.noChangesVerdict(), nil
		}
	}

	return h.ai.Handle(ctx, env)
}

// noChangesVerdict emits VerdictFail targeting the develop phase so the
// aggregate generates feedback and the developer retries.
func (h *CommitterHandler) noChangesVerdict() []event.Envelope {
	return []event.Envelope{
		event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
			Phase:       "develop",
			SourcePhase: h.ai.phase,
			Outcome:     event.VerdictFail,
			Summary:     "no code changes detected in workspace — developer must produce changes before commit",
			Issues: []event.Issue{
				{
					Severity:    "critical",
					Category:    "correctness",
					Description: "The developer phase completed without producing any code changes. The workspace has no uncommitted modifications and no unpushed commits.",
				},
			},
		})).WithSource("handler:" + h.ai.name),
	}
}

// workspaceHasChanges returns true if the workspace has uncommitted changes
// (git status --porcelain) or unpushed commits (git log @{u}..HEAD).
func workspaceHasChanges(ctx context.Context, wsPath string) (bool, error) {
	// Check for uncommitted changes (staged, unstaged, or untracked).
	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = wsPath
	statusOut, err := statusCmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	if len(strings.TrimSpace(string(statusOut))) > 0 {
		return true, nil
	}

	// Check for unpushed commits relative to the upstream tracking branch.
	logCmd := exec.CommandContext(ctx, "git", "log", "@{u}..HEAD", "--oneline")
	logCmd.Dir = wsPath
	logOut, err := logCmd.Output()
	if err != nil {
		// No upstream set — check if there are any local-only commits
		// by comparing to origin/<current-branch>.
		branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = wsPath
		branchOut, branchErr := branchCmd.Output()
		if branchErr != nil {
			return false, nil // can't determine, assume no changes
		}
		branch := strings.TrimSpace(string(branchOut))
		fallbackCmd := exec.CommandContext(ctx, "git", "log", "origin/"+branch+"..HEAD", "--oneline")
		fallbackCmd.Dir = wsPath
		fallbackOut, fallbackErr := fallbackCmd.Output()
		if fallbackErr != nil {
			return false, nil
		}
		return len(strings.TrimSpace(string(fallbackOut))) > 0, nil
	}
	return len(strings.TrimSpace(string(logOut))) > 0, nil
}

// resolveWorkspacePath finds the workspace path from WorkspaceReady events
// in the correlation chain.
func resolveWorkspacePath(ctx context.Context, store eventstore.Store, correlationID string) (string, error) {
	if correlationID == "" {
		return "", nil
	}

	events, err := store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return "", fmt.Errorf("load correlation chain: %w", err)
	}

	for _, e := range events {
		if e.Type == event.WorkspaceReady {
			var p event.WorkspaceReadyPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
			}
			return p.Path, nil
		}
	}
	return "", nil
}
