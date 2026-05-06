package jirapoller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	"github.com/marconn/rick-event-driven-development/internal/pluginstore"
)

// WorkflowRegistry is the narrow dependency the poller needs to validate
// that its configured WorkflowID points at a registered DAG before
// publishing a WorkflowRequested. *engine.Engine implements this.
//
// May be nil; when nil, validation is skipped. Production wiring must pass
// a real registry — passing nil only exists for tests that focus on
// unrelated paths.
type WorkflowRegistry interface {
	GetWorkflowDef(id string) (engine.WorkflowDef, bool)
}

// Config holds configuration for the Jira polling loop.
type Config struct {
	// JQL is the Jira Query Language expression to poll (required).
	JQL string

	// PollInterval is the time between polling cycles (default: 60s).
	PollInterval time.Duration

	// MaxResults per JQL query (default: 50).
	MaxResults int

	// WorkflowID is the Rick workflow to trigger (default: "pr-review").
	WorkflowID string

	// FieldMappings maps Jira field keys to Rick context.
	// Keys: "repo", "branch", "pr_url", "pr_number".
	// Values: Jira field IDs (e.g., "customfield_10100").
	// If empty, the poller extracts from labels (repo:owner/name, branch:name).
	FieldMappings map[string]string

	Logger *slog.Logger
}

func (c *Config) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 60 * time.Second
	}
	if c.MaxResults <= 0 {
		c.MaxResults = 50
	}
	if c.WorkflowID == "" {
		c.WorkflowID = "pr-review"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Poller polls Jira for new tickets and publishes Rick workflow events.
type Poller struct {
	cfg      Config
	jira     *jira.Client
	pstore   *pluginstore.Store
	bus      eventbus.Bus
	registry WorkflowRegistry
	logger   *slog.Logger
}

// NewPoller creates a Jira poller. registry may be nil in tests; production
// callers must pass a real registry so a misconfigured WorkflowID surfaces
// as a refusal-to-publish (with a loud Error log) rather than an unbounded
// stream of fail-fast WorkflowRequested events. Call Run() to start.
func NewPoller(jiraClient *jira.Client, pstore *pluginstore.Store, bus eventbus.Bus, registry WorkflowRegistry, cfg Config) *Poller {
	cfg.defaults()
	return &Poller{
		cfg:      cfg,
		jira:     jiraClient,
		pstore:   pstore,
		bus:      bus,
		registry: registry,
		logger:   cfg.Logger,
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	p.logger.Info("jira poller: starting",
		slog.String("jql", p.cfg.JQL),
		slog.Duration("interval", p.cfg.PollInterval),
		slog.String("workflow", p.cfg.WorkflowID),
	)

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	// Poll immediately on start.
	p.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	// Validate the configured WorkflowID points at a registered DAG BEFORE
	// we hit the Jira API. Without this guard, a typo / env-var drift /
	// plugin-not-loaded would publish a stream of WorkflowRequested events
	// the engine can only fail-fast against — operators would see waves of
	// WorkflowFailed in their tracker on every poll cycle until the config
	// is fixed. Short-circuiting here keeps both the Jira tracker and Rick's
	// store clean; recovery is automatic once the operator fixes the env
	// var (next cycle picks up the accumulated tickets).
	// docs/bugs/jira-dev-stuck-in-requested.md.
	if p.registry != nil {
		if _, ok := p.registry.GetWorkflowDef(p.cfg.WorkflowID); !ok {
			p.logger.Error("jira poller: skipping cycle; configured workflow_id is not a registered DAG",
				slog.String("workflow_id", p.cfg.WorkflowID),
				slog.String("hint", "check JIRA_POLL_WORKFLOW env var; the DAG must be a built-in or registered by a connected plugin"),
			)
			return
		}
	}

	result, err := p.jira.Search(ctx, p.cfg.JQL, p.cfg.MaxResults)
	if err != nil {
		p.logger.Error("jira poller: search failed", slog.Any("error", err))
		return
	}

	p.logger.Debug("jira poller: search returned",
		slog.Int("total", result.Total),
		slog.Int("fetched", len(result.Issues)),
	)

	for _, issue := range result.Issues {
		if err := p.processIssue(ctx, issue); err != nil {
			p.logger.Error("jira poller: process issue failed",
				slog.String("key", issue.Key),
				slog.Any("error", err),
			)
		}
	}
}

func (p *Poller) processIssue(ctx context.Context, issue jira.SearchIssue) error {
	processed, err := p.pstore.IsProcessed(issue.Key)
	if err != nil {
		return fmt.Errorf("check processed: %w", err)
	}
	if processed {
		return nil
	}

	raw, err := p.jira.FetchRawIssue(ctx, issue.Key)
	if err != nil {
		return fmt.Errorf("get issue: %w", err)
	}

	info := p.extractInfo(raw)
	if info.Repo == "" {
		p.logger.Warn("jira poller: skipping issue without repo",
			slog.String("key", issue.Key),
		)
		return nil
	}

	correlationID := uuid.New().String()
	prompt := p.buildPrompt(issue, info)

	payload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     prompt,
		WorkflowID: p.cfg.WorkflowID,
		Source:     fmt.Sprintf("jira:%s", issue.Key),
		Repo:       info.Repo,
		Ticket:     issue.Key,
		BaseBranch: info.Branch,
	})

	env := event.New(event.WorkflowRequested, 1, payload).
		WithCorrelation(correlationID).
		WithSource("jira-poller")

	if err := p.bus.Publish(ctx, env); err != nil {
		return fmt.Errorf("publish workflow: %w", err)
	}

	if err := p.pstore.SaveTicket(pluginstore.Ticket{
		TicketID:      issue.Key,
		CorrelationID: correlationID,
		Repo:          info.Repo,
		Branch:        info.Branch,
		PRURL:         info.PRURL,
		PRNumber:      info.PRNumber,
		Summary:       issue.Fields.Summary,
		Status:        "running",
	}); err != nil {
		return fmt.Errorf("save ticket: %w", err)
	}

	p.logger.Info("jira poller: workflow triggered",
		slog.String("ticket", issue.Key),
		slog.String("correlation", correlationID),
		slog.String("repo", info.Repo),
		slog.String("branch", info.Branch),
	)

	return nil
}

