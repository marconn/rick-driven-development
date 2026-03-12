package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/pluginstore"
)

// CIPollerConfig holds configuration for CI status polling.
type CIPollerConfig struct {
	// PollInterval between check run queries (default: 15s).
	PollInterval time.Duration

	// Timeout is the max time to wait for CI to finish (default: 10m).
	Timeout time.Duration

	// CIFixWorkflow is the workflow ID to trigger on CI failure (default: "ci-fix").
	CIFixWorkflow string

	// MaxCIRetries limits how many ci-fix workflows can chain (default: 2).
	MaxCIRetries int
}

func (c *CIPollerConfig) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	if c.CIFixWorkflow == "" {
		c.CIFixWorkflow = "ci-fix"
	}
	if c.MaxCIRetries <= 0 {
		c.MaxCIRetries = 2
	}
}

// CIPoller polls GitHub Actions check runs after a workflow completes and
// triggers a ci-fix workflow if CI fails.
type CIPoller struct {
	gh     *Client
	bus    eventbus.Bus
	pstore *pluginstore.Store
	cfg    CIPollerConfig
	logger *slog.Logger
}

// NewCIPoller creates a CI status poller. The bus is used to publish
// ci-fix workflow requests when CI fails.
func NewCIPoller(gh *Client, bus eventbus.Bus, pstore *pluginstore.Store, cfg CIPollerConfig, logger *slog.Logger) *CIPoller {
	cfg.defaults()
	return &CIPoller{
		gh:     gh,
		bus:    bus,
		pstore: pstore,
		cfg:    cfg,
		logger: logger,
	}
}

// PollAfterWorkflow starts polling CI status for a completed workflow.
// Should be called as a goroutine from the notification handler.
// Only polls for workflows that have PR info and completed successfully
// (the committer pushed code, so CI should be running).
func (p *CIPoller) PollAfterWorkflow(ctx context.Context, correlationID string, ticket *pluginstore.Ticket) {
	if ticket.PRNumber == 0 || ticket.Repo == "" {
		return
	}

	owner, repo, ok := parseOwnerRepo(ticket.Repo)
	if !ok {
		return
	}

	// Check if this is already a ci-fix retry to prevent infinite loops.
	ciAttempt := p.pstore.GetCIAttemptCount(ticket.TicketID)
	if ciAttempt >= p.cfg.MaxCIRetries {
		p.logger.Warn("ci-poller: max CI retries reached, skipping",
			slog.String("ticket", ticket.TicketID),
			slog.Int("attempts", ciAttempt),
		)
		return
	}

	// Get the PR's current HEAD SHA (the commit the committer just pushed).
	head, err := p.gh.GetPRHead(ctx, owner, repo, ticket.PRNumber)
	if err != nil {
		p.logger.Error("ci-poller: get PR head",
			slog.String("ticket", ticket.TicketID),
			slog.Any("error", err),
		)
		return
	}

	p.logger.Info("ci-poller: watching CI for commit",
		slog.String("ticket", ticket.TicketID),
		slog.String("sha", head.SHA[:8]),
		slog.String("repo", ticket.Repo),
	)

	result, err := p.waitForChecks(ctx, owner, repo, head.SHA)
	if err != nil {
		p.logger.Error("ci-poller: wait for checks",
			slog.String("ticket", ticket.TicketID),
			slog.Any("error", err),
		)
		return
	}

	if result.allPassed {
		p.logger.Info("ci-poller: CI passed",
			slog.String("ticket", ticket.TicketID),
			slog.String("sha", head.SHA[:8]),
		)
		return
	}

	// CI failed — trigger ci-fix workflow.
	p.logger.Warn("ci-poller: CI failed, triggering ci-fix workflow",
		slog.String("ticket", ticket.TicketID),
		slog.Int("failed_checks", len(result.failures)),
	)

	if err := p.pstore.IncrementCIAttempt(ticket.TicketID); err != nil {
		p.logger.Error("ci-poller: increment attempt",
			slog.String("ticket", ticket.TicketID),
			slog.Any("error", err),
		)
	}

	p.triggerCIFix(ctx, ticket, head, result)
}

