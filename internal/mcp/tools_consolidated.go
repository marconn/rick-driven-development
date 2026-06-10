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
			Description: "Inspect a workflow's observability data. Pass an include list to fetch exactly the panels you need in one call: status, timeline, tokens, verdicts, output, persona_output. Defaults to status when include is omitted.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"include": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"status", "timeline", "tokens", "verdicts", "output", "persona_output"},
						},
						"description": "Which panels to include. 'status' is the default if omitted. 'output' returns all persona outputs; 'persona_output' returns one persona (requires the persona arg).",
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
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolWorkflowInspect,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workflow_control",
			Description: "Control a workflow's execution. Set action to pause, resume, or cancel. In-flight personas always complete; the action only governs new dispatches.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"pause", "resume", "cancel"},
						"description": "pause: block new dispatches; resume: replay blocked dispatches; cancel: stop dispatching new personas.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Reason for the action.",
						"default":     "operator requested",
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
			Description: "Inspect an async job spawned by rick_consult or rick_run. Returns status and output by default; pass include to narrow to one. Output supports incremental reads via offset.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID returned by rick_consult or rick_run.",
					},
					"include": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"status", "output"},
						},
						"description": "Which panels to include. Defaults to both status and output.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"default":     0,
						"description": "Character offset to start reading output from (incremental reads).",
					},
					"max_length": map[string]any{
						"type":        "integer",
						"default":     50000,
						"description": "Maximum characters of output to return.",
					},
				},
				"required": []string{"job_id"},
			},
		},
		Handler: s.toolJobInspect,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_manager",
			Description: "Manage parallel development waves for a Jira epic or GitHub parent/project. Set action to plan (compute waves via topological sort), launch (start workflows for a wave), status (monitor progress), or cleanup (remove wave workspaces). All actions accept the same source shape: epic shorthand, or source={type,parent|project|epic,...}. Action-specific args: launch takes wave/dag/tickets/dry_run; status takes wave; cleanup takes wave/force.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"plan", "launch", "status", "cleanup"},
						"description": "plan: compute waves; launch: start a wave's workflows; status: monitor; cleanup: remove workspaces.",
					},
					"epic":   map[string]any{"type": "string", "description": "Jira epic key (back-compat shorthand for source.type='jira')."},
					"source": waveSourceSchema(),
					"wave":   map[string]any{"type": "integer", "description": "Wave number. For launch: omit for next ready wave. For status/cleanup: optional."},
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
}

// --- rick_workflow_inspect ---

type workflowInspectArgs struct {
	WorkflowID string   `json:"workflow_id"`
	Include    []string `json:"include"`
	Persona    string   `json:"persona"`
}

func (s *Server) toolWorkflowInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workflowInspectArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}

	include := args.Include
	if len(include) == 0 {
		include = []string{"status"}
	}

	result := map[string]any{"workflow_id": args.WorkflowID}
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

	for _, inc := range include {
		switch inc {
		case "status":
			run("status", s.toolWorkflowStatus)
		case "timeline":
			run("timeline", s.toolPhaseTimeline)
		case "tokens":
			run("tokens", s.toolTokenUsage)
		case "verdicts":
			run("verdicts", s.toolWorkflowVerdicts)
		case "output":
			run("output", s.toolWorkflowOutput)
		case "persona_output":
			if args.Persona == "" {
				errs["persona_output"] = "persona is required when include contains 'persona_output'"
				continue
			}
			run("persona_output", s.toolPersonaOutput)
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
	default:
		return nil, fmt.Errorf("invalid action %q: must be pause, resume, or cancel", args.Action)
	}
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
	if args.JobID == "" {
		return nil, fmt.Errorf("job_id is required")
	}

	include := args.Include
	if len(include) == 0 {
		include = []string{"status", "output"}
	}

	result := map[string]any{"job_id": args.JobID}
	errs := map[string]string{}

	run := func(panel string, fn func(context.Context, json.RawMessage) (any, error)) {
		out, err := fn(ctx, raw)
		if err != nil {
			errs[panel] = err.Error()
			return
		}
		result[panel] = out
	}

	for _, inc := range include {
		switch inc {
		case "status":
			run("status", s.toolJobStatus)
		case "output":
			run("output", s.toolJobOutput)
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
	default:
		return nil, fmt.Errorf("invalid action %q: must be plan, launch, status, or cleanup", args.Action)
	}
}
