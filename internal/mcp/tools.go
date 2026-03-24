package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// prURLRegexp matches GitHub PR URLs anywhere in text.
var prURLRegexp = regexp.MustCompile(`https?://[^/]*github[^/]*/([^/]+)/([^/]+)/pull/(\d+)`)

// extractPRURL finds a GitHub PR URL in text and returns owner, repo, PR number.
func extractPRURL(text string) (owner, repo string, prNumber int) {
	m := prURLRegexp.FindStringSubmatch(text)
	if m == nil {
		return "", "", 0
	}
	n, _ := strconv.Atoi(m[3])
	return m[1], m[2], n
}

// prSourceRegexp matches "gh:owner/repo#N" source references.
var prSourceRegexp = regexp.MustCompile(`^gh:([^#]+)#(\d+)$`)

// resolvePRBranch parses a "gh:owner/repo#N" source and returns the PR's head
// branch via `gh pr view`. Returns empty string if source is not a PR reference
// or if the lookup fails (non-fatal — workspace falls back to ticket-as-branch).
func resolvePRBranch(ctx context.Context, source string) string {
	m := prSourceRegexp.FindStringSubmatch(source)
	if m == nil {
		return ""
	}
	fullRepo, prNumber := m[1], m[2]
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber,
		"--repo", fullRepo,
		"--json", "headRefName",
		"-q", ".headRefName")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ToolDefinition describes an MCP tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolHandler executes a tool call and returns a JSON-serializable result.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool binds a definition to its handler.
type Tool struct {
	Definition ToolDefinition
	Handler    ToolHandler
}

// Deps holds the dependencies needed by MCP tools.
type Deps struct {
	Store          eventstore.Store
	Bus            eventbus.Bus
	Engine         *engine.Engine
	Workflows      *projection.WorkflowStatusProjection
	Tokens         *projection.TokenUsageProjection
	Timelines      *projection.PhaseTimelineProjection
	Verdicts       *projection.VerdictProjection
	SelectWorkflow func(name string) (engine.WorkflowDef, error)
	BackendName    string
	WorkDir        string
	Yolo           bool

	// Extended deps for Tier 1-5 tools.
	Backend    backend.Backend
	Jira       *jira.Client
	Confluence *confluence.Client
}