type checkResult struct {
	allPassed bool
	failures  []CheckRun
}

func (p *CIPoller) waitForChecks(ctx context.Context, owner, repo, sha string) (*checkResult, error) {
	deadline := time.After(p.cfg.Timeout)
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("ci-poller: timeout waiting for checks on %s", sha[:8])
		case <-ticker.C:
			resp, err := p.gh.GetCheckRuns(ctx, owner, repo, sha)
			if err != nil {
				p.logger.Warn("ci-poller: check runs query failed, retrying",
					slog.Any("error", err),
				)
				continue
			}

			if resp.TotalCount == 0 {
				continue // No checks registered yet, keep waiting.
			}

			allDone := true
			var failures []CheckRun
			for _, cr := range resp.CheckRuns {
				if cr.Status != "completed" {
					allDone = false
					break
				}
				if cr.Conclusion != "success" && cr.Conclusion != "skipped" && cr.Conclusion != "neutral" {
					failures = append(failures, cr)
				}
			}

			if !allDone {
				continue
			}

			return &checkResult{
				allPassed: len(failures) == 0,
				failures:  failures,
			}, nil
		}
	}
}

func (p *CIPoller) triggerCIFix(ctx context.Context, ticket *pluginstore.Ticket, head *PRHead, result *checkResult) {
	prompt := p.buildCIFixPrompt(ticket, head, result)

	payload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     prompt,
		WorkflowID: p.cfg.CIFixWorkflow,
		Source:     fmt.Sprintf("ci-fix:%s", ticket.TicketID),
		Repo:       ticket.Repo,
		Ticket:     ticket.TicketID,
		RepoBranch: ticket.Branch,
	})

	correlationID := fmt.Sprintf("%s-cifix-%d", ticket.CorrelationID, p.pstore.GetCIAttemptCount(ticket.TicketID))

	env := event.New(event.WorkflowRequested, 1, payload).
		WithCorrelation(correlationID).
		WithSource("ci-poller")

	if err := p.bus.Publish(ctx, env); err != nil {
		p.logger.Error("ci-poller: publish ci-fix workflow",
			slog.String("ticket", ticket.TicketID),
			slog.Any("error", err),
		)
		return
	}

	// Track the ci-fix workflow correlation so the reporter can post results.
	_ = p.pstore.SaveTicket(pluginstore.Ticket{
		TicketID:      ticket.TicketID + "-cifix",
		CorrelationID: correlationID,
		Repo:          ticket.Repo,
		Branch:        ticket.Branch,
		PRURL:         ticket.PRURL,
		PRNumber:      ticket.PRNumber,
		Summary:       fmt.Sprintf("CI fix for %s", ticket.Summary),
		Status:        "running",
	})

	p.logger.Info("ci-poller: ci-fix workflow triggered",
		slog.String("ticket", ticket.TicketID),
		slog.String("correlation", correlationID),
	)
}

func (p *CIPoller) buildCIFixPrompt(ticket *pluginstore.Ticket, head *PRHead, result *checkResult) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Fix CI failures for ticket %s on branch %s (commit %s).\n\n",
		ticket.TicketID, head.Ref, head.SHA[:8]))

	b.WriteString("## Failed Checks\n\n")
	for _, cr := range result.failures {
		b.WriteString(fmt.Sprintf("### %s — %s\n", cr.Name, cr.Conclusion))
		if cr.Output.Title != "" {
			b.WriteString(fmt.Sprintf("**%s**\n", cr.Output.Title))
		}
		if cr.Output.Summary != "" {
			b.WriteString(cr.Output.Summary + "\n")
		}
		b.WriteString(fmt.Sprintf("Details: %s\n\n", cr.HTMLURL))
	}

	b.WriteString("## Instructions\n\n")
	b.WriteString("Fix the CI failures listed above. Focus on:\n")
	b.WriteString("1. Read the failing check output and identify the root cause\n")
	b.WriteString("2. Make the minimum changes needed to fix the failures\n")
	b.WriteString("3. Do not introduce new features or refactoring — CI fix only\n")

	return b.String()
}