type issueInfo struct {
	Repo     string
	Branch   string
	PRURL    string
	PRNumber int
}

func (p *Poller) extractInfo(raw *jira.RawIssue) issueInfo {
	info := issueInfo{}

	// Try configured field mappings first.
	if field, ok := p.cfg.FieldMappings["repo"]; ok {
		if v, exists := raw.Fields[field]; exists {
			info.Repo = jira.ExtractTextField(v)
		}
	}
	if field, ok := p.cfg.FieldMappings["branch"]; ok {
		if v, exists := raw.Fields[field]; exists {
			info.Branch = jira.ExtractTextField(v)
		}
	}
	if field, ok := p.cfg.FieldMappings["pr_url"]; ok {
		if v, exists := raw.Fields[field]; exists {
			info.PRURL = jira.ExtractTextField(v)
		}
	}

	// Fallback: extract from labels (repo:owner/name, branch:name).
	if info.Repo == "" {
		if labelsRaw, ok := raw.Fields["labels"]; ok {
			var labels []string
			if err := json.Unmarshal(labelsRaw, &labels); err == nil {
				for _, l := range labels {
					if strings.HasPrefix(l, "repo:") {
						info.Repo = strings.TrimPrefix(l, "repo:")
					}
					if strings.HasPrefix(l, "branch:") {
						info.Branch = strings.TrimPrefix(l, "branch:")
					}
				}
			}
		}
	}

	if info.Branch == "" {
		info.Branch = "main"
	}

	if info.PRURL != "" && info.PRNumber == 0 {
		info.PRNumber = extractPRNumber(info.PRURL)
	}

	return info
}

func (p *Poller) buildPrompt(issue jira.SearchIssue, info issueInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the code changes for Jira ticket %s.\n\n", issue.Key)
	fmt.Fprintf(&b, "**Summary:** %s\n\n", issue.Fields.Summary)

	desc := jira.ExtractTextField(issue.Fields.Description)
	if desc != "" {
		fmt.Fprintf(&b, "**Description:**\n%s\n\n", desc)
	}

	fmt.Fprintf(&b, "**Repository:** %s\n", info.Repo)
	fmt.Fprintf(&b, "**Branch:** %s\n", info.Branch)
	if info.PRURL != "" {
		fmt.Fprintf(&b, "**Pull Request:** %s\n", info.PRURL)
	}

	return b.String()
}

func extractPRNumber(prURL string) int {
	parts := strings.Split(prURL, "/")
	if len(parts) < 2 {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &n); err != nil {
		return 0
	}
	return n
}
