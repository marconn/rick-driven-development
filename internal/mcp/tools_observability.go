package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// safeBranchRe validates git branch names to prevent flag injection.
var safeBranchRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/_.\-]+$`)

// safeRepoRe validates owner/repo format to prevent argument injection.
var safeRepoRe = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+/[a-zA-Z0-9_.\-]+$`)

func (s *Server) registerObservabilityTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_search_workflows",
			Description: "Find workflows by business key (ticket, source, repo). Uses the event_tags SQLite table for O(1) lookup.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira ticket key.",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Source reference (e.g., gh:owner/repo#123).",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": "Repository name.",
					},
					"status": map[string]any{
						"type":        "string",
						"enum":        []string{"running", "completed", "failed", "paused", "cancelled"},
						"description": "Filter by workflow status.",
					},
				},
			},
		},
		Handler: s.toolSearchWorkflows,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_retry_workflow",
			Description: "Resume a failed or cancelled workflow. With from_phase set, the original workflow is re-dispatched at that phase — every upstream PersonaCompleted is preserved, so only from_phase and its DAG-downstream re-run (no token re-bill for already-done work). Without from_phase, a brand-new workflow is started with the original prompt/dag/source/ticket.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The failed workflow's aggregate ID.",
					},
					"from_phase": map[string]any{
						"type":        "string",
						"description": "Handler name to restart at. Must be present in the workflow's DAG. When set, the original correlation is reused and upstream completions are preserved.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolRetryWorkflow,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workflow_output",
			Description: "Get consolidated output from all personas in a workflow. Saves N calls to rick_persona_output.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"phases": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Filter to specific phases (optional).",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolWorkflowOutput,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_diff",
			Description: "Show the git diff of code changes from a workflow's workspace. Requires the workspace to still exist.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"stat_only": map[string]any{
						"type":    "boolean",
						"default": false,
						"description": "Show only diffstat, not full diff.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolDiff,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_pr_diff",
			Description: "Fetch the diff of a GitHub pull request by repo and PR number. Does not require a workflow or workspace — calls GitHub directly via `gh` CLI.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo": map[string]any{
						"type":        "string",
						"description": "Repository in owner/repo format (e.g. 'hulilabs/hulihealth-web').",
					},
					"pr_number": map[string]any{
						"type":        "integer",
						"description": "Pull request number.",
					},
					"stat_only": map[string]any{
						"type":    "boolean",
						"default": false,
						"description": "Show only diffstat, not full diff.",
					},
				},
				"required": []string{"repo", "pr_number"},
			},
		},
		Handler: s.toolPRDiff,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_create_pr",
			Description: "Create a GitHub PR from a completed workflow's workspace. Extracts ticket reference from correlation tags, builds PR title/body from workflow context.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"title": map[string]any{
						"type":        "string",
						"description": "Override PR title (auto-generated from ticket if omitted).",
					},
					"draft": map[string]any{
						"type":    "boolean",
						"default": false,
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolCreatePR,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_project_sync",
			Description: "Generate a project dependency diagram from a Jira epic. Returns a Mermaid graph + status table with progress summary.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Epic issue key.",
					},
				},
				"required": []string{"epic"},
			},
		},
		Handler: s.toolProjectSync,
	})
}

// --- Handlers ---

type searchWorkflowsArgs struct {
	Ticket string `json:"ticket"`
	Source string `json:"source"`
	Repo   string `json:"repo"`
	Status string `json:"status"`
}