func (s *Server) registerBuiltinTools() { //nolint:funlen // tool registration is intentionally verbose

	// Register tool groups from separate files.
	s.registerJobTools()
	s.registerWorkspaceTools()
	s.registerJiraTools()
	s.registerWaveTools()
	s.registerObservabilityTools()
	s.registerConfluenceTools()

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_run_workflow",
			Description: "Start a new Rick AI workflow. All code-producing workflows provision an isolated workspace first and commit at the end. Runs a full pipeline (workspace, research, architect, develop, review, QA, commit) or a subset based on the selected DAG. For BTU technical planning use rick_plan_btu instead.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "The task description or requirements for the workflow.",
					},
					"dag": map[string]any{
						"type":        "string",
						"description": "Which workflow DAG to use. Call rick_list_workflows to see all registered workflows (built-in and plugin-provided). Defaults to 'workspace-dev'.",
						"default":     "workspace-dev",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Source reference for the workflow, e.g. 'gh:owner/repo#123' for a GitHub PR.",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": "Repository in 'owner/repo' format.",
					},
					"pr_number": map[string]any{
						"type":        "integer",
						"description": "Pull request number (used with repo for PR-based workflows).",
					},
					"ticket": map[string]any{
						"type":        "string",
						"description": "Issue tracker ticket ID, e.g. 'PROJ-123'.",
					},
				},
				"required": []string{"prompt"},
			},
		},
		Handler: s.toolRunWorkflow,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workflow_status",
			Description: "Get the current status of a workflow by replaying its events into the aggregate state machine. Shows workflow status, phase states, iterations, and token usage.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolWorkflowStatus,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_list_workflows",
			Description: "List available workflow DAGs (definitions) and all tracked workflow runs with their current status.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		Handler: s.toolListWorkflows,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_list_events",
			Description: "List events for a specific workflow (by aggregate ID) or globally. Returns event type, timestamp, source, and payload summary.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "Filter events by workflow aggregate ID. Omit for global event stream.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of events to return.",
						"default":     50,
					},
				},
			},
		},
		Handler: s.toolListEvents,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_token_usage",
			Description: "Get token consumption for a workflow, broken down by phase and AI backend.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolTokenUsage,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_phase_timeline",
			Description: "Get timing and iteration details for each phase in a workflow. Shows start/end times, duration, and iteration count.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolPhaseTimeline,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workflow_verdicts",
			Description: "Get review verdicts for a workflow. Shows pass/fail outcomes, summaries, and detailed issues from reviewer and QA phases.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolWorkflowVerdicts,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_persona_output",
			Description: "Get the AI response output for a specific persona in a workflow. Returns the raw LLM output text, optionally truncated.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"persona": map[string]any{
						"type":        "string",
						"description": "The persona name (e.g. 'developer', 'reviewer').",
					},
					"max_length": map[string]any{
						"type":        "integer",
						"description": "Maximum characters to return. Defaults to 10000.",
						"default":     10000,
					},
				},
				"required": []string{"workflow_id", "persona"},
			},
		},
		Handler: s.toolPersonaOutput,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_list_dead_letters",
			Description: "List all dead letter entries (events that failed delivery). Shows event ID, handler, error, and attempt count.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		Handler: s.toolListDeadLetters,
	})

	// --- Operator Intervention Tools ---

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_cancel_workflow",
			Description: "Cancel a running workflow. In-flight personas complete but no new personas are dispatched.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID to cancel.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Reason for cancellation.",
						"default":     "operator requested",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolCancelWorkflow,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_pause_workflow",
			Description: "Pause a running workflow. In-flight personas complete but new dispatches are blocked until resumed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID to pause.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Reason for pausing.",
						"default":     "operator requested",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolPauseWorkflow,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_resume_workflow",
			Description: "Resume a paused workflow. Blocked persona dispatches are replayed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID to resume.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Reason for resuming.",
					},
				},
				"required": []string{"workflow_id"},
			},
		},
		Handler: s.toolResumeWorkflow,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_inject_guidance",
			Description: "Inject operator guidance into a paused workflow. The guidance becomes part of the context for the next persona invocation. Auto-resumes by default.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Guidance text for the next persona.",
					},
					"target": map[string]any{
						"type":        "string",
						"description": "Target persona (optional, defaults to next in chain).",
					},
					"auto_resume": map[string]any{
						"type":        "boolean",
						"description": "Resume workflow after injecting guidance.",
						"default":     true,
					},
				},
				"required": []string{"workflow_id", "content"},
			},
		},
		Handler: s.toolInjectGuidance,
	})

	// --- BTU Planning ---

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_plan_btu",
			Description: "Start a BTU technical planning workflow. Reads a Confluence BTU page, researches the codebase, generates a technical implementation plan with Fibonacci story point estimates in Spanish, and writes it back to Confluence. The workflow pauses twice for human review: once after generating the plan, and once after estimating story points. Use rick_approve_hint or rick_reject_hint to review.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"page_id": map[string]any{
						"type":        "string",
						"description": "Confluence page ID (numeric) or full Confluence page URL.",
					},
					"ticket": map[string]any{
						"type":        "string",
						"description": "Ticket reference, e.g. 'BTU-1724'. Optional but recommended for tracking.",
					},
					"prompt": map[string]any{
						"type":        "string",
						"description": "Additional context or instructions for the planning workflow. Optional.",
					},
				},
				"required": []string{"page_id"},
			},
		},
		Handler: s.toolPlanBTU,
	})

	// --- Hint Approval/Rejection ---

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_approve_hint",
			Description: "Approve a pending hint for a paused workflow. The workflow pauses when a persona emits a hint (draft plan, estimates) for human review. Approving triggers full execution. Optionally include guidance to adjust the persona's behavior.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"persona": map[string]any{
						"type":        "string",
						"description": "The persona whose hint to approve (e.g. 'plan-architect', 'estimator').",
					},
					"guidance": map[string]any{
						"type":        "string",
						"description": "Optional guidance to adjust the persona's behavior before full execution.",
					},
				},
				"required": []string{"workflow_id", "persona"},
			},
		},
		Handler: s.toolApproveHint,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_reject_hint",
			Description: "Reject a pending hint for a paused workflow. Use action 'skip' to mark the persona as complete without full execution, or 'fail' to fail the entire workflow.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{
						"type":        "string",
						"description": "The workflow aggregate ID.",
					},
					"persona": map[string]any{
						"type":        "string",
						"description": "The persona whose hint to reject.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Reason for rejection.",
					},
					"action": map[string]any{
						"type":        "string",
						"description": "What to do: 'skip' (mark persona complete without execution) or 'fail' (fail the workflow).",
						"default":     "skip",
					},
				},
				"required": []string{"workflow_id", "persona"},
			},
		},
		Handler: s.toolRejectHint,
	})
}

func (s *Server) register(t Tool) {
	s.tools[t.Definition.Name] = t
}

// --- Tool Handlers ---

