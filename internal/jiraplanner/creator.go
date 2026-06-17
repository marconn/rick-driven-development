package jiraplanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// --- jira-task-creator (plan-jira workflow) ---

// TaskCreatorHandler reads the project plan from shared state and creates
// Jira issues (epic + tasks with dependency links).
type TaskCreatorHandler struct {
	jira   *jira.Client
	state  *PlanningState
	logger *slog.Logger
}

// NewTaskCreator creates a jira-task-creator handler.
func NewTaskCreator(j *jira.Client, state *PlanningState, logger *slog.Logger) *TaskCreatorHandler {
	return &TaskCreatorHandler{jira: j, state: state, logger: logger}
}

func (c *TaskCreatorHandler) Name() string            { return "jira-task-creator" }
func (c *TaskCreatorHandler) Subscribes() []event.Type { return nil }

func (c *TaskCreatorHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wd := c.state.Get(env.CorrelationID)
	wd.mu.RLock()
	plan := wd.Plan
	wd.mu.RUnlock()

	if plan == nil {
		return nil, fmt.Errorf("jira-task-creator: no project plan in state")
	}

	return createJiraIssues(ctx, c.jira, plan, "jira-task-creator", env.CorrelationID, c.logger)
}

// --- task-creator (standalone workflow) ---

// StandaloneCreatorHandler generates a project plan from a free-text prompt
// and creates Jira issues in one shot. No shared state or hint pause.
type StandaloneCreatorHandler struct {
	backend backend.Backend
	jira    *jira.Client
	store   eventstore.Store
	logger  *slog.Logger
}

// NewStandaloneCreator creates a task-creator handler.
func NewStandaloneCreator(be backend.Backend, j *jira.Client, store eventstore.Store, logger *slog.Logger) *StandaloneCreatorHandler {
	return &StandaloneCreatorHandler{backend: be, jira: j, store: store, logger: logger}
}

func (c *StandaloneCreatorHandler) Name() string            { return "task-creator" }
func (c *StandaloneCreatorHandler) Subscribes() []event.Type { return nil }

func (c *StandaloneCreatorHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	prompt := c.extractPrompt(ctx, env)
	if prompt == "" {
		return nil, fmt.Errorf("task-creator: no prompt provided in workflow payload")
	}

	c.logger.Info("generating task plan from prompt", slog.String("prompt", truncateStr(prompt, 100)))

	userPrompt := renderTemplate(TaskCreatorUserPromptTemplate, map[string]string{
		"Prompt": prompt,
	})

	resp, err := c.backend.Run(ctx, backend.Request{
		SystemPrompt: TaskCreatorSystemPrompt,
		UserPrompt:   userPrompt,
	})
	if err != nil {
		return nil, fmt.Errorf("task-creator: generate plan: %w", err)
	}

	plan, err := ParseProjectPlan(resp.Output)
	if err != nil {
		return nil, fmt.Errorf("task-creator: parse plan: %w", err)
	}

	if plan.EpicTitle == "" {
		plan.EpicTitle = "Tareas: " + truncateStr(prompt, 60)
	}
	if plan.Goal == "" {
		plan.Goal = plan.EpicTitle
	}

	return createJiraIssues(ctx, c.jira, plan, "task-creator", env.CorrelationID, c.logger)
}

func (c *StandaloneCreatorHandler) extractPrompt(ctx context.Context, env event.Envelope) string {
	events, err := c.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return ""
	}
	for _, e := range events {
		if e.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.Prompt != "" {
			return p.Prompt
		}
	}
	return ""
}

// --- shared Jira issue creation ---

// jiraIssueWriter is the minimal slice of *jira.Client that createJiraIssues
// needs. Declaring it as an interface keeps the dedup contract explicit and
// lets the create path be driven by a stub without standing up an HTTP server.
// *jira.Client satisfies it directly.
type jiraIssueWriter interface {
	Search(ctx context.Context, jql string, maxResults int) (*jira.SearchResult, error)
	CreateEpic(ctx context.Context, title, description string) (string, error)
	CreateTask(ctx context.Context, epicKey, title, description string, storyPoints float64) (string, error)
	LinkIssues(ctx context.Context, blockerKey, blockedKey string) error
}