func (s *Server) toolSearchWorkflows(ctx context.Context, raw json.RawMessage) (any, error) {
	var args searchWorkflowsArgs
	if raw != nil {
		_ = json.Unmarshal(raw, &args)
	}

	// Collect correlation IDs from tag lookups.
	corrSet := make(map[string]bool)
	searched := false

	for _, pair := range []struct{ key, val string }{
		{"ticket", args.Ticket},
		{"source", args.Source},
		{"repo", args.Repo},
	} {
		if pair.val == "" {
			continue
		}
		searched = true
		ids, err := s.deps.Store.LoadByTag(ctx, pair.key, pair.val)
		if err != nil {
			continue
		}
		for _, id := range ids {
			corrSet[id] = true
		}
	}

	if !searched {
		return nil, fmt.Errorf("at least one of ticket, source, or repo is required")
	}

	// Build workflow summaries from projections.
	var results []workflowSummary
	if s.deps.Workflows != nil {
		all := s.deps.Workflows.All()
		for _, ws := range all {
			if !corrSet[ws.AggregateID] {
				continue
			}
			if args.Status != "" && ws.Status != args.Status {
				continue
			}
			summary := workflowSummary{
				AggregateID: ws.AggregateID,
				WorkflowID:  ws.WorkflowID,
				Status:      ws.Status,
				Prompt:      ws.Prompt,
				Source:      ws.Source,
				Ticket:      ws.Ticket,
				FailReason:  ws.FailReason,
				FailPhase:   ws.FailPersona,
				FailureKind: ws.FailureKind,
				FailBackend: ws.FailBackend,
			}
			if !ws.StartedAt.IsZero() {
				summary.StartedAt = ws.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			if !ws.CompletedAt.IsZero() {
				summary.CompletedAt = ws.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			results = append(results, summary)
		}
	}

	return map[string]any{
		"workflows": results,
		"count":     len(results),
	}, nil
}

func (s *Server) toolRetryWorkflow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		WorkflowID string `json:"workflow_id"`
		FromPhase  string `json:"from_phase"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}

	agg, err := s.loadWorkflowAggregate(ctx, args.WorkflowID)
	if err != nil {
		return nil, err
	}
	if agg.Status != engine.StatusFailed && agg.Status != engine.StatusCancelled {
		return nil, fmt.Errorf("can only retry failed or cancelled workflows (current: %s)", agg.Status)
	}

	if args.FromPhase != "" {
		return s.resumeWorkflowFromPhase(ctx, args.WorkflowID, args.FromPhase, agg)
	}

	// Legacy path: full re-run as a fresh workflow, preserving original params.
	events, err := s.deps.Store.Load(ctx, args.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}

	var orig event.WorkflowRequestedPayload
	for _, env := range events {
		if env.Type == event.WorkflowRequested {
			if unmarshalErr := json.Unmarshal(env.Payload, &orig); unmarshalErr != nil {
				return nil, fmt.Errorf("unmarshal original request: %w", unmarshalErr)
			}
			break
		}
	}

	if orig.Prompt == "" {
		return nil, fmt.Errorf("could not find original prompt in workflow events")
	}

	retryArgs := map[string]any{
		"prompt": orig.Prompt,
		"dag":    orig.WorkflowID,
		"source": orig.Source,
		"ticket": orig.Ticket,
	}
	if orig.Repo != "" {
		retryArgs["repo"] = orig.Repo
	}
	if orig.BaseBranch != "" {
		retryArgs["base_branch"] = orig.BaseBranch
	}
	wfArgs, _ := json.Marshal(retryArgs)

	result, err := s.toolRunWorkflow(ctx, wfArgs)
	if err != nil {
		return nil, fmt.Errorf("start retry workflow: %w", err)
	}

	retryResult, ok := result.(runWorkflowResult)
	if !ok {
		return nil, fmt.Errorf("unexpected result type")
	}

	return map[string]any{
		"original_workflow_id": args.WorkflowID,
		"retry_workflow_id":    retryResult.WorkflowID,
		"correlation_id":       retryResult.CorrelationID,
		"dag":                  retryResult.DAG,
		"status":               "started",
	}, nil
}

// resumeWorkflowFromPhase reuses the original correlation and emits a
// WorkflowRetried event. The aggregate clears CompletedPersonas for fromPhase
// + DAG-downstream (computed here, stored in the payload so replay stays
// deterministic), flips Status back to Running, and PersonaRunner re-dispatches
// fromPhase — so upstream completions are preserved and no tokens are re-paid.
func (s *Server) resumeWorkflowFromPhase(
	ctx context.Context,
	workflowID string,
	fromPhase string,
	agg *engine.WorkflowAggregate,
) (any, error) {
	def, ok := s.deps.Engine.GetWorkflowDef(agg.WorkflowID)
	if !ok {
		return nil, fmt.Errorf("workflow definition %q not registered", agg.WorkflowID)
	}
	if _, inGraph := def.Graph[fromPhase]; !inGraph {
		return nil, fmt.Errorf("from_phase %q is not in workflow %q's DAG", fromPhase, agg.WorkflowID)
	}

	invalidated := def.DownstreamOf(fromPhase) // includes fromPhase itself

	retryEvt := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           fromPhase,
		InvalidatedPersonas: invalidated,
		Reason:              "operator retry via mcp",
	})).
		WithAggregate(workflowID, agg.Version+1).
		WithCorrelation(workflowID).
		WithSource("mcp:retry_workflow")

	if err := s.deps.Store.Append(ctx, workflowID, agg.Version, []event.Envelope{retryEvt}); err != nil {
		return nil, fmt.Errorf("store retry event: %w", err)
	}
	if err := s.deps.Bus.Publish(ctx, retryEvt); err != nil {
		return nil, fmt.Errorf("publish retry event: %w", err)
	}

	return map[string]any{
		"original_workflow_id": workflowID,
		"correlation_id":       workflowID,
		"dag":                  agg.WorkflowID,
		"from_phase":           fromPhase,
		"invalidated_personas": invalidated,
		"status":               "resumed",
	}, nil
}

type workflowOutputArgs struct {
	WorkflowID string   `json:"workflow_id"`
	Phases     []string `json:"phases"`
}

type phaseOutput struct {
	Phase      string `json:"phase"`
	Output     string `json:"output"`
	Backend    string `json:"backend"`
	TokensUsed int    `json:"tokens_used"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
}

func (s *Server) toolWorkflowOutput(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workflowOutputArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}

	allEvents, err := s.deps.Store.LoadByCorrelation(ctx, args.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("load correlation events: %w", err)
	}

	// Collect PersonaCompleted → OutputRef mappings keyed by handler name.
	// The "phases" filter parameter is kept for back-compat with callers; values
	// are matched case-insensitively against handler names plus the legacy
	// phase verbs that map to those handlers (so "develop" still resolves to
	// "developer", etc).
	type outputSlot struct {
		ref     string
		persona string
	}
	outputRefs := make(map[string]outputSlot) // persona → slot
	for _, env := range allEvents {
		if env.Type != event.PersonaCompleted {
			continue
		}
		var p event.PersonaCompletedPayload
		if unmarshalErr := json.Unmarshal(env.Payload, &p); unmarshalErr != nil {
			continue
		}
		if p.OutputRef != "" {
			outputRefs[p.Persona] = outputSlot{ref: p.OutputRef, persona: p.Persona}
		}
	}

	// Filter to requested phases. Accept both handler names and the legacy
	// phase-verb aliases; case-insensitive so operators don't have to remember
	// which form they prefer. An empty filter means "return every slot".
	legacyAliases := map[string]string{
		"develop":  "developer",
		"research": "researcher",
		"commit":   "committer",
		"review":   "reviewer",
	}
	phaseFilter := make(map[string]bool, len(args.Phases))
	for _, p := range args.Phases {
		key := strings.ToLower(strings.TrimSpace(p))
		phaseFilter[key] = true
		if alias, ok := legacyAliases[key]; ok {
			phaseFilter[alias] = true
		}
	}
	matchesFilter := func(slot outputSlot) bool {
		if len(phaseFilter) == 0 {
			return true
		}
		return phaseFilter[strings.ToLower(slot.persona)]
	}

	const maxOutputLen = 10000
	var outputs []phaseOutput

	for persona, slot := range outputRefs {
		if !matchesFilter(slot) {
			continue
		}
		ref := slot.ref

		aiEvt, loadErr := s.deps.Store.LoadEvent(ctx, ref)
		if loadErr != nil {
			continue
		}

		var aiPayload event.AIResponsePayload
		if unmarshalErr := json.Unmarshal(aiEvt.Payload, &aiPayload); unmarshalErr != nil {
			continue
		}

		outputStr := extractOutputString(aiPayload.Output)
		truncated := false
		if len(outputStr) > maxOutputLen {
			outputStr = outputStr[:maxOutputLen]
			truncated = true
		}

		outputs = append(outputs, phaseOutput{
			Phase:      persona,
			Output:     outputStr,
			Backend:    aiPayload.Backend,
			TokensUsed: aiPayload.TokensUsed,
			DurationMS: aiPayload.DurationMS,
			Truncated:  truncated,
		})
	}

	return map[string]any{
		"workflow_id": args.WorkflowID,
		"phases":      outputs,
		"count":       len(outputs),
	}, nil
}