type runWorkflowArgs struct {
	Prompt   string `json:"prompt"`
	DAG      string `json:"dag"`
	Source   string `json:"source,omitempty"`
	Repo     string `json:"repo,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	Ticket   string `json:"ticket,omitempty"`
}

type runWorkflowResult struct {
	WorkflowID    string `json:"workflow_id"`
	CorrelationID string `json:"correlation_id"`
	DAG           string `json:"dag"`
	Status        string `json:"status"`
}

func isWorkflowRegistered(eng *engine.Engine, id string) bool {
	for _, def := range eng.RegisteredWorkflows() {
		if def.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) toolRunWorkflow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args runWorkflowArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if args.DAG == "" {
		args.DAG = "workspace-dev"
	}

	// Auto-select jira-dev when ticket is provided but the caller used the
	// default DAG. jira-dev fetches ticket context from Jira (including repo
	// resolution from labels/components) before provisioning the workspace —
	// workspace-dev would silently fail because it has no repo info.
	if args.Ticket != "" && args.DAG == "workspace-dev" {
		args.DAG = "jira-dev"
	}

	// Validate and register the workflow definition if not already registered
	// Ensure the workflow is registered. Built-in workflows are pre-registered at
	// startup; plugin-provided workflows are registered dynamically via gRPC when
	// the plugin connects. Either way, the engine is the source of truth.
	if !isWorkflowRegistered(s.deps.Engine, args.DAG) {
		// Try to register a built-in def (handles standalone mcp mode where
		// serve.go pre-registration hasn't run).
		if s.deps.SelectWorkflow != nil {
			if def, err := s.deps.SelectWorkflow(args.DAG); err == nil {
				s.deps.Engine.RegisterWorkflow(def)
			}
		}
		if !isWorkflowRegistered(s.deps.Engine, args.DAG) {
			return nil, fmt.Errorf("workflow %q not registered — start the required plugin or use rick_list_workflows to see available workflows", args.DAG)
		}
	}

	aggregateID := uuid.New().String()
	correlationID := aggregateID

	source := args.Source
	if source == "" {
		if args.Repo != "" && args.PRNumber > 0 {
			source = fmt.Sprintf("gh:%s#%d", args.Repo, args.PRNumber)
		} else if owner, repo, pr := extractPRURL(args.Prompt); pr > 0 {
			// User pasted a GitHub PR URL in the prompt — extract it.
			source = fmt.Sprintf("gh:%s/%s#%d", owner, repo, pr)
			if args.Repo == "" {
				args.Repo = owner + "/" + repo
			}
		} else {
			source = "mcp"
		}
	}

	// Resolve PR head branch when source is a PR reference (gh:owner/repo#N).
	// This ensures ci-fix and other PR-scoped workflows check out the correct branch.
	repoBranch := resolvePRBranch(ctx, source)

	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     args.Prompt,
		WorkflowID: args.DAG,
		Source:     source,
		Repo:       args.Repo,
		Ticket:     args.Ticket,
		RepoBranch: repoBranch,
	})).
		WithAggregate(aggregateID, 1).
		WithCorrelation(correlationID).
		WithSource("mcp:run_workflow")

	if err := s.deps.Store.Append(ctx, aggregateID, 0, []event.Envelope{reqEvt}); err != nil {
		return nil, fmt.Errorf("store workflow requested: %w", err)
	}
	if err := s.deps.Bus.Publish(ctx, reqEvt); err != nil {
		return nil, fmt.Errorf("publish workflow requested: %w", err)
	}

	return runWorkflowResult{
		WorkflowID:    aggregateID,
		CorrelationID: correlationID,
		DAG:           args.DAG,
		Status:        "started",
	}, nil
}

type workflowStatusArgs struct {
	WorkflowID string `json:"workflow_id"`
}

type pendingHintSummary struct {
	Persona       string   `json:"persona"`
	Confidence    float64  `json:"confidence"`
	Plan          string   `json:"plan"`
	Blockers      []string `json:"blockers,omitempty"`
	TokenEstimate int      `json:"token_estimate"`
	EventID       string   `json:"event_id"`
}

type runningPhaseSummary struct {
	Phase     string `json:"phase"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

type workflowStatusResult struct {
	ID                string               `json:"id"`
	Status            string               `json:"status"`
	WorkflowID        string               `json:"workflow_id"`
	Version           int                  `json:"version"`
	TokensUsed        int                  `json:"tokens_used"`
	CompletedPersonas map[string]bool      `json:"completed_personas"`
	FeedbackCount     map[string]int       `json:"feedback_count"`
	PendingHints      []pendingHintSummary `json:"pending_hints,omitempty"`
	RunningPhases     []runningPhaseSummary `json:"running_phases,omitempty"`
}

func (s *Server) toolWorkflowStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workflowStatusArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}

	events, err := s.deps.Store.Load(ctx, args.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("workflow not found: %s", args.WorkflowID)
	}

	agg := engine.NewWorkflowAggregate(args.WorkflowID)
	for _, env := range events {
		agg.Apply(env)
	}

	result := workflowStatusResult{
		ID:                agg.ID,
		Status:            string(agg.Status),
		WorkflowID:        agg.WorkflowID,
		Version:           agg.Version,
		TokensUsed:        agg.TokensUsed,
		CompletedPersonas: agg.CompletedPersonas,
		FeedbackCount:     agg.FeedbackCount,
	}

	// Scan for pending hints when paused (low-cost: only on paused workflows).
	if agg.Status == engine.StatusPaused {
		result.PendingHints = s.findPendingHints(ctx, args.WorkflowID)
	}

	// Show in-progress phases for running workflows.
	if agg.Status == engine.StatusRunning && s.deps.Timelines != nil {
		for _, pt := range s.deps.Timelines.ForWorkflow(args.WorkflowID) {
			if pt.Status == "running" && !pt.StartedAt.IsZero() {
				result.RunningPhases = append(result.RunningPhases, runningPhaseSummary{
					Phase:     pt.Phase,
					ElapsedMS: time.Since(pt.StartedAt).Milliseconds(),
				})
			}
		}
	}

	return result, nil
}

type listWorkflowsResult struct {
	AvailableDAGs []dagSummary      `json:"available_dags"`
	Workflows     []workflowSummary `json:"workflows"`
}

type dagSummary struct {
	ID                string   `json:"id"`
	Required          []string `json:"required_phases"`
	MaxIterations     int      `json:"max_iterations"`
	EscalateOnMaxIter bool     `json:"escalate_on_max_iter,omitempty"`
}

