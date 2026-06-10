package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// registerConsolidatedTools registers the facade tools that multiplex over the
// fine-grained handlers. Each facade keeps a flat input schema and routes via a
// simple string enum or include-list, so the LLM never has to choose among a
// dozen near-identical tool names. The underlying handlers (toolWorkflowStatus,
// toolWavePlan, …) remain the single source of truth — the facades only
// dispatch to them.
func (s *Server) registerConsolidatedTools() {
	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workflow_inspect",
			Description: "Read workflow data. With workflow_id: per-workflow panels (status, timeline, tokens, verdicts, output, persona_output). Without workflow_id: global panels (list = workflow runs + DAG definitions, filterable by ticket/source/repo/status; events = event stream; dead_letters = failed deliveries). Pass an include list to pick panels; defaults to status when a workflow_id is given, otherwise list.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID. Required for per-workflow panels; omit for global panels (list/events/dead_letters).",
					},
					"include": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"status", "timeline", "tokens", "verdicts", "output", "persona_output", "list", "events", "dead_letters"},
						},
						"description": "Which panels to include. Per-workflow: status (default w/ id), timeline, tokens, verdicts, output (all persona outputs), persona_output (one persona, requires persona). Global: list (default w/o id), events, dead_letters.",
					},
					"persona": map[string]any{
						"type":        "string",
						"description": "Persona name. Required only when include contains 'persona_output'.",
					},
					"max_length": map[string]any{
						"type":        "integer",
						"description": "Max characters for persona_output. Defaults to 10000.",
						"default":     10000,
					},
					"phases": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Filter 'output' to specific phases (optional).",
					},
					"ticket": map[string]any{"type": "string", "description": "list panel: filter runs by Jira ticket key."},
					"source": map[string]any{"type": "string", "description": "list panel: filter runs by source reference (e.g. gh:owner/repo#123)."},
					"repo":   map[string]any{"type": "string", "description": "list panel: filter runs by repository name."},
					"status": map[string]any{
						"type":        "string",
						"enum":        []string{"running", "completed", "failed", "paused", "cancelled"},
						"description": "list panel: filter runs by status (only applies when a ticket/source/repo filter is also set).",
					},
					"limit": map[string]any{"type": "integer", "description": "events panel: max events to return.", "default": 50},
				},
			},
		},
		Handler: s.toolWorkflowInspect,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workflow_control",
			Description: "Act on a workflow. action=pause|resume|cancel governs dispatch (in-flight personas always complete). action=retry restarts a failed/cancelled workflow (from_phase to re-dispatch at a phase, preserving upstream completions). action=inject_guidance feeds operator guidance to the next persona (content required; target/auto_resume optional). action=approve_hint triggers full persona execution (persona required; optional guidance). action=reject_hint skips or fails a pending hint (persona required; reject_action=skip|fail).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"pause", "resume", "cancel", "retry", "inject_guidance", "approve_hint", "reject_hint"},
						"description": "The control verb. See the tool description for per-action arguments.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "pause/resume/cancel/reject_hint: reason for the action.",
						"default":     "operator requested",
					},
					"from_phase": map[string]any{
						"type":        "string",
						"description": "retry only: handler name to re-dispatch at (must be in the DAG). Omit to start a fresh run with the original parameters.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "inject_guidance only: guidance text for the next persona.",
					},
					"target": map[string]any{
						"type":        "string",
						"description": "inject_guidance only: target persona (defaults to next in chain).",
					},
					"auto_resume": map[string]any{
						"type":        "boolean",
						"description": "inject_guidance only: resume the workflow after injecting (default true).",
						"default":     true,
					},
					"persona": map[string]any{
						"type":        "string",
						"description": "approve_hint/reject_hint only: the persona whose hint to act on.",
					},
					"guidance": map[string]any{
						"type":        "string",
						"description": "approve_hint only: optional guidance to adjust the persona before full execution.",
					},
					"reject_action": map[string]any{
						"type":        "string",
						"enum":        []string{"skip", "fail"},
						"description": "reject_hint only: skip (mark persona complete) or fail (fail the workflow). Defaults to skip.",
					},
				},
				"required": []string{"workflow_id", "action"},
			},
		},
		Handler: s.toolWorkflowControl,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_diff_viewer",
			Description: "Fetch code diffs. Provide workflow_id for a workflow's workspace git diff, or repo + pr_number for a GitHub PR diff via the gh CLI (no workflow/workspace needed).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID (for a workspace diff).",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": "Repository in owner/repo format (for a PR diff).",
					},
					"pr_number": map[string]any{
						"type":        "integer",
						"description": "Pull request number (for a PR diff).",
					},
					"stat_only": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "Show only the diffstat, not the full diff.",
					},
				},
			},
		},
		Handler: s.toolDiffViewer,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_job_inspect",
			Description: "Inspect async jobs spawned by rick_consult or rick_run. With job_id: status and output for that job (output supports incremental reads via offset). Without job_id: include=list returns all tracked jobs, include=backends returns the AI backends rick knows about and which are active. Defaults to status+output when a job_id is given, otherwise list.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID returned by rick_consult or rick_run. Omit for the global list/backends panels.",
					},
					"include": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"status", "output", "list", "backends"},
						},
						"description": "Which panels to include. With job_id: status, output (default both). Without job_id: list (all jobs, default), backends (known/active AI backends).",
					},
					"offset": map[string]any{
						"type":        "integer",
						"default":     0,
						"description": "output only: character offset to start reading from (incremental reads).",
					},
					"max_length": map[string]any{
						"type":        "integer",
						"default":     50000,
						"description": "output only: maximum characters to return.",
					},
				},
			},
		},
		Handler: s.toolJobInspect,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_manager",
			Description: "Manage parallel development waves for a Jira epic or GitHub parent/project. action=plan computes waves via topological sort; launch starts a wave's workflows; status monitors progress; cleanup removes wave workspaces; pr_links returns cross-referenced PRs per child (issue for a single ref, or a wave source). All actions accept the same source shape: epic shorthand, or source={type,parent|project|epic,...}. Action-specific args: launch takes wave/dag/tickets/dry_run; status takes wave; cleanup takes wave/force; pr_links takes issue/wave.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"plan", "launch", "status", "cleanup", "pr_links"},
						"description": "plan: compute waves; launch: start a wave's workflows; status: monitor; cleanup: remove workspaces; pr_links: cross-referenced PRs per child.",
					},
					"epic":   map[string]any{"type": "string", "description": "Jira epic key (back-compat shorthand for source.type='jira')."},
					"source": waveSourceSchema(),
					"issue":  map[string]any{"type": "string", "description": "pr_links only: single GitHub issue ref 'owner/repo#N'. Mutually exclusive with source/epic."},
					"wave":   map[string]any{"type": "integer", "description": "Wave number. For launch: omit for next ready wave. For status/cleanup/pr_links: optional."},
					"dag":    map[string]any{"type": "string", "description": "launch only: workflow DAG for each child (defaults by source kind)."},
					"tickets": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "launch only: subset of child IDs to launch ('PROJ-123' or 'owner/repo#N').",
					},
					"dry_run": map[string]any{"type": "boolean", "default": false, "description": "launch only: plan the launch without starting workflows."},
					"force":   map[string]any{"type": "boolean", "default": false, "description": "cleanup only: remove workspaces even if dirty."},
				},
				"required": []string{"action"},
			},
		},
		Handler: s.toolWaveManager,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workspace",
			Description: "Manage isolated local repository clones under $RICK_REPOS_PATH. action=setup creates a clone and checks out a branch (repo + ticket required; optional isolate/suffix/base) — ALWAYS use before code-writing jobs. action=cleanup removes a workspace (path OR correlation_id). action=list shows every workspace with branch + dirty status. Deletion is guarded to *-rick-ws-* paths under $RICK_REPOS_PATH.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"setup", "cleanup", "list"},
						"description": "setup: create an isolated clone; cleanup: remove a workspace; list: show all workspaces.",
					},
					"repo":           map[string]any{"type": "string", "description": "setup: repository name under $RICK_REPOS_PATH (e.g. 'backend')."},
					"ticket":         map[string]any{"type": "string", "description": "setup: Jira ticket ID for the branch name (e.g. 'PROJ-12345')."},
					"isolate":        map[string]any{"type": "boolean", "default": true, "description": "setup: create isolated local clone (ALWAYS true for code-writing jobs)."},
					"suffix":         map[string]any{"type": "string", "description": "setup: optional suffix for parallel tasks on the same repo."},
					"base":           map[string]any{"type": "string", "default": "main", "description": "setup: base branch to create from (branch created from origin/<base>)."},
					"path":           map[string]any{"type": "string", "description": "cleanup: absolute workspace path. Mutually exclusive with correlation_id."},
					"correlation_id": map[string]any{"type": "string", "description": "cleanup: workflow correlation ID; resolves the path from the WorkspaceReady event. Mutually exclusive with path."},
				},
				"required": []string{"action"},
			},
		},
		Handler: s.toolWorkspace,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_confluence",
			Description: "Read or write a Confluence page. action=read returns title, body, version, space key for a page (page_id required, accepts a numeric ID or full URL). action=write replaces the content under a heading (page_id + content + after_heading required; content is markdown converted to storage format). Requires CONFLUENCE_URL/EMAIL/TOKEN.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"read", "write"},
						"description": "read: fetch a page; write: replace the section under a heading.",
					},
					"page_id":       map[string]any{"type": "string", "description": "Confluence page ID (numeric) or full page URL."},
					"content":       map[string]any{"type": "string", "description": "write only: content to write (markdown or HTML)."},
					"after_heading": map[string]any{"type": "string", "description": "write only: insert after this heading (e.g. 'Plan Tecnico')."},
				},
				"required": []string{"action", "page_id"},
			},
		},
		Handler: s.toolConfluence,
	})
}