func (s *Server) toolDiff(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		WorkflowID string `json:"workflow_id"`
		StatOnly   bool   `json:"stat_only"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}

	// Find the workspace path from WorkspaceReady events.
	allEvents, err := s.deps.Store.LoadByCorrelation(ctx, args.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}

	workDir := ""
	baseBranch := ""
	for _, env := range allEvents {
		if env.Type == event.WorkspaceReady {
			var p event.WorkspaceReadyPayload
			if unmarshalErr := json.Unmarshal(env.Payload, &p); unmarshalErr == nil {
				workDir = p.Path
				baseBranch = p.Base
			}
		}
	}

	if workDir == "" {
		return nil, fmt.Errorf("no workspace found for workflow %s", args.WorkflowID)
	}

	// Validate branch name to prevent git flag injection.
	if baseBranch != "" && !safeBranchRe.MatchString(baseBranch) {
		return nil, fmt.Errorf("invalid branch name: %q", baseBranch)
	}

	// Two-section diff. Bug 2026-04-29: prior single `git diff origin/<base>...HEAD`
	// only showed commits since base, hiding 4 staged files / 356 insertions
	// during operator triage of a paused workflow. Always include the
	// uncommitted (working tree vs HEAD) section so a paused-mid-pipeline
	// workspace is never reported as empty.
	const sectionCap = 25000
	var sections []string

	if baseBranch != "" {
		committed, cerr := runGitDiff(ctx, workDir, args.StatOnly, "origin/"+baseBranch+"...HEAD")
		if cerr != nil {
			committed = "[error: " + cerr.Error() + "]"
		}
		sections = append(sections,
			"=== Committed since origin/"+baseBranch+" ===\n"+capDiffSection(committed, sectionCap))
	}

	uncommitted, uerr := runGitDiff(ctx, workDir, args.StatOnly, "HEAD")
	if uerr != nil {
		uncommitted = "[error: " + uerr.Error() + "]"
	}
	sections = append(sections,
		"=== Uncommitted (staged + unstaged) ===\n"+capDiffSection(uncommitted, sectionCap))

	diff := strings.Join(sections, "\n\n")
	truncated := false
	if len(diff) > 50000 {
		diff = diff[:50000]
		truncated = true
	}

	return map[string]any{
		"workflow_id": args.WorkflowID,
		"workspace":   workDir,
		"diff":        diff,
		"truncated":   truncated,
	}, nil
}

// runGitDiff executes `git -C <workDir> diff [--stat] <ref>` and returns
// stdout. Stderr is intentionally discarded — git writes warnings (e.g.
// missing remote ref) there that would mask the actual diff for callers.
func runGitDiff(ctx context.Context, workDir string, statOnly bool, ref string) (string, error) {
	cmdArgs := []string{"-C", workDir, "diff"}
	if statOnly {
		cmdArgs = append(cmdArgs, "--stat")
	}
	cmdArgs = append(cmdArgs, ref)
	out, err := exec.CommandContext(ctx, "git", cmdArgs...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func capDiffSection(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (section truncated) ...\n"
}

func (s *Server) toolPRDiff(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Repo     string `json:"repo"`
		PRNumber int    `json:"pr_number"`
		StatOnly bool   `json:"stat_only"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if args.PRNumber <= 0 {
		return nil, fmt.Errorf("pr_number is required and must be positive")
	}

	// Validate repo format to prevent argument injection.
	if !safeRepoRe.MatchString(args.Repo) {
		return nil, fmt.Errorf("invalid repo format: %q (expected owner/repo)", args.Repo)
	}

	ghArgs := []string{"pr", "diff", fmt.Sprintf("%d", args.PRNumber), "--repo", args.Repo}
	if args.StatOnly {
		ghArgs = append(ghArgs, "--name-only")
	}

	out, err := exec.CommandContext(ctx, "gh", ghArgs...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr diff: %w", err)
	}

	diff := string(out)
	truncated := false
	if len(diff) > 50000 {
		diff = diff[:50000]
		truncated = true
	}

	return map[string]any{
		"repo":      args.Repo,
		"pr_number": args.PRNumber,
		"diff":      diff,
		"truncated": truncated,
	}, nil
}