type workflowSummary struct {
	AggregateID string `json:"aggregate_id"`
	WorkflowID  string `json:"workflow_id"`
	Status      string `json:"status"`
	Prompt      string `json:"prompt,omitempty"`
	Source      string `json:"source,omitempty"`
	Ticket      string `json:"ticket,omitempty"`
	FailReason  string `json:"fail_reason,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// findPendingHints scans correlation events for HintEmitted without matching
// HintApproved/HintRejected. Only called for paused workflows.
func (s *Server) findPendingHints(ctx context.Context, correlationID string) []pendingHintSummary {
	allEvents, err := s.deps.Store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return nil
	}

	// Collect all HintEmitted events, then remove those with matching approvals/rejections.
	emitted := make(map[string]pendingHintSummary) // persona → hint
	for _, env := range allEvents {
		switch env.Type {
		case event.HintEmitted:
			var h event.HintEmittedPayload
			if err := json.Unmarshal(env.Payload, &h); err == nil {
				emitted[h.Persona] = pendingHintSummary{
					Persona:       h.Persona,
					Confidence:    h.Confidence,
					Plan:          h.Plan,
					Blockers:      h.Blockers,
					TokenEstimate: h.TokenEstimate,
					EventID:       string(env.ID),
				}
			}
		case event.HintApproved:
			var h event.HintApprovedPayload
			if err := json.Unmarshal(env.Payload, &h); err == nil {
				delete(emitted, h.Persona)
			}
		case event.HintRejected:
			var h event.HintRejectedPayload
			if err := json.Unmarshal(env.Payload, &h); err == nil {
				delete(emitted, h.Persona)
			}
		}
	}

	if len(emitted) == 0 {
		return nil
	}
	hints := make([]pendingHintSummary, 0, len(emitted))
	for _, h := range emitted {
		hints = append(hints, h)
	}
	return hints
}

func (s *Server) toolListWorkflows(_ context.Context, _ json.RawMessage) (any, error) {
	if s.deps.Workflows == nil {
		return nil, fmt.Errorf("workflow projection not available")
	}

	// Available workflow definitions — includes both built-in and
	// dynamically registered (via gRPC) workflows.
	registered := s.deps.Engine.RegisteredWorkflows()
	dags := make([]dagSummary, 0, len(registered))
	for _, def := range registered {
		dags = append(dags, dagSummary{
			ID:                def.ID,
			Required:          def.Required,
			MaxIterations:     def.MaxIterations,
			EscalateOnMaxIter: def.EscalateOnMaxIter,
		})
	}

	// Active/past workflow runs.
	all := s.deps.Workflows.All()
	summaries := make([]workflowSummary, 0, len(all))
	for _, ws := range all {
		summary := workflowSummary{
			AggregateID: ws.AggregateID,
			WorkflowID:  ws.WorkflowID,
			Status:      ws.Status,
			Prompt:      ws.Prompt,
			Source:      ws.Source,
			Ticket:      ws.Ticket,
			FailReason:  ws.FailReason,
		}
		if !ws.StartedAt.IsZero() {
			summary.StartedAt = ws.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if !ws.CompletedAt.IsZero() {
			summary.CompletedAt = ws.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		summaries = append(summaries, summary)
	}

	return listWorkflowsResult{AvailableDAGs: dags, Workflows: summaries}, nil
}

type listEventsArgs struct {
	WorkflowID string `json:"workflow_id"`
	Limit      int    `json:"limit"`
}

type eventSummary struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Version       int    `json:"version"`
	Timestamp     string `json:"timestamp"`
	Source        string `json:"source"`
	CorrelationID string `json:"correlation_id,omitempty"`
	AggregateID   string `json:"aggregate_id,omitempty"`
}

type listEventsResult struct {
	Events []eventSummary `json:"events"`
	Count  int            `json:"count"`
}

func (s *Server) toolListEvents(ctx context.Context, raw json.RawMessage) (any, error) {
	var args listEventsArgs
	if raw != nil {
		_ = json.Unmarshal(raw, &args)
	}
	if args.Limit <= 0 {
		args.Limit = 50
	}

	var envelopes []event.Envelope

	if args.WorkflowID != "" {
		events, err := s.deps.Store.Load(ctx, args.WorkflowID)
		if err != nil {
			return nil, fmt.Errorf("load events: %w", err)
		}
		envelopes = events
	} else {
		positioned, err := s.deps.Store.LoadAll(ctx, 0, args.Limit)
		if err != nil {
			return nil, fmt.Errorf("load all events: %w", err)
		}
		for _, pe := range positioned {
			envelopes = append(envelopes, pe.Event)
		}
	}

	// Apply limit for per-workflow queries
	if len(envelopes) > args.Limit {
		envelopes = envelopes[len(envelopes)-args.Limit:]
	}

	summaries := make([]eventSummary, 0, len(envelopes))
	for _, env := range envelopes {
		summaries = append(summaries, eventSummary{
			ID:            string(env.ID),
			Type:          string(env.Type),
			Version:       env.Version,
			Timestamp:     env.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
			Source:        env.Source,
			CorrelationID: env.CorrelationID,
			AggregateID:   env.AggregateID,
		})
	}

	return listEventsResult{
		Events: summaries,
		Count:  len(summaries),
	}, nil
}

type tokenUsageArgs struct {
	WorkflowID string `json:"workflow_id"`
}

type tokenUsageResult struct {
	WorkflowID string         `json:"workflow_id"`
	Total      int            `json:"total"`
	ByPhase    map[string]int `json:"by_phase"`
	ByBackend  map[string]int `json:"by_backend"`
}

func (s *Server) toolTokenUsage(_ context.Context, raw json.RawMessage) (any, error) {
	var args tokenUsageArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if s.deps.Tokens == nil {
		return nil, fmt.Errorf("token usage projection not available")
	}

	usage, ok := s.deps.Tokens.Get(args.WorkflowID)
	if !ok {
		return tokenUsageResult{
			WorkflowID: args.WorkflowID,
			ByPhase:    map[string]int{},
			ByBackend:  map[string]int{},
		}, nil
	}

	return tokenUsageResult{
		WorkflowID: args.WorkflowID,
		Total:      usage.Total,
		ByPhase:    usage.ByPhase,
		ByBackend:  usage.ByBackend,
	}, nil
}

type phaseTimelineArgs struct {
	WorkflowID string `json:"workflow_id"`
}

type phaseTimelineEntry struct {
	Phase       string `json:"phase"`
	Status      string `json:"status"`
	Iterations  int    `json:"iterations"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	ElapsedMS   int64  `json:"elapsed_ms,omitempty"` // for running phases: wall-clock since start
}