// --- rick_workflow_inspect ---

type workflowInspectArgs struct {
	WorkflowID string   `json:"workflow_id"`
	Include    []string `json:"include"`
	Persona    string   `json:"persona"`
	Ticket     string   `json:"ticket"`
	Source     string   `json:"source"`
	Repo       string   `json:"repo"`
	Status     string   `json:"status"`
}

func (s *Server) toolWorkflowInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workflowInspectArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	include := args.Include
	if len(include) == 0 {
		if args.WorkflowID != "" {
			include = []string{"status"}
		} else {
			include = []string{"list"}
		}
	}

	result := map[string]any{}
	if args.WorkflowID != "" {
		result["workflow_id"] = args.WorkflowID
	}
	errs := map[string]string{}

	// run dispatches one sub-handler and records its output or error under
	// `panel`. We never swallow a sub-handler error: a partial fetch must tell
	// the caller which panel failed and why, so a single bad panel can't be
	// mistaken for "no data".
	run := func(panel string, fn func(context.Context, json.RawMessage) (any, error)) {
		out, err := fn(ctx, raw)
		if err != nil {
			errs[panel] = err.Error()
			return
		}
		result[panel] = out
	}

	// requireID guards the per-workflow panels: without a workflow_id they have
	// nothing to read, so record a panel error rather than dispatching.
	requireID := func(panel string, fn func(context.Context, json.RawMessage) (any, error)) {
		if args.WorkflowID == "" {
			errs[panel] = fmt.Sprintf("workflow_id is required for panel %q", panel)
			return
		}
		run(panel, fn)
	}

	for _, inc := range include {
		switch inc {
		case "status":
			requireID("status", s.toolWorkflowStatus)
		case "timeline":
			requireID("timeline", s.toolPhaseTimeline)
		case "tokens":
			requireID("tokens", s.toolTokenUsage)
		case "verdicts":
			requireID("verdicts", s.toolWorkflowVerdicts)
		case "output":
			requireID("output", s.toolWorkflowOutput)
		case "persona_output":
			if args.WorkflowID == "" {
				errs["persona_output"] = "workflow_id is required for panel \"persona_output\""
				continue
			}
			if args.Persona == "" {
				errs["persona_output"] = "persona is required when include contains 'persona_output'"
				continue
			}
			run("persona_output", s.toolPersonaOutput)
		case "list":
			// "list" absorbs the former rick_search_workflows: when any business
			// key filter is present, route to the tag-index search; otherwise
			// return the full run + DAG-definition listing.
			if args.Ticket != "" || args.Source != "" || args.Repo != "" || args.Status != "" {
				run("list", s.toolSearchWorkflows)
			} else {
				run("list", s.toolListWorkflows)
			}
		case "events":
			run("events", s.toolListEvents)
		case "dead_letters":
			run("dead_letters", s.toolListDeadLetters)
		default:
			errs[inc] = fmt.Sprintf("unknown include panel: %q", inc)
		}
	}

	if len(errs) > 0 {
		result["errors"] = errs
	}
	return result, nil
}

