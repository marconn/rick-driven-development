package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/pluginstore"
)

// FetcherHandler fetches PR review comments from GitHub and emits enrichment events.
// Implements handler.Handler for use in DAG-based workflows.
type FetcherHandler struct {
	gh     *Client
	store  eventstore.Store
	pstore *pluginstore.Store // for ticket fallback lookup
	logger *slog.Logger
}

// NewFetcherHandler creates a FetcherHandler.
func NewFetcherHandler(gh *Client, store eventstore.Store, pstore *pluginstore.Store, logger *slog.Logger) *FetcherHandler {
	return &FetcherHandler{gh: gh, store: store, pstore: pstore, logger: logger}
}

// Name returns the handler name.
func (f *FetcherHandler) Name() string { return "github-pr-fetcher" }

// Subscribes returns nil — dispatch is controlled by the DAG.
func (f *FetcherHandler) Subscribes() []event.Type { return nil }

// Handle processes a dispatched event by fetching PR feedback from GitHub.
//
// When the GitHub client is nil (GITHUB_TOKEN unset), the handler short-circuits
// with an empty enrichment so DAGs that include github-pr-fetcher can still
// complete. Operators see a clear log line and an empty Summary in the event
// store rather than a silently-skipped step or a hard failure.
func (f *FetcherHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	if f.gh == nil {
		f.logger.Warn("github-pr-fetcher: GITHUB_TOKEN unset, skipping PR fetch — feedback-analyzer will only see operator prompt",
			slog.String("correlation", env.CorrelationID),
		)
		enrichment := event.ContextEnrichmentPayload{
			Source:  "github-pr-fetcher",
			Kind:    "pr-reviews",
			Summary: "(GITHUB_TOKEN not configured — no PR reviews or inline comments fetched)",
		}
		return []event.Envelope{
			event.New(event.ContextEnrichment, 1, event.MustMarshal(enrichment)),
		}, nil
	}

	// Load WorkflowRequested (or WorkflowStarted) to get source field.
	source, err := f.resolveSource(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("github-pr-fetcher: resolve source: %w", err)
	}

	owner, repo, prNumber, parseErr := parsePRRef(source, "")
	if parseErr != nil {
		// Try pluginstore fallback (jira-poller flow: ticket has repo+PR info).
		if f.pstore != nil && env.CorrelationID != "" {
			ticket, ticketErr := f.pstore.GetTicketByCorrelation(env.CorrelationID)
			if ticketErr == nil && ticket != nil && ticket.PRNumber > 0 && ticket.Repo != "" {
				o, r, ok := parseOwnerRepo(ticket.Repo)
				if ok {
					owner, repo, prNumber = o, r, ticket.PRNumber
					parseErr = nil
				}
			}
		}
		if parseErr != nil {
			return nil, fmt.Errorf("no PR reference found: %w", parseErr)
		}
	}

	f.logger.Info("fetching PR feedback",
		slog.String("owner", owner),
		slog.String("repo", repo),
		slog.Int("pr", prNumber),
	)

	feedback, err := f.FetchPRFeedback(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("fetch PR feedback: %w", err)
	}

	enrichment := event.ContextEnrichmentPayload{
		Source:  "github-pr-fetcher",
		Kind:    "pr-reviews",
		Summary: feedback,
	}
	return []event.Envelope{
		event.New(event.ContextEnrichment, 1, event.MustMarshal(enrichment)),
	}, nil
}

// resolveSource reads the WorkflowStarted event from the store to extract the Source field.
func (f *FetcherHandler) resolveSource(ctx context.Context, env event.Envelope) (string, error) {
	events, err := f.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return "", fmt.Errorf("load events: %w", err)
	}
	for _, e := range events {
		if event.IsWorkflowStarted(e.Type) {
			var p event.WorkflowStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Source != "" {
				return p.Source, nil
			}
		}
		if e.Type == event.WorkflowRequested {
			var p event.WorkflowRequestedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Source != "" {
				return p.Source, nil
			}
		}
	}
	return "", fmt.Errorf("no source found in workflow events for correlation %s", env.CorrelationID)
}