// correlationMarker returns the plain-text token embedded in an epic
// description so a re-dispatch can find the already-created epic via JQL text
// search. Both the embed (append to description) and the lookup (JQL `~`) use
// this single helper so the token can't drift between the two sides.
//
// The token is a SINGLE unbroken alphanumeric string (non-alphanumerics
// stripped from the correlation ID): Jira's JQL `~` runs through Lucene, which
// tokenizes on `:` and `-`. A hyphenated-UUID-plus-colon form like
// `rick-corr:<uuid>` would split into separate terms, so `description ~` could
// miss a marker that is genuinely present. Collapsing to one term
// (e.g. `rickcorr<hexuuid>`) makes it index and match as a single Lucene term.
func correlationMarker(correlationID string) string {
	var b strings.Builder
	b.WriteString("rickcorr")
	for _, r := range correlationID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func createJiraIssues(ctx context.Context, j jiraIssueWriter, plan *ProjectPlan, sourceName, correlationID string, logger *slog.Logger) ([]event.Envelope, error) {
	if j == nil {
		return nil, fmt.Errorf("%s: JIRA_URL not configured", sourceName)
	}

	// External-truth idempotency: before creating anything, look for an epic
	// already carrying this correlation's marker. The marker lives in the epic
	// description (atomic with CreateEpic — no create-then-tag window), so a
	// search hit proves a prior run already created the epic. Skipped when the
	// correlation ID is empty (can't key the marker) or when the search errors
	// (degrade to creating rather than wedging the workflow).
	if correlationID != "" {
		if existing := findExistingEpic(ctx, j, correlationID, logger); existing != "" {
			logger.Info("epic already created for correlation, idempotent skip",
				slog.String("key", existing), slog.String("correlation", correlationID))
			summary := fmt.Sprintf("Épico %s ya existía para esta solicitud (idempotente, no se recreó)", existing)
			enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
				Source:  sourceName,
				Kind:    "jira-issues",
				Summary: summary,
			})).WithSource("handler:" + sourceName)
			return []event.Envelope{enrichEvt}, nil
		}
	}

	epicDesc := buildEpicDescription(plan)
	if correlationID != "" {
		// Plain-text line carrying the single-term marker so the JQL
		// `description ~` precheck indexes and matches it as one Lucene term.
		epicDesc = strings.TrimRight(epicDesc, "\n") + "\n\n" + correlationMarker(correlationID)
	}

	logger.Info("creating Jira epic", slog.String("title", plan.EpicTitle), slog.Int("tasks", len(plan.Tasks)))

	epicKey, err := j.CreateEpic(ctx, plan.EpicTitle, epicDesc)
	if err != nil {
		return nil, fmt.Errorf("%s: create epic: %w", sourceName, err)
	}
	logger.Info("epic created", slog.String("key", epicKey))

	// Sort tasks by priority (lower number = higher priority).
	tasks := make([]JiraTask, len(plan.Tasks))
	copy(tasks, plan.Tasks)
	sort.Slice(tasks, func(i, k int) bool {
		pi, pk := tasks[i].Priority, tasks[k].Priority
		if pi == 0 {
			pi = 99
		}
		if pk == 0 {
			pk = 99
		}
		return pi < pk
	})

	var taskKeys []string
	titleToKey := make(map[string]string)
	for idx, task := range tasks {
		taskKey, err := j.CreateTask(ctx, epicKey, task.Title, task.Description, task.StoryPoints)
		if err != nil {
			logger.Error("failed to create task", slog.Int("index", idx+1), slog.String("title", task.Title), slog.Any("error", err))
			continue
		}
		taskKeys = append(taskKeys, taskKey)
		titleToKey[task.Title] = taskKey
	}

	// Link task dependencies ("Blocks" links).
	for _, task := range tasks {
		blockedKey, ok := titleToKey[task.Title]
		if !ok || len(task.Dependencies) == 0 {
			continue
		}
		for _, depTitle := range task.Dependencies {
			blockerKey := resolveDepKey(depTitle, titleToKey)
			if blockerKey == "" {
				continue
			}
			if err := j.LinkIssues(ctx, blockerKey, blockedKey); err != nil {
				logger.Error("failed to link", slog.String("blocker", blockerKey), slog.String("blocked", blockedKey), slog.Any("error", err))
			}
		}
	}

	summary := buildSummary(epicKey, taskKeys, len(tasks)-len(taskKeys), plan.Goal)

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  sourceName,
		Kind:    "jira-issues",
		Summary: summary,
	})).WithSource("handler:" + sourceName)

	return []event.Envelope{enrichEvt}, nil
}

