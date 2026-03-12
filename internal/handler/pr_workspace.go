package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/workspace"
)

// prSourcePattern matches the "gh:owner/repo#123" format used in Source fields.
var prSourcePattern = regexp.MustCompile(`^gh:([^/]+/[^#]+)#(\d+)$`)

// PRWorkspaceHandler provisions an isolated git workspace for pull request review.
// It fires on WorkflowStarted for "pr-review", extracts the PR number and repo from
// the Source field, fetches the PR branch via `gh`, and calls SetupWorkspace in
// isolated mode so multiple concurrent reviews never share a directory.
type PRWorkspaceHandler struct {
	store eventstore.Store
}

// NewPRWorkspace creates a PRWorkspaceHandler from the shared Deps.
func NewPRWorkspace(d Deps) *PRWorkspaceHandler {
	return &PRWorkspaceHandler{store: d.Store}
}

// Name returns the unique handler identifier.
func (h *PRWorkspaceHandler) Name() string { return "pr-workspace" }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *PRWorkspaceHandler) Subscribes() []event.Type { return nil }

// Handle processes the WorkflowStarted event, provisions an isolated workspace
// for the PR branch, and emits a WorkspaceReady event.
func (h *PRWorkspaceHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	params, err := h.loadWorkflowRequested(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("pr-workspace: load params: %w", err)
	}

	fullRepo, prNumber, err := parsePRSource(params.Source)
	if err != nil {
		return nil, fmt.Errorf("pr-workspace: parse source %q: %w", params.Source, err)
	}

	headBranch, baseBranch, err := fetchPRBranches(ctx, fullRepo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("pr-workspace: fetch PR branches: %w", err)
	}

	// Use first 8 chars of correlation as suffix to avoid workspace collisions
	// when multiple PR reviews run concurrently.
	suffix := env.CorrelationID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}

	// repo is the last path segment (e.g., "owner/myrepo" → "myrepo").
	// SetupWorkspace resolves the full path from $RICK_REPOS_PATH/<repo>.
	repoName := repoNameFromFull(fullRepo)

	result, err := workspace.SetupWorkspace(repoName, headBranch, "", baseBranch, suffix, true)
	if err != nil {
		return nil, fmt.Errorf("pr-workspace: setup workspace: %w", err)
	}

	readyEvt := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path:     result.Path,
		Branch:   result.Branch,
		Base:     result.Base,
		Isolated: result.Isolated,
	})).WithSource("handler:pr-workspace")

	return []event.Envelope{readyEvt}, nil
}

// loadWorkflowRequested reads WorkflowRequested from the correlation chain.
func (h *PRWorkspaceHandler) loadWorkflowRequested(ctx context.Context, correlationID string) (event.WorkflowRequestedPayload, error) {
	if correlationID == "" {
		return event.WorkflowRequestedPayload{}, nil
	}

	events, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return event.WorkflowRequestedPayload{}, fmt.Errorf("load correlation chain: %w", err)
	}

	for _, e := range events {
		if e.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return event.WorkflowRequestedPayload{}, fmt.Errorf("unmarshal workflow requested: %w", err)
		}
		return p, nil
	}

	return event.WorkflowRequestedPayload{}, nil
}

// parsePRSource extracts the full repo ("owner/repo") and PR number string
// from a source field like "gh:owner/repo#123".
func parsePRSource(source string) (fullRepo, prNumber string, err error) {
	m := prSourcePattern.FindStringSubmatch(source)
	if m == nil {
		return "", "", fmt.Errorf("expected format gh:owner/repo#N, got %q", source)
	}
	return m[1], m[2], nil
}

// repoNameFromFull returns the repository name portion from "owner/repo".
func repoNameFromFull(fullRepo string) string {
	parts := strings.SplitN(fullRepo, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return fullRepo
}

// prBranchInfo holds the branch names from a PR.
type prBranchInfo struct {
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
}

// fetchPRBranches runs `gh pr view` to get the head and base branch names for a PR.
func fetchPRBranches(ctx context.Context, fullRepo, prNumber string) (headBranch, baseBranch string, err error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber,
		"--repo", fullRepo,
		"--json", "headRefName,baseRefName")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("gh pr view %s: %w", prNumber, err)
	}

	var info prBranchInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return "", "", fmt.Errorf("unmarshal gh pr view output: %w", err)
	}

	if info.HeadRefName == "" {
		return "", "", fmt.Errorf("headRefName is empty for PR %s", prNumber)
	}
	if info.BaseRefName == "" {
		info.BaseRefName = "main"
	}

	return info.HeadRefName, info.BaseRefName, nil
}