type phaseTimelineResult struct {
	WorkflowID string               `json:"workflow_id"`
	Phases     []phaseTimelineEntry `json:"phases"`
}

func (s *Server) toolPhaseTimeline(_ context.Context, raw json.RawMessage) (any, error) {
	var args phaseTimelineArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if s.deps.Timelines == nil {
		return nil, fmt.Errorf("phase timeline projection not available")
	}

	timelines := s.deps.Timelines.ForWorkflow(args.WorkflowID)
	entries := make([]phaseTimelineEntry, 0, len(timelines))
	for _, pt := range timelines {
		entry := phaseTimelineEntry{
			Phase:      pt.Phase,
			Status:     pt.Status,
			Iterations: pt.Iterations,
			DurationMS: pt.Duration.Milliseconds(),
		}
		if !pt.StartedAt.IsZero() {
			entry.StartedAt = pt.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if !pt.CompletedAt.IsZero() {
			entry.CompletedAt = pt.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if pt.Status == "running" && !pt.StartedAt.IsZero() {
			entry.ElapsedMS = time.Since(pt.StartedAt).Milliseconds()
		}
		entries = append(entries, entry)
	}

	return phaseTimelineResult{
		WorkflowID: args.WorkflowID,
		Phases:     entries,
	}, nil
}

type deadLetterSummary struct {
	ID       string `json:"id"`
	EventID  string `json:"event_id"`
	Handler  string `json:"handler"`
	Error    string `json:"error"`
	Attempts int    `json:"attempts"`
	FailedAt string `json:"failed_at"`
}

type listDeadLettersResult struct {
	DeadLetters []deadLetterSummary `json:"dead_letters"`
	Count       int                 `json:"count"`
}

func (s *Server) toolListDeadLetters(ctx context.Context, _ json.RawMessage) (any, error) {
	dls, err := s.deps.Store.LoadDeadLetters(ctx)
	if err != nil {
		return nil, fmt.Errorf("load dead letters: %w", err)
	}

	summaries := make([]deadLetterSummary, 0, len(dls))
	for _, dl := range dls {
		summaries = append(summaries, deadLetterSummary{
			ID:       dl.ID,
			EventID:  dl.EventID,
			Handler:  dl.Handler,
			Error:    dl.Error,
			Attempts: dl.Attempts,
			FailedAt: dl.FailedAt,
		})
	}

	return listDeadLettersResult{
		DeadLetters: summaries,
		Count:       len(summaries),
	}, nil
}

// --- Operator Intervention Tool Handlers ---

type interventionArgs struct {
	WorkflowID string `json:"workflow_id"`
	Reason     string `json:"reason"`
}

type interventionResult struct {
	WorkflowID string `json:"workflow_id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
}

// loadWorkflowAggregate loads a workflow aggregate, validating it exists.
func (s *Server) loadWorkflowAggregate(ctx context.Context, workflowID string) (*engine.WorkflowAggregate, error) {
	events, err := s.deps.Store.Load(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("workflow not found: %s", workflowID)
	}
	agg := engine.NewWorkflowAggregate(workflowID)
	for _, env := range events {
		agg.Apply(env)
	}
	return agg, nil
}

func (s *Server) toolCancelWorkflow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args interventionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if args.Reason == "" {
		args.Reason = "operator requested"
	}

	agg, err := s.loadWorkflowAggregate(ctx, args.WorkflowID)
	if err != nil {
		return nil, err
	}
	if agg.Status != engine.StatusRunning && agg.Status != engine.StatusPaused && agg.Status != engine.StatusRequested {
		return nil, fmt.Errorf("cannot cancel workflow in %s state", agg.Status)
	}

	cancelEvt := event.New(event.WorkflowCancelled, 1, event.MustMarshal(event.WorkflowCancelledPayload{
		Reason: args.Reason,
		Source: "mcp",
	})).
		WithAggregate(args.WorkflowID, agg.Version+1).
		WithCorrelation(args.WorkflowID).
		WithSource("mcp:cancel_workflow")

	if err := s.deps.Store.Append(ctx, args.WorkflowID, agg.Version, []event.Envelope{cancelEvt}); err != nil {
		return nil, fmt.Errorf("store cancel event: %w", err)
	}
	if err := s.deps.Bus.Publish(ctx, cancelEvt); err != nil {
		return nil, fmt.Errorf("publish cancel event: %w", err)
	}

	return interventionResult{
		WorkflowID: args.WorkflowID,
		Action:     "cancelled",
		Status:     string(engine.StatusCancelled),
	}, nil
}

func (s *Server) toolPauseWorkflow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args interventionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if args.Reason == "" {
		args.Reason = "operator requested"
	}

	agg, err := s.loadWorkflowAggregate(ctx, args.WorkflowID)
	if err != nil {
		return nil, err
	}
	if agg.Status != engine.StatusRunning {
		return nil, fmt.Errorf("cannot pause workflow in %s state", agg.Status)
	}

	pauseEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: args.Reason,
		Source: "operator",
	})).
		WithAggregate(args.WorkflowID, agg.Version+1).
		WithCorrelation(args.WorkflowID).
		WithSource("mcp:pause_workflow")

	if err := s.deps.Store.Append(ctx, args.WorkflowID, agg.Version, []event.Envelope{pauseEvt}); err != nil {
		return nil, fmt.Errorf("store pause event: %w", err)
	}
	if err := s.deps.Bus.Publish(ctx, pauseEvt); err != nil {
		return nil, fmt.Errorf("publish pause event: %w", err)
	}

	return interventionResult{
		WorkflowID: args.WorkflowID,
		Action:     "paused",
		Status:     string(engine.StatusPaused),
	}, nil
}

func (s *Server) toolResumeWorkflow(ctx context.Context, raw json.RawMessage) (any, error) {
	var args interventionArgs
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
	if agg.Status != engine.StatusPaused {
		return nil, fmt.Errorf("cannot resume workflow in %s state", agg.Status)
	}

	resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
		Reason: args.Reason,
	})).
		WithAggregate(args.WorkflowID, agg.Version+1).
		WithCorrelation(args.WorkflowID).
		WithSource("mcp:resume_workflow")

	if err := s.deps.Store.Append(ctx, args.WorkflowID, agg.Version, []event.Envelope{resumeEvt}); err != nil {
		return nil, fmt.Errorf("store resume event: %w", err)
	}
	if err := s.deps.Bus.Publish(ctx, resumeEvt); err != nil {
		return nil, fmt.Errorf("publish resume event: %w", err)
	}

	return interventionResult{
		WorkflowID: args.WorkflowID,
		Action:     "resumed",
		Status:     string(engine.StatusRunning),
	}, nil
}

type guidanceArgs struct {
	WorkflowID string `json:"workflow_id"`
	Content    string `json:"content"`
	Target     string `json:"target"`
	AutoResume *bool  `json:"auto_resume"`
}

type guidanceResult struct {
	WorkflowID string `json:"workflow_id"`
	Action     string `json:"action"`
	Resumed    bool   `json:"resumed"`
}

func (s *Server) toolInjectGuidance(ctx context.Context, raw json.RawMessage) (any, error) {
	var args guidanceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if args.Content == "" {
		return nil, fmt.Errorf("content is required")
	}

	autoResume := true
	if args.AutoResume != nil {
		autoResume = *args.AutoResume
	}

	agg, err := s.loadWorkflowAggregate(ctx, args.WorkflowID)
	if err != nil {
		return nil, err
	}
	if agg.Status != engine.StatusPaused && agg.Status != engine.StatusRunning {
		return nil, fmt.Errorf("cannot inject guidance into workflow in %s state", agg.Status)
	}

	var allEvents []event.Envelope
	nextVersion := agg.Version + 1

	guidanceEvt := event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
		Content:    args.Content,
		Target:     args.Target,
		AutoResume: autoResume,
	})).
		WithAggregate(args.WorkflowID, nextVersion).
		WithCorrelation(args.WorkflowID).
		WithSource("mcp:inject_guidance")
	allEvents = append(allEvents, guidanceEvt)

	resumed := false
	if autoResume && agg.Status == engine.StatusPaused {
		nextVersion++
		resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
			Reason: "auto-resume after guidance injection",
		})).
			WithAggregate(args.WorkflowID, nextVersion).
			WithCorrelation(args.WorkflowID).
			WithSource("mcp:inject_guidance")
		allEvents = append(allEvents, resumeEvt)
		resumed = true
	}

	if err := s.deps.Store.Append(ctx, args.WorkflowID, agg.Version, allEvents); err != nil {
		return nil, fmt.Errorf("store guidance events: %w", err)
	}
	for _, evt := range allEvents {
		if pubErr := s.deps.Bus.Publish(ctx, evt); pubErr != nil {
			return nil, fmt.Errorf("publish event: %w", pubErr)
		}
	}

	return guidanceResult{
		WorkflowID: args.WorkflowID,
		Action:     "guidance_injected",
		Resumed:    resumed,
	}, nil
}

// --- BTU Planning Tool ---

type planBTUArgs struct {
	PageID string `json:"page_id"`
	Ticket string `json:"ticket,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

type planBTUResult struct {
	WorkflowID string `json:"workflow_id"`
	PageID     string `json:"page_id"`
	Ticket     string `json:"ticket"`
	Status     string `json:"status"`
}

func (s *Server) toolPlanBTU(ctx context.Context, raw json.RawMessage) (any, error) {
	var args planBTUArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.PageID == "" {
		return nil, fmt.Errorf("page_id is required")
	}

	pageID := args.PageID
	if idx := strings.LastIndex(pageID, "/pages/"); idx >= 0 {
		rest := pageID[idx+7:]
		if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
			pageID = rest[:slashIdx]
		} else {
			pageID = rest
		}
	}

	if s.deps.SelectWorkflow != nil {
		def, err := s.deps.SelectWorkflow("plan-btu")
		if err != nil {
			return nil, fmt.Errorf("plan-btu workflow not available: %w", err)
		}
		s.deps.Engine.RegisterWorkflow(def)
	}

	prompt := args.Prompt
	if prompt == "" {
		prompt = fmt.Sprintf("Generate technical implementation plan for Confluence page %s", pageID)
	}

	aggregateID := uuid.New().String()
	source := "confluence:" + pageID

	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     prompt,
		WorkflowID: "plan-btu",
		Source:     source,
		Ticket:     args.Ticket,
	})).
		WithAggregate(aggregateID, 1).
		WithCorrelation(aggregateID).
		WithSource("mcp:plan_btu")

	if err := s.deps.Store.Append(ctx, aggregateID, 0, []event.Envelope{reqEvt}); err != nil {
		return nil, fmt.Errorf("store workflow requested: %w", err)
	}
	if err := s.deps.Bus.Publish(ctx, reqEvt); err != nil {
		return nil, fmt.Errorf("publish workflow requested: %w", err)
	}

	return planBTUResult{
		WorkflowID: aggregateID,
		PageID:     pageID,
		Ticket:     args.Ticket,
		Status:     "started",
	}, nil
}

