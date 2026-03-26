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
// detected (no uncommitted files, no unpushed commits, AND no divergence from
// the base branch), it emits a VerdictFail targeting the "develop" phase to
// trigger the feedback loop — forcing the developer to retry instead of
// silently completing the workflow.
type CommitterHandler struct {
	ai    *AIHandler
	store eventstore.Store
}

// workspaceInfo holds the resolved workspace path and base branch.
type workspaceInfo struct {
	Path string
	Base string
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
	ws, err := resolveWorkspace(ctx, h.store, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("committer: resolve workspace: %w", err)
	}

	// Workspace-less workflows (prompt-only) skip the change check.
	if ws.Path != "" {
		hasChanges, err := workspaceHasChanges(ctx, ws.Path, ws.Base)
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
// (git status --porcelain), unpushed commits (git log @{u}..HEAD), or
// divergence from the base branch (git log origin/<base>..HEAD). The base
// branch check catches the case where the developer committed and pushed
// autonomously — the feature branch has no local/unpushed changes but has
// diverged from the base.
func workspaceHasChanges(ctx context.Context, wsPath, baseBranch string) (bool, error) {
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
		if len(strings.TrimSpace(string(fallbackOut))) > 0 {
			return true, nil
		}
	} else if len(strings.TrimSpace(string(logOut))) > 0 {
		return true, nil
	}

	// Check for divergence from the base branch. This catches the case where
	// the developer committed and pushed to the feature branch autonomously —
	// git status is clean and @{u}..HEAD is empty, but the feature branch has
	// commits that the base branch doesn't.
	if baseBranch != "" {
		divergeCmd := exec.CommandContext(ctx, "git", "log", "origin/"+baseBranch+"..HEAD", "--oneline")
		divergeCmd.Dir = wsPath
		divergeOut, divergeErr := divergeCmd.Output()
		if divergeErr == nil && len(strings.TrimSpace(string(divergeOut))) > 0 {
			return true, nil
		}
	}

	return false, nil
}

// resolveWorkspace finds the workspace path and base branch from WorkspaceReady
// events in the correlation chain.
func resolveWorkspace(ctx context.Context, store eventstore.Store, correlationID string) (workspaceInfo, error) {
	if correlationID == "" {
		return workspaceInfo{}, nil
	}

	events, err := store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return workspaceInfo{}, fmt.Errorf("load correlation chain: %w", err)
	}

	for _, e := range events {
		if e.Type == event.WorkspaceReady {
			var p event.WorkspaceReadyPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				continue
			}
			return workspaceInfo{Path: p.Path, Base: p.Base}, nil
		}
	}
	return workspaceInfo{}, nil
}