func (s *Server) toolCreatePR(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		WorkflowID string `json:"workflow_id"`
		Title      string `json:"title"`
		Draft      bool   `json:"draft"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}

	// Find workspace and ticket from workflow events.
	allEvents, err := s.deps.Store.LoadByCorrelation(ctx, args.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}

	workDir := ""
	baseBranch := "main"
	ticket := ""
	branch := ""

	for _, env := range allEvents {
		switch env.Type {
		case event.WorkspaceReady:
			var p event.WorkspaceReadyPayload
			if unmarshalErr := json.Unmarshal(env.Payload, &p); unmarshalErr == nil {
				workDir = p.Path
				baseBranch = p.Base
				branch = p.Branch
			}
		case event.WorkflowRequested:
			var p event.WorkflowRequestedPayload
			if unmarshalErr := json.Unmarshal(env.Payload, &p); unmarshalErr == nil {
				ticket = p.Ticket
			}
		}
	}

	if workDir == "" {
		return nil, fmt.Errorf("no workspace found for workflow %s", args.WorkflowID)
	}
	if branch == "" {
		return nil, fmt.Errorf("no branch found for workflow %s", args.WorkflowID)
	}
	if !safeBranchRe.MatchString(branch) {
		return nil, fmt.Errorf("invalid branch name: %q", branch)
	}
	if !safeBranchRe.MatchString(baseBranch) {
		return nil, fmt.Errorf("invalid base branch name: %q", baseBranch)
	}

	// Push the branch first.
	pushCmd := exec.CommandContext(ctx, "git", "-C", workDir, "push", "-u", "origin", branch)
	if out, pushErr := pushCmd.CombinedOutput(); pushErr != nil {
		return nil, fmt.Errorf("git push: %s (%w)", strings.TrimSpace(string(out)), pushErr)
	}

	// Build PR title.
	title := args.Title
	if title == "" && ticket != "" {
		title = ticket
		// Try to enrich with Jira summary.
		if s.deps.Jira != nil {
			if issue, fetchErr := s.deps.Jira.FetchIssue(ctx, ticket); fetchErr == nil {
				title = fmt.Sprintf("%s: %s", ticket, issue.Fields.Summary)
			}
		}
	}
	if title == "" {
		wfShort := args.WorkflowID
		if len(wfShort) > 8 {
			wfShort = wfShort[:8]
		}
		title = fmt.Sprintf("Rick workflow %s", wfShort)
	}

	// Create PR via gh CLI.
	ghArgs := []string{"pr", "create",
		"--title", title,
		"--base", baseBranch,
		"--body", fmt.Sprintf("Automated PR from Rick workflow `%s`", args.WorkflowID),
	}
	if args.Draft {
		ghArgs = append(ghArgs, "--draft")
	}

	ghCmd := exec.CommandContext(ctx, "gh", ghArgs...)
	ghCmd.Dir = workDir
	out, err := ghCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	prURL := strings.TrimSpace(string(out))

	return map[string]any{
		"workflow_id": args.WorkflowID,
		"pr_url":      prURL,
		"title":       title,
		"base":        baseBranch,
		"branch":      branch,
	}, nil
}

func (s *Server) toolProjectSync(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Epic string `json:"epic"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Epic == "" {
		return nil, fmt.Errorf("epic is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	children, err := s.deps.Jira.FetchEpicChildren(ctx, args.Epic, true)
	if err != nil {
		return nil, fmt.Errorf("fetch epic children: %w", err)
	}

	// Build dependency graph.
	depGraph := make(map[string][]string) // key → keys it's blocked by
	for _, child := range children {
		links, linkErr := s.deps.Jira.FetchIssueLinks(ctx, child.Key)
		if linkErr != nil {
			continue
		}
		for _, link := range links {
			if link.Type == "Blocks" && link.InwardKey != "" {
				depGraph[child.Key] = append(depGraph[child.Key], link.InwardKey)
			}
		}
	}

	// Build Mermaid diagram.
	var mermaid strings.Builder
	mermaid.WriteString("graph TD\n")

	statusIcon := map[string]string{
		"Done": "✅", "DONE": "✅", "Closed": "✅",
		"IN DEVELOPMENT": "🔧", "In Development": "🔧",
		"WF PEER REVIEW": "👀", "In Review": "👀",
		"TO DO": "📋", "To Do": "📋", "Backlog": "📋",
		"Cancelled": "❌", "CANCELLED": "❌",
	}

	for _, child := range children {
		icon := statusIcon[child.Status]
		if icon == "" {
			icon = "⏳"
		}
		fmt.Fprintf(&mermaid, "    %s[\"%s %s: %s\"]\n", child.Key, icon, child.Key, child.Summary)
	}

	for key, blockedBy := range depGraph {
		for _, blocker := range blockedBy {
			fmt.Fprintf(&mermaid, "    %s --> %s\n", blocker, key)
		}
	}

	// Build status table.
	var table strings.Builder
	table.WriteString("| Ticket | Summary | Status | Assignee | Points |\n")
	table.WriteString("|--------|---------|--------|----------|--------|\n")

	var totalPoints float64
	done := 0
	inProgress := 0

	for _, child := range children {
		fmt.Fprintf(&table, "| %s | %s | %s | %s | %.0f |\n",
			child.Key, child.Summary, child.Status, child.Assignee, child.Points)
		totalPoints += child.Points
		switch child.Status {
		case "Done", "DONE", "Closed":
			done++
		case "IN DEVELOPMENT", "In Development", "WF PEER REVIEW", "In Review":
			inProgress++
		}
	}

	diagram := mermaid.String()
	statusTable := table.String()
	summary := fmt.Sprintf("Progress: %d/%d done, %d in progress. Total: %.0f points.",
		done, len(children), inProgress, totalPoints)

	return map[string]any{
		"epic":     args.Epic,
		"mermaid":  diagram,
		"table":    statusTable,
		"summary":  summary,
		"total":    len(children),
		"done":     done,
		"progress": inProgress,
		"points":   totalPoints,
	}, nil
}