// --- Verdict Tool ---

type workflowVerdictsArgs struct {
	WorkflowID string `json:"workflow_id"`
}

type verdictIssueResult struct {
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
}

type verdictResult struct {
	Phase       string               `json:"phase"`
	SourcePhase string               `json:"source_phase"`
	Outcome     string               `json:"outcome"`
	Summary     string               `json:"summary"`
	Issues      []verdictIssueResult `json:"issues,omitempty"`
}

type workflowVerdictsResult struct {
	WorkflowID string          `json:"workflow_id"`
	Verdicts   []verdictResult `json:"verdicts"`
	Count      int             `json:"count"`
}

func (s *Server) toolWorkflowVerdicts(_ context.Context, raw json.RawMessage) (any, error) {
	var args workflowVerdictsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if s.deps.Verdicts == nil {
		return nil, fmt.Errorf("verdict projection not available")
	}

	records := s.deps.Verdicts.ForWorkflow(args.WorkflowID)
	verdicts := make([]verdictResult, 0, len(records))
	for _, r := range records {
		vr := verdictResult{
			Phase:       r.Phase,
			SourcePhase: r.SourcePhase,
			Outcome:     r.Outcome,
			Summary:     r.Summary,
		}
		if len(r.Issues) > 0 {
			vr.Issues = make([]verdictIssueResult, len(r.Issues))
			for i, iss := range r.Issues {
				vr.Issues[i] = verdictIssueResult{
					Severity:    iss.Severity,
					Category:    iss.Category,
					Description: iss.Description,
					File:        iss.File,
					Line:        iss.Line,
				}
			}
		}
		verdicts = append(verdicts, vr)
	}

	return workflowVerdictsResult{
		WorkflowID: args.WorkflowID,
		Verdicts:   verdicts,
		Count:      len(verdicts),
	}, nil
}