// --- rick_workflow_control ---

type workflowControlArgs struct {
	WorkflowID string `json:"workflow_id"`
	Action     string `json:"action"`
}

func (s *Server) toolWorkflowControl(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workflowControlArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	switch args.Action {
	case "pause":
		return s.toolPauseWorkflow(ctx, raw)
	case "resume":
		return s.toolResumeWorkflow(ctx, raw)
	case "cancel":
		return s.toolCancelWorkflow(ctx, raw)
	case "retry":
		return s.toolRetryWorkflow(ctx, raw)
	case "inject_guidance":
		return s.toolInjectGuidance(ctx, raw)
	case "approve_hint":
		return s.toolApproveHint(ctx, raw)
	case "reject_hint":
		// toolRejectHint reads its skip/fail decision from an "action" key, which
		// collides with this facade's discriminator. Remap the caller-facing
		// "reject_action" onto the "action" the handler expects.
		return s.toolRejectHint(ctx, remapRejectAction(raw))
	default:
		return nil, fmt.Errorf("invalid action %q: must be pause, resume, cancel, retry, inject_guidance, approve_hint, or reject_hint", args.Action)
	}
}

// remapRejectAction rewrites {action:"reject_hint", reject_action:"skip"|"fail"}
// into the {action:"skip"|"fail"} shape toolRejectHint unmarshals. On any parse
// failure it falls back to the original bytes so the handler surfaces the error.
func remapRejectAction(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	ra, _ := m["reject_action"].(string)
	if ra == "" {
		ra = "skip"
	}
	m["action"] = ra
	delete(m, "reject_action")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// --- rick_diff_viewer ---

type diffViewerArgs struct {
	WorkflowID string `json:"workflow_id"`
	Repo       string `json:"repo"`
	PRNumber   int    `json:"pr_number"`
}

func (s *Server) toolDiffViewer(ctx context.Context, raw json.RawMessage) (any, error) {
	var args diffViewerArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.WorkflowID != "" {
		return s.toolDiff(ctx, raw)
	}
	if args.Repo != "" && args.PRNumber > 0 {
		return s.toolPRDiff(ctx, raw)
	}
	return nil, fmt.Errorf("must provide either workflow_id (workspace diff) or both repo and pr_number (PR diff)")
}

// --- rick_job_inspect ---

type jobInspectArgs struct {
	JobID   string   `json:"job_id"`
	Include []string `json:"include"`
}

func (s *Server) toolJobInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jobInspectArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	include := args.Include
	if len(include) == 0 {
		if args.JobID != "" {
			include = []string{"status", "output"}
		} else {
			include = []string{"list"}
		}
	}

	result := map[string]any{}
	if args.JobID != "" {
		result["job_id"] = args.JobID
	}
	errs := map[string]string{}

	run := func(panel string, fn func(context.Context, json.RawMessage) (any, error)) {
		out, err := fn(ctx, raw)
		if err != nil {
			errs[panel] = err.Error()
			return
		}
		result[panel] = out
	}

	requireJob := func(panel string, fn func(context.Context, json.RawMessage) (any, error)) {
		if args.JobID == "" {
			errs[panel] = fmt.Sprintf("job_id is required for panel %q", panel)
			return
		}
		run(panel, fn)
	}

	for _, inc := range include {
		switch inc {
		case "status":
			requireJob("status", s.toolJobStatus)
		case "output":
			requireJob("output", s.toolJobOutput)
		case "list":
			run("list", s.toolJobsList)
		case "backends":
			run("backends", s.toolBackends)
		default:
			errs[inc] = fmt.Sprintf("unknown include panel: %q", inc)
		}
	}

	if len(errs) > 0 {
		result["errors"] = errs
	}
	return result, nil
}

