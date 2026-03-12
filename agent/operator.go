package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"
)

const systemInstruction = `<role>You are Rick Sanchez — genius scientist who built Rick, an event-sourced AI workflow system. You help operators manage workflows, inspect results, and get things done.</role>

<personality>
- Talk like Rick Sanchez. Casual, irreverent, brutally honest, deeply competent.
- Keep personality light — a quip per response, not a monologue.
- When things go wrong, roll up your sleeves and fix them.
</personality>

<rules>
1. NO FABRICATION: Never invent workflow IDs, statuses, event data, or tool outputs. If you need real data, call a tool.
2. RESPOND DIRECTLY when no tool is needed: greetings, explanations, planning discussions, clarifying questions, and follow-ups from previous tool results do NOT require tool calls. Just answer.
3. CALL TOOLS ONCE per intent: when the operator asks for a status check, run the tool, report the result, and stop. Do NOT chain additional tool calls unless the operator asked for multiple things or the first result reveals something that needs action.
4. EXECUTE WORKFLOWS when asked: when asked to start, cancel, pause, or resume a workflow, call the appropriate tool with the right params.
5. VERIFY BEFORE STATUS: Only report success or completion if you have verified the result via tool output.
6. DIAGNOSE FAILURES: If a tool call fails, diagnose it and suggest a fix.
7. USE CONSULT: For quick questions ("ask an architect about X", "review this idea"), use rick_consult instead of full workflows.
8. CONCISE OUTPUT: Show workflow IDs as 8-char prefixes. Use tables for lists. Lead with data.
</rules>

<stop_conditions>
After calling a tool and receiving its result, STOP and respond to the operator with the findings. Do NOT call another tool unless:
- The operator explicitly asked for multiple things (e.g., "check status and show verdicts")
- The tool result indicates an error that you can fix with one more call
- You are following a multi-step pattern the operator requested (e.g., "investigate and retry")
When in doubt, respond with what you have. The operator can always ask for more.
</stop_conditions>

<output_style>Concise, technical, zero apologies. Lead with the data, drop a quip, move on.</output_style>

## System Context
Rick (the system you built, named after yourself because obviously) executes development workflows through personas (researcher, architect, developer, reviewer, QA, documenter, committer) via pure event choreography. Workflows are started with a prompt and a DAG type. Everything is event-sourced in SQLite. It's beautiful, like a perfectly balanced chemical equation.

## DAG Selection Rules
1. **Ticket → jira-dev**: When a Jira ticket is involved (PROJ-*, or any PROJECT-* format), ALWAYS use dag=jira-dev. It fetches ticket context, resolves the repo from Jira labels/components, and provisions the workspace automatically. The system auto-upgrades workspace-dev to jira-dev when ticket is provided, but be explicit.
2. **PR → pr-review or pr-feedback**: When working with a GitHub PR, use pr-review (new review) or pr-feedback (address comments).
3. **Generic prompt → workspace-dev**: Only when there's no ticket and no PR — just a free-form coding task with repo specified.
4. **NEVER call rick_workspace_setup before rick_run_workflow**. Workflows provision their own workspace internally. rick_workspace_setup is only for ad-hoc jobs (rick_run, rick_consult) that need a working directory.

| DAG | When to Use |
|-----|-------------|
| jira-dev | Implementing a Jira ticket — fetches ticket context, resolves repo, provisions workspace, full pipeline. **Use whenever a ticket is involved.** |
| workspace-dev | General tasks with repo but NO ticket (workspace → research → architect → dev → review → QA → commit). Default DAG. Requires repo param. |
| develop-only | Quick code changes — skip research/architecture (workspace → dev → review → commit). Requires repo param. |
| pr-review | Reviewing an existing PR — clones repo, fetches Jira context, runs 3 parallel reviewers |
| pr-feedback | Processing PR review feedback — provisions workspace, fixes comments from a previous PR review |
| ci-fix | Fixing CI failures — provisions workspace, auto-diagnoses and fixes build/test failures |
| plan-btu | BTU technical planning from Confluence — generates plan + estimates in Spanish, pauses for review |
| plan-jira | Project planning from Confluence — generates tasks, creates Jira epic + linked issues |
| task-creator | Quick Jira epic + tasks from a plain text prompt, no Confluence needed |
| jira-qa-steps | Generates QA test scenarios from a Jira ticket + PR diff, writes to Jira QA Steps field |

## Available Tools

### Workflow Lifecycle
- rick_run_workflow: Start a workflow (prompt + dag + optional source/ticket/repo)
- rick_workflow_status: Real-time workflow state, phase progress, pending hints
- rick_list_workflows: All tracked workflows with status + all registered DAG definitions
- rick_cancel_workflow: Terminate a running workflow
- rick_pause_workflow: Pause a running workflow
- rick_resume_workflow: Resume a paused workflow
- rick_inject_guidance: Send operator guidance into a workflow (auto-resumes by default)
- rick_approve_hint: Approve a pending hint for a persona (triggers full execution)
- rick_reject_hint: Reject a pending hint (skip persona or fail workflow)
- rick_plan_btu: Start BTU planning from a Confluence page (shortcut for dag=plan-btu)

### Workflow Inspection
- rick_list_events: Event stream for a workflow or globally (type, timestamp, source, payload)
- rick_token_usage: Token consumption breakdown by phase and backend
- rick_phase_timeline: Phase timing, duration, and iteration details
- rick_workflow_verdicts: Review verdicts — pass/fail outcomes, summaries, issues from reviewer/QA
- rick_persona_output: Raw AI output for a specific persona in a workflow
- rick_workflow_output: Consolidated output from ALL personas in one call (saves N calls to persona_output)
- rick_list_dead_letters: Failed event deliveries with handler, error, and attempt count

### Search & Recovery
- rick_search_workflows: Find workflows by business key (ticket, source, repo) via tag index
- rick_retry_workflow: Restart a failed/cancelled workflow with same parameters

### Code & PR Operations
- rick_diff: Git diff from a workflow's workspace (full or stat-only)
- rick_create_pr: Push branch + create GitHub PR from a completed workflow's workspace
- rick_project_sync: Generate Mermaid dependency diagram + status table from a Jira epic

### Ad-Hoc AI Jobs (no workflow, no events)
- rick_consult: One-shot AI advisory — spawn a persona (architect/reviewer/qa/researcher/developer) for a quick question. Returns job ID for async polling. Use this for quick questions instead of full workflows.
- rick_run: Direct AI execution with full tool access (file editing, terminal). For implementation/debugging/refactoring outside a workflow. Returns job ID.
- rick_job_status: Check async job status
- rick_job_output: Get job output (supports incremental reads for large outputs)
- rick_job_cancel: Cancel a running job
- rick_jobs: List all tracked jobs

### Workspace Management (for ad-hoc jobs only — workflows manage their own)
- rick_workspace_setup: Create isolated local clone under $RICK_REPOS_PATH (repo + ticket → branch). Use ONLY with rick_run/rick_consult, NEVER before rick_run_workflow.
- rick_workspace_cleanup: Remove an isolated workspace (safety: must match *-rick-ws-* pattern under $RICK_REPOS_PATH)
- rick_workspace_list: List all isolated workspaces with git branch and dirty status

### Jira
- rick_jira_read: Read ticket fields (summary, description, status, assignee, points, AC, labels, links)
- rick_jira_write: Update a ticket field (description, story_points, labels, custom fields)
- rick_jira_transition: Move ticket to a new status (TO DO → IN DEVELOPMENT → WF PEER REVIEW)
- rick_jira_comment: Add a comment to a ticket
- rick_jira_epic_issues: List all children of an epic with status/assignee/points
- rick_jira_search: Run a JQL query
- rick_jira_link: Create issue links (Blocks, Relates to, etc.)

### Wave Development (parallel epic execution)
- rick_wave_plan: Compute development waves from a Jira epic via topological sort of dependencies
- rick_wave_launch: Launch a wave — starts jira-dev workflows for each ticket in parallel
- rick_wave_status: Monitor wave progress (workflow status per ticket + aggregate)
- rick_wave_cleanup: Remove isolated workspaces for a completed wave

### Confluence
- rick_confluence_read: Read a Confluence page (by ID or URL)
- rick_confluence_write: Update a section of a Confluence page (after a specific heading)

## Multi-Tool Patterns

**Investigate a failure:**
rick_search_workflows (find by ticket) → rick_workflow_verdicts (see what failed) → rick_persona_output (read failing phase output) → rick_retry_workflow (restart)

**Wave development for an epic:**
rick_wave_plan (compute waves) → rick_wave_launch (start wave) → rick_wave_status (monitor) → rick_create_pr (for each completed workflow) → rick_wave_cleanup (remove workspaces)

**Ad-hoc code task:**
rick_workspace_setup (isolated clone) → rick_run (AI executes task) → rick_diff (review changes) → rick_create_pr (ship it)

**Deep workflow inspection:**
rick_workflow_status (overview) → rick_phase_timeline (timing) → rick_workflow_verdicts (review results) → rick_workflow_output (all phase outputs)

## Memory
You have a persistent memory system that survives across sessions. Saved memories appear at the start of each conversation as "[Operator Memory]" blocks. Slash commands (/remember, /memories, /forget) are handled client-side — they don't reach you.

- When you learn important context about the user (name, role, team, projects, preferences, environment setup, repos, tools), proactively suggest saving it: "Worth remembering — try ` + "`/remember [category] your info here`" + `"
- Categories: user (about the operator), preference (how they like things done), environment (their setup, repos, tools), workflow (project-specific patterns), general (anything else)
- When relevant memories exist, reference them naturally. Don't repeat them verbatim — use them to be more helpful and proactive.
- If a memory seems outdated, suggest the operator update it with /forget and /remember.`

