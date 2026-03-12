package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/workspace"
)

// PRCleanupHandler removes the isolated workspace directory created by PRWorkspaceHandler.
// It fires after pr-consolidator completes. If the workspace was not isolated, it is a no-op.
// Cleanup is best-effort — the handler never returns an error for a missing directory.
type PRCleanupHandler struct {
	store eventstore.Store
}

// NewPRCleanup creates a PRCleanupHandler from the shared Deps.
func NewPRCleanup(d Deps) *PRCleanupHandler {
	return &PRCleanupHandler{store: d.Store}
}

// Name returns the unique handler identifier.
func (h *PRCleanupHandler) Name() string { return "pr-cleanup" }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *PRCleanupHandler) Subscribes() []event.Type { return nil }

// Handle loads the WorkspaceReady event from the correlation chain and removes
// the isolated workspace directory. Returns no events — side-effect only handler.
func (h *PRCleanupHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wsPayload, err := h.loadWorkspaceReady(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("pr-cleanup: load workspace ready: %w", err)
	}

	// Only remove isolated workspaces — shared repos must never be deleted.
	if wsPayload != nil && wsPayload.Isolated && wsPayload.Path != "" {
		workspace.CleanupIsolatedWorkspace(wsPayload.Path)
	}

	return nil, nil
}

// loadWorkspaceReady scans the correlation chain for the WorkspaceReady event.
// Returns nil if no WorkspaceReady event is found (non-isolated workflows).
func (h *PRCleanupHandler) loadWorkspaceReady(ctx context.Context, correlationID string) (*event.WorkspaceReadyPayload, error) {
	if correlationID == "" {
		return nil, nil
	}

	events, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return nil, fmt.Errorf("load correlation chain: %w", err)
	}

	for _, e := range events {
		if e.Type != event.WorkspaceReady {
			continue
		}
		var p event.WorkspaceReadyPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fmt.Errorf("unmarshal workspace ready: %w", err)
		}
		return &p, nil
	}

	return nil, nil
}