// --- Persona Output Tool ---

type personaOutputArgs struct {
	WorkflowID string `json:"workflow_id"`
	Persona    string `json:"persona"`
	MaxLength  int    `json:"max_length"`
}

type personaOutputResult struct {
	WorkflowID string `json:"workflow_id"`
	Persona    string `json:"persona"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	Backend    string `json:"backend"`
	TokensUsed int    `json:"tokens_used"`
	DurationMS int64  `json:"duration_ms"`
}

func (s *Server) toolPersonaOutput(ctx context.Context, raw json.RawMessage) (any, error) {
	var args personaOutputArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if args.Persona == "" {
		return nil, fmt.Errorf("persona is required")
	}
	if args.MaxLength <= 0 {
		args.MaxLength = 10000
	}

	// Scan all events correlated to this workflow to find PersonaCompleted for the target persona.
	allEvents, err := s.deps.Store.LoadByCorrelation(ctx, args.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("load correlation events: %w", err)
	}

	outputRef := ""
	for _, env := range allEvents {
		if env.Type != event.PersonaCompleted {
			continue
		}
		var p event.PersonaCompletedPayload
		if unmarshalErr := json.Unmarshal(env.Payload, &p); unmarshalErr != nil {
			continue
		}
		if p.Persona == args.Persona && p.OutputRef != "" {
			outputRef = p.OutputRef
			// Keep the last one to handle retries — later completions win.
		}
	}

	if outputRef == "" {
		return nil, fmt.Errorf("no output available for persona %q in workflow %s", args.Persona, args.WorkflowID)
	}

	aiEvt, err := s.deps.Store.LoadEvent(ctx, outputRef)
	if err != nil {
		return nil, fmt.Errorf("load AI response event: %w", err)
	}

	var aiPayload event.AIResponsePayload
	if err := json.Unmarshal(aiEvt.Payload, &aiPayload); err != nil {
		return nil, fmt.Errorf("unmarshal AI response payload: %w", err)
	}

	// The Output field is json.RawMessage — it may be a JSON-quoted string or raw text.
	outputStr := extractOutputString(aiPayload.Output)

	truncated := false
	if len(outputStr) > args.MaxLength {
		outputStr = outputStr[:args.MaxLength]
		truncated = true
	}

	return personaOutputResult{
		WorkflowID: args.WorkflowID,
		Persona:    args.Persona,
		Output:     outputStr,
		Truncated:  truncated,
		Backend:    aiPayload.Backend,
		TokensUsed: aiPayload.TokensUsed,
		DurationMS: aiPayload.DurationMS,
	}, nil
}

// extractOutputString attempts to unquote a JSON string from raw output.
// Falls back to raw bytes if the payload is not a JSON-encoded string.
func extractOutputString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// --- Hint Approval/Rejection Tools ---

type hintActionArgs struct {
	WorkflowID string `json:"workflow_id"`
	Persona    string `json:"persona"`
	Guidance   string `json:"guidance,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Action     string `json:"action,omitempty"`
}