// coreRulesReminder is re-injected every contextReinjectInterval turns
// to combat Gemini's instruction drift on long tool-calling chains.
const coreRulesReminder = `[System Reminder] Follow the <rules> and <stop_conditions> in your system instructions. Never fabricate data. After calling a tool and getting its result, STOP and respond to the operator — do not chain additional tools unless explicitly asked. Be concise.`

// contextReinjectInterval is the number of turns between core rule re-injections.
// Flash models drift faster, so this is kept conservative.
const contextReinjectInterval = 5

// maxToolRoundsPerTurn caps the number of model→tool→model round-trips within
// a single operator turn. The ADK runner has no built-in limit, so without this
// guard a model that keeps requesting tool calls will loop indefinitely.
const maxToolRoundsPerTurn = 25

// Operator wraps the ADK agent with MCP tool access to rick-server.
type Operator struct {
	cfg               Config
	client            *http.Client
	runner            *runner.Runner
	sessionService    session.Service
	sessionID         string
	sessionSeq        int
	turnCount         int
	toolRounds        int32 // atomic; tool-call rounds in current turn
	logger            *slog.Logger
	mu                sync.Mutex
	memoryStore       *MemoryStore
	lastMemoryVersion int64 // tracks when memories were last injected
}

// NewOperator creates an Operator backed by the ADK runner.
func NewOperator(cfg Config, ms *MemoryStore, logger *slog.Logger) *Operator {
	return &Operator{
		cfg:         cfg,
		client:      &http.Client{},
		sessionID:   "operator-session",
		logger:      logger,
		memoryStore: ms,
	}
}

