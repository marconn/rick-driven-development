package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	gh "github.com/marconn/rick-event-driven-development/internal/github"
)

// ghIssueSourceRegexp parses a `gh:owner/repo#N` source string. Reused across
// PR and issue workflows — the DAG distinguishes intent.
var ghIssueSourceRegexp = regexp.MustCompile(`^gh:([^/]+)/([^#]+)#(\d+)$`)

// GithubContextHandler fetches a GitHub issue and emits context enrichment for
// downstream personas. Mirrors JiraContextHandler but reads from GitHub Issues
// instead of Jira tickets. Used in the github-dev workflow.
type GithubContextHandler struct {
	store  eventstore.Store
	github *gh.Client
}

// NewGithubContext creates a GithubContextHandler.
func NewGithubContext(d Deps) *GithubContextHandler {
	return &GithubContextHandler{
		store:  d.Store,
		github: d.GitHub,
	}
}

func (h *GithubContextHandler) Name() string             { return "github-context" }
func (h *GithubContextHandler) Subscribes() []event.Type { return nil }

func (h *GithubContextHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	params, err := h.loadWorkflowRequested(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("github-context: load params: %w", err)
	}

	owner, repo, number, err := resolveGithubIssueRef(params)
	if err != nil {
		return nil, fmt.Errorf("github-context: %w", err)
	}

	if h.github == nil {
		return nil, fmt.Errorf("github-context: GITHUB_TOKEN not configured")
	}

	issue, err := h.github.GetIssue(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("github-context: fetch %s/%s#%d: %w", owner, repo, number, err)
	}
	if issue.PullRequest != nil {
		return nil, fmt.Errorf("github-context: %s/%s#%d is a pull request, not an issue — use dag=pr-review instead", owner, repo, number)
	}
	if issue.State == "closed" && !hasAllowClosedOverride(params.Prompt) {
		return nil, fmt.Errorf(
			"github-context: %s/%s#%d is closed (state_reason=%q, closed_at=%q) — refusing to implement already-resolved work; "+
				"if this is intentional (re-implementation, intentional revert, separate copy), add the directive `allow-closed` to the prompt",
			owner, repo, number, issue.StateReason, issue.ClosedAt,
		)
	}

	enrichment := buildGithubIssueEnrichment(owner, repo, number, issue)
	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(enrichment)).
		WithSource("handler:github-context")

	return []event.Envelope{enrichEvt}, nil
}

func (h *GithubContextHandler) loadWorkflowRequested(ctx context.Context, correlationID string) (event.WorkflowRequestedPayload, error) {
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

// hasAllowClosedOverride reports whether the operator opted in to running
// github-dev against a closed issue. The directive is a literal `allow-closed`
// substring in the workflow prompt — kept simple so the error message can
// quote it verbatim and the operator types it deliberately rather than
// accidentally.
func hasAllowClosedOverride(prompt string) bool {
	return strings.Contains(prompt, "allow-closed")
}

// resolveGithubIssueRef extracts owner/repo/issue-number from the workflow
// params. Priority:
//  1. Source="gh:owner/repo#N" — authoritative.
//  2. Repo="owner/repo" + Ticket=N — explicit fields.
// Returns a descriptive error if neither form is present.
func resolveGithubIssueRef(p event.WorkflowRequestedPayload) (string, string, int, error) {
	if m := ghIssueSourceRegexp.FindStringSubmatch(p.Source); m != nil {
		n, err := strconv.Atoi(m[3])
		if err != nil {
			return "", "", 0, fmt.Errorf("parse issue number from source: %w", err)
		}
		return m[1], m[2], n, nil
	}

	if p.Repo != "" && p.Ticket != "" {
		parts := strings.SplitN(p.Repo, "/", 2)
		if len(parts) != 2 {
			return "", "", 0, fmt.Errorf("repo %q must be owner/name", p.Repo)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(p.Ticket, "#"))
		if err != nil {
			return "", "", 0, fmt.Errorf("parse ticket %q as issue number: %w", p.Ticket, err)
		}
		return parts[0], parts[1], n, nil
	}

	return "", "", 0, fmt.Errorf("no issue reference — set source=gh:owner/repo#N or repo+ticket")
}

func buildGithubIssueEnrichment(owner, repo string, number int, issue *gh.Issue) event.ContextEnrichmentPayload {
	fullRepo := owner + "/" + repo

	var sb strings.Builder
	fmt.Fprintf(&sb, "## GitHub Issue: %s#%d\n\n", fullRepo, number)
	fmt.Fprintf(&sb, "**Title**: %s\n", issue.Title)
	if issue.State != "" {
		fmt.Fprintf(&sb, "**State**: %s\n", issue.State)
	}
	if issue.State == "closed" {
		if issue.StateReason != "" {
			fmt.Fprintf(&sb, "**State reason**: %s\n", issue.StateReason)
		}
		if issue.ClosedAt != "" {
			fmt.Fprintf(&sb, "**Closed at**: %s\n", issue.ClosedAt)
		}
	}
	if issue.User.Login != "" {
		fmt.Fprintf(&sb, "**Author**: @%s\n", issue.User.Login)
	}
	if issue.HTMLURL != "" {
		fmt.Fprintf(&sb, "**URL**: %s\n", issue.HTMLURL)
	}
	if len(issue.Labels) > 0 {
		names := make([]string, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			names = append(names, l.Name)
		}
		fmt.Fprintf(&sb, "**Labels**: %s\n", strings.Join(names, ", "))
	}
	sb.WriteString("\n")
	if body := strings.TrimSpace(issue.Body); body != "" {
		fmt.Fprintf(&sb, "**Description**:\n%s\n\n", body)
	}
	fmt.Fprintf(&sb, "**Repo**: %s\n", fullRepo)

	// "ticket" drives the workspace branch name via the workspace handler's
	// enrichment fallback. github-dev has no caller-supplied ticket (only
	// source=gh:owner/repo#N), so derive a branch-safe name from the issue
	// number — `issue-<N>` is human-readable and collision-safe within a repo.
	return event.ContextEnrichmentPayload{
		Source:  "github-context",
		Kind:    "issue",
		Summary: strings.TrimSpace(sb.String()),
		Items: []event.EnrichmentItem{
			{Name: "repo", Reason: fullRepo},
			{Name: "ticket", Reason: fmt.Sprintf("issue-%d", number)},
		},
	}
}