type hintActionResult struct {
	WorkflowID string `json:"workflow_id"`
	Persona    string `json:"persona"`
	Action     string `json:"action"`
	Status     string `json:"status"`
}

func (s *Server) toolApproveHint(ctx context.Context, raw json.RawMessage) (any, error) {
	var args hintActionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" || args.Persona == "" {
		return nil, fmt.Errorf("workflow_id and persona are required")
	}

	agg, err := s.loadWorkflowAggregate(ctx, args.WorkflowID)
	if err != nil {
		return nil, err
	}
	if agg.Status != engine.StatusPaused && agg.Status != engine.StatusRunning {
		return nil, fmt.Errorf("cannot approve hint for workflow in %s state", agg.Status)
	}

	// Find the HintEmitted event to get the trigger ID for replay
	triggerID := ""
	events, loadErr := s.deps.Store.LoadByCorrelation(ctx, args.WorkflowID)
	if loadErr == nil {
		for _, evt := range events {
			if evt.Type == event.HintEmitted {
				var hint event.HintEmittedPayload
				if unmarshalErr := json.Unmarshal(evt.Payload, &hint); unmarshalErr == nil {
					if hint.Persona == args.Persona {
						triggerID = hint.TriggerID
					}
				}
			}
		}
	}

	var allEvents []event.Envelope
	nextVersion := agg.Version + 1

	approveEvt := event.New(event.HintApproved, 1, event.MustMarshal(event.HintApprovedPayload{
		Persona:   args.Persona,
		TriggerID: triggerID,
		Guidance:  args.Guidance,
	})).
		WithAggregate(args.WorkflowID, nextVersion).
		WithCorrelation(args.WorkflowID).
		WithSource("mcp:approve_hint")
	allEvents = append(allEvents, approveEvt)

	// Auto-resume if paused
	if agg.Status == engine.StatusPaused {
		nextVersion++
		resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
			Reason: fmt.Sprintf("hint approved for %s", args.Persona),
		})).
			WithAggregate(args.WorkflowID, nextVersion).
			WithCorrelation(args.WorkflowID).
			WithSource("mcp:approve_hint")
		allEvents = append(allEvents, resumeEvt)
	}

	if err := s.deps.Store.Append(ctx, args.WorkflowID, agg.Version, allEvents); err != nil {
		return nil, fmt.Errorf("store hint approval: %w", err)
	}
	for _, evt := range allEvents {
		if pubErr := s.deps.Bus.Publish(ctx, evt); pubErr != nil {
			return nil, fmt.Errorf("publish event: %w", pubErr)
		}
	}

	return hintActionResult{
		WorkflowID: args.WorkflowID,
		Persona:    args.Persona,
		Action:     "approved",
		Status:     "running",
	}, nil
}

func (s *Server) toolRejectHint(ctx context.Context, raw json.RawMessage) (any, error) {
	var args hintActionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.WorkflowID == "" || args.Persona == "" {
		return nil, fmt.Errorf("workflow_id and persona are required")
	}
	if args.Action == "" {
		args.Action = "skip"
	}
	if args.Action != "skip" && args.Action != "fail" {
		return nil, fmt.Errorf("action must be 'skip' or 'fail'")
	}

	agg, err := s.loadWorkflowAggregate(ctx, args.WorkflowID)
	if err != nil {
		return nil, err
	}

	var allEvents []event.Envelope
	nextVersion := agg.Version + 1

	rejectEvt := event.New(event.HintRejected, 1, event.MustMarshal(event.HintRejectedPayload{
		Persona: args.Persona,
		Reason:  args.Reason,
		Action:  args.Action,
	})).
		WithAggregate(args.WorkflowID, nextVersion).
		WithCorrelation(args.WorkflowID).
		WithSource("mcp:reject_hint")
	allEvents = append(allEvents, rejectEvt)

	// Auto-resume if paused
	if agg.Status == engine.StatusPaused {
		nextVersion++
		resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
			Reason: fmt.Sprintf("hint rejected for %s (action=%s)", args.Persona, args.Action),
		})).
			WithAggregate(args.WorkflowID, nextVersion).
			WithCorrelation(args.WorkflowID).
			WithSource("mcp:reject_hint")
		allEvents = append(allEvents, resumeEvt)
	}

	if err := s.deps.Store.Append(ctx, args.WorkflowID, agg.Version, allEvents); err != nil {
		return nil, fmt.Errorf("store hint rejection: %w", err)
	}
	for _, evt := range allEvents {
		if pubErr := s.deps.Bus.Publish(ctx, evt); pubErr != nil {
			return nil, fmt.Errorf("publish event: %w", pubErr)
		}
	}

	return hintActionResult{
		WorkflowID: args.WorkflowID,
		Persona:    args.Persona,
		Action:     "rejected:" + args.Action,
		Status:     "running",
	}, nil
}