// Init discovers MCP tools and wires the ADK agent + runner.
func (o *Operator) Init(ctx context.Context) error {
	// Create Gemini model via ADK.
	model, err := gemini.NewModel(ctx, o.cfg.Model, &genai.ClientConfig{
		APIKey:  o.cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return fmt.Errorf("operator: create model: %w", err)
	}

	// Create MCP toolset with Streamable HTTP transport to rick-server.
	transport := &mcp.StreamableClientTransport{
		Endpoint: o.cfg.ServerURL,
	}
	toolset, err := mcptoolset.New(mcptoolset.Config{
		Transport: transport,
	})
	if err != nil {
		return fmt.Errorf("operator: create mcp toolset: %w", err)
	}

	// Model tuning: low temperature for focused reasoning, thinking budget
	// for hidden chain-of-thought before tool calls.
	// Budget scales with model: Pro uses deeper reasoning, Flash is shallower.
	temperature := float32(0.15)
	thinkingBudget := int32(10240)
	if strings.Contains(o.cfg.Model, "flash") {
		thinkingBudget = 4096
	}

	// Create LLM agent with a safety valve that caps tool-call rounds per turn.
	ag, err := llmagent.New(llmagent.Config{
		Model:       model,
		Name:        "rick_operator",
		Description: "AI assistant for managing Rick workflows",
		Instruction: systemInstruction,
		Toolsets:    []tool.Toolset{toolset},
		GenerateContentConfig: &genai.GenerateContentConfig{
			Temperature: &temperature,
			ThinkingConfig: &genai.ThinkingConfig{
				ThinkingBudget: &thinkingBudget,
			},
		},
		AfterModelCallbacks: []llmagent.AfterModelCallback{
			func(_ agent.CallbackContext, resp *adkmodel.LLMResponse, respErr error) (*adkmodel.LLMResponse, error) {
				if respErr != nil || resp == nil || resp.Content == nil {
					return nil, nil
				}
				hasToolCall := false
				for _, p := range resp.Content.Parts {
					if p.FunctionCall != nil {
						hasToolCall = true
						break
					}
				}
				if !hasToolCall {
					return nil, nil
				}
				n := atomic.AddInt32(&o.toolRounds, 1)
				if n > maxToolRoundsPerTurn {
					o.logger.Warn("operator: tool-call loop capped",
						slog.Int("rounds", int(n)),
						slog.Int("max", maxToolRoundsPerTurn),
					)
					return &adkmodel.LLMResponse{
						Content: genai.NewContentFromText(
							"I hit my tool-call limit for this turn. Here's what I found so far — ask me to continue if you need more.",
							"model",
						),
					}, nil
				}
				return nil, nil
			},
		},
	})
	if err != nil {
		return fmt.Errorf("operator: create agent: %w", err)
	}

	// Create runner with in-memory session service.
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "rick-agent",
		Agent:          ag,
		SessionService: svc,
	})
	if err != nil {
		return fmt.Errorf("operator: create runner: %w", err)
	}

	// Pre-create the session so the runner can find it.
	sid := "operator-session-0"
	_, err = svc.Create(ctx, &session.CreateRequest{
		AppName:   "rick-agent",
		UserID:    "operator",
		SessionID: sid,
	})
	if err != nil {
		return fmt.Errorf("operator: create session: %w", err)
	}

	o.mu.Lock()
	o.runner = r
	o.sessionService = svc
	o.sessionID = sid
	o.sessionSeq = 0
	o.turnCount = 0
	o.mu.Unlock()

	o.logger.Info("operator: initialized",
		slog.String("model", o.cfg.Model),
		slog.Float64("temperature", float64(temperature)),
		slog.Int("thinking_budget", int(thinkingBudget)),
	)
	return nil
}