// findExistingEpic searches Jira for an epic carrying the correlation marker.
// Returns the existing epic key, or "" if none (or on search error — the caller
// degrades to creating rather than failing the workflow). The marker is a
// globally-unique token (rick-corr:<correlationID>), so a text match alone is
// sufficient; project scoping is unnecessary for correctness and is omitted to
// avoid depending on the client's unexported default project.
func findExistingEpic(ctx context.Context, j jiraIssueWriter, correlationID string, logger *slog.Logger) string {
	jql := fmt.Sprintf(`issuetype = Epic AND description ~ %q`, correlationMarker(correlationID))
	res, err := j.Search(ctx, jql, 1)
	if err != nil {
		logger.Warn("idempotency search failed, proceeding to create", slog.Any("error", err))
		return ""
	}
	if res == nil || len(res.Issues) == 0 {
		return ""
	}
	return res.Issues[0].Key
}

func buildEpicDescription(plan *ProjectPlan) string {
	var sb strings.Builder
	if plan.Goal != "" {
		fmt.Fprintf(&sb, "**Objetivo:** %s\n\n", plan.Goal)
	}
	if plan.EpicDesc != "" {
		sb.WriteString(plan.EpicDesc)
		sb.WriteString("\n\n")
	}
	if len(plan.Risks) > 0 {
		sb.WriteString("**Riesgos:**\n")
		for _, r := range plan.Risks {
			fmt.Fprintf(&sb, "- %s (probabilidad: %s) → %s\n", r.Description, r.Probability, r.Mitigation)
		}
		sb.WriteString("\n")
	}
	if len(plan.Dependencies) > 0 {
		sb.WriteString("**Dependencias:**\n")
		for _, d := range plan.Dependencies {
			fmt.Fprintf(&sb, "- %s: %s\n", d.Name, d.Description)
		}
	}
	return strings.TrimSpace(sb.String())
}

func buildSummary(epicKey string, taskKeys []string, failed int, goal string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Épico %s creado", epicKey)
	if goal != "" {
		fmt.Fprintf(&sb, " — %s", truncateStr(goal, 80))
	}
	if len(taskKeys) > 0 {
		fmt.Fprintf(&sb, ". Tareas: %s", strings.Join(taskKeys, ", "))
	}
	if failed > 0 {
		fmt.Fprintf(&sb, " (%d fallidas)", failed)
	}
	return sb.String()
}

// resolveDepKey finds the Jira key for a dependency title. Tries exact match
// first, then falls back to substring matching for fuzzy AI-generated refs.
func resolveDepKey(depTitle string, titleToKey map[string]string) string {
	if key, ok := titleToKey[depTitle]; ok {
		return key
	}
	depLower := strings.ToLower(depTitle)
	for title, key := range titleToKey {
		titleLower := strings.ToLower(title)
		if strings.Contains(titleLower, depLower) || strings.Contains(depLower, titleLower) {
			return key
		}
	}
	return ""
}
