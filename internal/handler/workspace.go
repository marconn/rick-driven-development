package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/workspace"
)

// WorkspaceHandler provisions a git workspace before AI handlers run.
// It reads workspace params from the correlation chain (WorkflowRequested, and
// optionally context.enrichment), calls workspace.SetupWorkspace, and emits a
// WorkspaceReady event carrying the provisioned path.
//
// Always checks context.enrichment for repo info — harmless if no enrichment
// exists, required for jira-dev where jira-context provides repo from Jira.
type WorkspaceHandler struct {
	store eventstore.Store
	name  string
}

// NewWorkspace creates a WorkspaceHandler with the canonical name "workspace".
func NewWorkspace(d Deps) *WorkspaceHandler {
	return &WorkspaceHandler{
		store: d.Store,
		name:  "workspace",
	}
}

func (h *WorkspaceHandler) Name() string             { return h.name }
func (h *WorkspaceHandler) Subscribes() []event.Type { return nil }

// Handle processes the triggering event, extracts workspace params from the
// correlation chain, and provisions the workspace.
func (h *WorkspaceHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	params, err := h.loadWorkspaceParams(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("workspace handler: load params: %w", err)
	}

	// No workspace params — skip setup, this is a prompt-only workflow.
	if params.Repo == "" && params.Ticket == "" {
		return nil, nil
	}

	// Ticket without repo is a configuration error — the workspace handler
	// cannot provision a branch without knowing which repository to clone.
	// Use jira-dev DAG (which resolves repo from Jira) or pass repo explicitly.
	if params.Repo == "" {
		return nil, fmt.Errorf("workspace handler: ticket %q provided but repo is missing — use dag=jira-dev or pass repo explicitly", params.Ticket)
	}

	// Code-producing workflows require an operator-supplied branch identifier.
	// The historical `rick/<corr8>` fallback was removed because the resulting
	// branch names carried zero intent and surfaced on every PR Rick opened.
	// Operators must now pass one of: ticket (Jira key → jira-dev branches the
	// workspace on the key), branch (free-form name for workspace-dev /
	// develop-only), or use a DAG whose upstream handler emits a branch via
	// ContextEnrichment (github-dev → issue-<N>, pr-* → PR head branch).
	if params.Ticket == "" && params.RepoBranch == "" {
		return nil, fmt.Errorf(
			"workspace handler: code-producing workflow requires a branch identifier — " +
				"pass ticket=<key> for Jira-driven runs (auto-routes to jira-dev), " +
				"pass branch=<name> for free-form runs, " +
				"or select a DAG that derives the branch (github-dev / pr-review / pr-feedback / ci-fix)",
		)
	}

	// First 8 chars of correlation disambiguate the workspace directory when
	// two workflows target the same repo/ticket concurrently. Does NOT touch
	// the branch name — that comes from ticket or RepoBranch verbatim.
	suffix := env.CorrelationID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}

	result, err := workspace.SetupWorkspace(
		params.Repo,
		params.Ticket,
		params.RepoBranch,
		params.BaseBranch,
		suffix,
		env.CorrelationID,
		params.Isolate,
	)
	if err != nil {
		return nil, fmt.Errorf("workspace handler: setup workspace: %w", err)
	}

	readyEvt := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path:     result.Path,
		Branch:   result.Branch,
		Base:     result.Base,
		Isolated: result.Isolated,
	})).WithSource("handler:" + h.name)

	return []event.Envelope{readyEvt}, nil
}

// loadWorkspaceParams reads workspace configuration from the correlation chain.
// Checks both WorkflowRequested and context.enrichment for repo info.
func (h *WorkspaceHandler) loadWorkspaceParams(ctx context.Context, correlationID string) (event.WorkflowRequestedPayload, error) {
	if correlationID == "" {
		return event.WorkflowRequestedPayload{}, nil
	}

	events, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return event.WorkflowRequestedPayload{}, fmt.Errorf("load correlation chain: %w", err)
	}

	var params event.WorkflowRequestedPayload
	var enrichmentRepo, enrichmentTicket, enrichmentBranch string

	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			if err := json.Unmarshal(e.Payload, &params); err != nil {
				return event.WorkflowRequestedPayload{}, fmt.Errorf("unmarshal workflow requested: %w", err)
			}
		case event.ContextEnrichment:
			var ep event.ContextEnrichmentPayload
			if err := json.Unmarshal(e.Payload, &ep); err != nil {
				continue
			}
			for _, item := range ep.Items {
				switch item.Name {
				case "repo":
					enrichmentRepo = item.Reason
				case "ticket":
					enrichmentTicket = item.Reason
				case "branch":
					enrichmentBranch = item.Reason
				}
			}
		}
	}

	// Fall back to enrichment values when WorkflowRequested doesn't specify them.
	// github-dev relies on github-context to emit `ticket` (e.g. "issue-641"),
	// since callers pass only source=gh:owner/repo#N — the ticket field is empty.
	if params.Repo == "" && enrichmentRepo != "" {
		params.Repo = enrichmentRepo
	}
	if params.RepoBranch == "" && enrichmentBranch != "" {
		params.RepoBranch = enrichmentBranch
	}
	if params.Ticket == "" && params.RepoBranch == "" && enrichmentTicket != "" {
		params.Ticket = enrichmentTicket
	}

	return params, nil
}