// FetchPRFeedback fetches reviews, inline comments, and diff for a PR,
// formatting them as human-readable markdown.
func (f *FetcherHandler) FetchPRFeedback(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	pr, err := f.gh.GetPR(ctx, owner, repo, prNumber)
	if err != nil {
		return "", fmt.Errorf("get PR: %w", err)
	}

	reviews, err := f.gh.GetPRReviews(ctx, owner, repo, prNumber)
	if err != nil {
		return "", fmt.Errorf("get reviews: %w", err)
	}

	comments, err := f.gh.GetPRReviewComments(ctx, owner, repo, prNumber)
	if err != nil {
		return "", fmt.Errorf("get review comments: %w", err)
	}

	diff, err := f.gh.GetPRDiff(ctx, owner, repo, prNumber)
	if err != nil {
		// Non-fatal: diff is supplementary.
		f.logger.Warn("failed to fetch PR diff, continuing without it",
			slog.Any("error", err),
		)
		diff = ""
	}

	return formatPRFeedback(pr, reviews, comments, diff), nil
}

func formatPRFeedback(pr *PullRequest, reviews []Review, comments []ReviewComment, diff string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("## PR #%d: %s\n\n", pr.Number, pr.Title))

	// Reviews (top-level, body-only).
	hasReviews := false
	for _, r := range reviews {
		if r.Body == "" {
			continue
		}
		if !hasReviews {
			b.WriteString("### Reviews\n\n")
			hasReviews = true
		}
		b.WriteString(fmt.Sprintf("**@%s** (%s):\n", r.User.Login, r.State))
		b.WriteString(fmt.Sprintf("> %s\n\n", strings.ReplaceAll(r.Body, "\n", "\n> ")))
	}

	// Inline diff comments.
	if len(comments) > 0 {
		b.WriteString("### Inline Comments\n\n")
		for _, c := range comments {
			loc := c.Path
			if c.Line > 0 {
				loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
			}
			b.WriteString(fmt.Sprintf("**@%s** on `%s`:\n", c.User.Login, loc))
			if c.DiffHunk != "" {
				b.WriteString("```diff\n")
				b.WriteString(c.DiffHunk)
				b.WriteString("\n```\n")
			}
			b.WriteString(fmt.Sprintf("> %s\n\n", strings.ReplaceAll(c.Body, "\n", "\n> ")))
		}
	}

	// Diff summary (file-level statistics only — avoids flooding the context).
	if diff != "" {
		b.WriteString("### Diff Summary\n\n")
		lines := strings.Split(diff, "\n")
		var diffSummary []string
		filesChanged := 0
		additions := 0
		deletions := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "diff --git") {
				filesChanged++
				diffSummary = append(diffSummary, line)
			} else if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				additions++
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				deletions++
			}
		}
		b.WriteString(fmt.Sprintf("%d files changed, %d additions(+), %d deletions(-)\n\n", filesChanged, additions, deletions))
		for _, s := range diffSummary {
			b.WriteString(s + "\n")
		}
		b.WriteString("\n")
	}

	return b.String()
}

// prRefRegexp matches "gh:owner/repo#123" format.
var prRefRegexp = regexp.MustCompile(`^gh:([^/]+)/([^#]+)#(\d+)$`)

// prURLRegexp matches GitHub PR URLs: https://github.com/owner/repo/pull/123
// Also matches with trailing path segments (e.g., /files, /commits).
var prURLRegexp = regexp.MustCompile(`https?://[^/]*github[^/]*/([^/]+)/([^/]+)/pull/(\d+)`)

// parsePRRef extracts owner, repo, and PR number from Source and Repo fields.
// Supported formats:
//   - "gh:owner/repo#123"
//   - "https://github.com/owner/repo/pull/123"
//   - Any string containing a GitHub PR URL
func parsePRRef(source, repo string) (owner, repoName string, prNumber int, err error) {
	// Try structured format: "gh:owner/repo#123"
	if m := prRefRegexp.FindStringSubmatch(source); m != nil {
		n, _ := strconv.Atoi(m[3])
		return m[1], m[2], n, nil
	}

	// Try GitHub PR URL in source field.
	if m := prURLRegexp.FindStringSubmatch(source); m != nil {
		n, _ := strconv.Atoi(m[3])
		return m[1], m[2], n, nil
	}

	// Fallback: parse repo field "owner/repo" — but we still need a PR number
	// which we can't get from just the repo field.
	if repo != "" {
		return "", "", 0, fmt.Errorf("source %q has no PR reference and repo %q has no PR number", source, repo)
	}

	return "", "", 0, fmt.Errorf("cannot parse PR reference from source=%q repo=%q", source, repo)
}

// ParsePRURL extracts owner, repo, and PR number from a GitHub PR URL found
// anywhere in the text. Returns zero values if no URL is found.
func ParsePRURL(text string) (owner, repo string, prNumber int, ok bool) {
	if m := prURLRegexp.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[3])
		return m[1], m[2], n, true
	}
	return "", "", 0, false
}