// --- rick_wave_manager ---

type waveManagerArgs struct {
	Action string `json:"action"`
}

func (s *Server) toolWaveManager(ctx context.Context, raw json.RawMessage) (any, error) {
	var args waveManagerArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Each underlying handler unmarshals only the fields it needs from raw and
	// ignores the extra "action" key, so passing raw through unchanged is safe.
	switch args.Action {
	case "plan":
		return s.toolWavePlan(ctx, raw)
	case "launch":
		return s.toolWaveLaunch(ctx, raw)
	case "status":
		return s.toolWaveStatus(ctx, raw)
	case "cleanup":
		return s.toolWaveCleanup(ctx, raw)
	case "pr_links":
		return s.toolGitHubPRLinks(ctx, raw)
	default:
		return nil, fmt.Errorf("invalid action %q: must be plan, launch, status, cleanup, or pr_links", args.Action)
	}
}

// --- rick_workspace ---

type workspaceArgs struct {
	Action string `json:"action"`
}

func (s *Server) toolWorkspace(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workspaceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	switch args.Action {
	case "setup":
		return s.toolWorkspaceSetup(ctx, raw)
	case "cleanup":
		return s.toolWorkspaceCleanup(ctx, raw)
	case "list":
		return s.toolWorkspaceList(ctx, raw)
	default:
		return nil, fmt.Errorf("invalid action %q: must be setup, cleanup, or list", args.Action)
	}
}

// --- rick_confluence ---

type confluenceArgs struct {
	Action string `json:"action"`
}

func (s *Server) toolConfluence(ctx context.Context, raw json.RawMessage) (any, error) {
	var args confluenceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	switch args.Action {
	case "read":
		return s.toolConfluenceRead(ctx, raw)
	case "write":
		return s.toolConfluenceWrite(ctx, raw)
	default:
		return nil, fmt.Errorf("invalid action %q: must be read or write", args.Action)
	}
}