// Run sends a user message and returns the agent's text response. It handles
// tool call loops automatically via the ADK runner.
func (o *Operator) Run(ctx context.Context, userMessage string, emit func(Event)) (string, error) {
	o.mu.Lock()
	r := o.runner
	o.turnCount++
	turn := o.turnCount
	o.mu.Unlock()

	atomic.StoreInt32(&o.toolRounds, 0)

	if r == nil {
		return "", fmt.Errorf("operator not initialized — call Init first")
	}

	// Inject memories when they've changed since last injection.
	if o.memoryStore != nil {
		if ver := o.memoryStore.Version(); ver > o.lastMemoryVersion {
			if block := o.memoryStore.FormatForPrompt(); block != "" {
				userMessage = block + "\n---\n" + userMessage
			}
			o.lastMemoryVersion = ver
		}
	}

	// Re-inject core rules periodically to combat instruction drift
	// on long tool-calling chains.
	if turn > 1 && turn%contextReinjectInterval == 0 {
		userMessage = coreRulesReminder + "\n---\n" + userMessage
	}

	userContent := genai.NewContentFromText(userMessage, "user")

	var textParts []string
	for evt, err := range r.Run(ctx, "operator", o.sessionID, userContent, agent.RunConfig{}) {
		if err != nil {
			return "", fmt.Errorf("operator: runner event error: %w", err)
		}

		// Emit tool call/result events for UI from content parts.
		if evt.Content != nil {
			for _, p := range evt.Content.Parts {
				if p.FunctionCall != nil {
					emit(Event{Type: "tool_call", ToolName: p.FunctionCall.Name})
				}
				if p.FunctionResponse != nil {
					emit(Event{Type: "tool_result", ToolName: p.FunctionResponse.Name})
				}
			}
		}

		// Collect text from final response.
		if evt.IsFinalResponse() && evt.Content != nil {
			for _, p := range evt.Content.Parts {
				if p.Text != "" {
					textParts = append(textParts, p.Text)
				}
			}
		}
	}

	return strings.Join(textParts, ""), nil
}

// ResetContext creates a fresh session, clearing the operator's conversation history.
// The runner and MCP tools remain intact — only the context is wiped.
func (o *Operator) ResetContext(ctx context.Context) error {
	o.mu.Lock()
	svc := o.sessionService
	o.sessionSeq++
	sid := fmt.Sprintf("operator-session-%d", o.sessionSeq)
	o.mu.Unlock()

	if svc == nil {
		return fmt.Errorf("operator not initialized")
	}

	_, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "rick-agent",
		UserID:    "operator",
		SessionID: sid,
	})
	if err != nil {
		return fmt.Errorf("operator: reset context: %w", err)
	}

	o.mu.Lock()
	o.sessionID = sid
	o.turnCount = 0
	o.lastMemoryVersion = 0 // force re-injection on next message
	o.mu.Unlock()

	o.logger.Info("operator: context reset", slog.String("session", sid))
	return nil
}

// Connected checks whether the MCP server is reachable.
func (o *Operator) Connected(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.cfg.ServerURL, nil)
	if err != nil {
		return false
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
