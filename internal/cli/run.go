package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	"github.com/marconn/rick-event-driven-development/internal/persona"
	"github.com/marconn/rick-event-driven-development/internal/projection"
	"github.com/marconn/rick-event-driven-development/internal/source"
)

type runOpts struct {
	backendName string
	dagName     string
	dbPath      string
	sourcePath  string
	yolo        bool
	workDir     string
	repo        string
	ticket      string
}

func newRunCmd() *cobra.Command {
	opts := &runOpts{}

	cmd := &cobra.Command{
		Use:   "run [prompt]",
		Short: "Run a workflow",
		Long: `Execute a structured AI workflow from a prompt or source reference.

Examples:
  rick run "Build a REST API for user management"
  rick run --source file:requirements.md
  rick run --source jira:PROJ-123
  rick run --backend gemini "Implement feature X"
  rick run --dag develop-only "Fix the bug in handler.go"
  rick run --dag workspace-dev --source gh:acme/myapp#368`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflow(cmd.Context(), opts, args)
		},
	}

	cmd.Flags().StringVarP(&opts.backendName, "backend", "b", "claude", "AI backend (claude, gemini)")
	cmd.Flags().StringVarP(&opts.dagName, "dag", "d", "workspace-dev", "Workflow definition (workspace-dev, develop-only, pr-review, jira-dev)")
	cmd.Flags().StringVar(&opts.dbPath, "db", "rick.db", "SQLite database path")
	cmd.Flags().StringVarP(&opts.sourcePath, "source", "s", "", "Source reference (file:path, jira:KEY-123, gh:owner/repo#1)")
	cmd.Flags().BoolVar(&opts.yolo, "yolo", false, "Skip AI backend permission checks")
	cmd.Flags().StringVarP(&opts.workDir, "workdir", "w", ".", "Working directory for AI backends")
	cmd.Flags().StringVar(&opts.repo, "repo", "", "Repository name (auto-derived from gh: source if omitted)")
	cmd.Flags().StringVar(&opts.ticket, "ticket", "", "Ticket/branch name (auto-derived from gh: source if omitted)")

	return cmd
}

func runWorkflow(ctx context.Context, opts *runOpts, args []string) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	// Resolve the prompt from args or source flag
	prompt, sourceRef, err := resolvePrompt(ctx, opts, args)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Create store
	store, err := eventstore.NewSQLiteStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Create bus
	bus := eventbus.NewChannelBus(eventbus.WithLogger(logger))

	// Create backend
	be, err := backend.New(opts.backendName)
	if err != nil {
		return err
	}

	// Create persona system
	personas := persona.DefaultRegistry()
	builder := persona.NewPromptBuilder()

	// Create and register handlers
	reg := handler.NewRegistry()
	deps := handler.Deps{
		Backend:    be,
		Store:      store,
		Personas:   personas,
		Builder:    builder,
		Jira:       jira.NewClientFromEnv(),
		Confluence: confluence.NewClientFromEnv(),
		Estimation: openEstimationStore(logger),
		MsMap:      loadMicroserviceMap(logger),
		Logger:     logger,
		WorkDir:    opts.workDir,
		Yolo:       opts.yolo,
	}
	if err := handler.RegisterAll(reg, deps); err != nil {
		return fmt.Errorf("register handlers: %w", err)
	}

	// Create engine
	eng := engine.NewEngine(store, bus, logger)

	// Register workflow definition
	workflowDef, err := selectWorkflowDef(opts.dagName)
	if err != nil {
		return err
	}
	eng.RegisterWorkflow(workflowDef)

	// Create persona runner — sole dispatcher for all handlers
	dispatcher := engine.NewLocalDispatcher(reg)
	personaRunner := engine.NewPersonaRunner(store, bus, dispatcher, logger)
	personaRunner.RegisterWorkflow(workflowDef)

	// Start engine (lifecycle only)
	eng.Start()
	defer eng.Stop()

	// Start persona dispatch
	personaRunner.Start(ctx, reg)
	defer func() { _ = personaRunner.Close() }()

	// Start projections for token tracking, timeline visibility, and verdicts
	projRunner := projection.NewRunner(store, bus, logger)
	projRunner.Register(projection.NewTokenUsageProjection())
	projRunner.Register(projection.NewPhaseTimelineProjection())
	projRunner.Register(projection.NewVerdictProjection())
	if err := projRunner.Start(ctx); err != nil {
		return fmt.Errorf("start projections: %w", err)
	}
	defer projRunner.Stop()

	// Generate workflow ID — correlationID == aggregateID by convention so
	// the Engine can resolve the workflow aggregate from any event in the chain.
	aggregateID := uuid.New().String()
	correlationID := aggregateID

	// Subscribe for completion/failure before publishing
	result := make(chan workflowResult, 1)
	unsub := subscribeForResult(bus, aggregateID, result, logger)
	defer unsub()

	// Auto-derive repo/ticket from gh: source when not explicitly set
	if strings.HasPrefix(opts.sourcePath, "gh:") {
		ref := strings.TrimPrefix(opts.sourcePath, "gh:")
		if parts := strings.SplitN(ref, "#", 2); len(parts) == 2 {
			if opts.repo == "" {
				// Extract repo name: "acme/myapp" → "clinic"
				repoParts := strings.Split(parts[0], "/")
				opts.repo = repoParts[len(repoParts)-1]
			}
			if opts.ticket == "" {
				opts.ticket = fmt.Sprintf("issue-%s", parts[1])
			}
		}
	}

	// Publish WorkflowRequested
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     prompt,
		WorkflowID: opts.dagName,
		Source:     sourceRef,
		Repo:       opts.repo,
		Ticket:     opts.ticket,
	})).
		WithAggregate(aggregateID, 1).
		WithCorrelation(correlationID).
		WithSource("cli:run")

	if err := store.Append(ctx, aggregateID, 0, []event.Envelope{reqEvt}); err != nil {
		return fmt.Errorf("store workflow requested: %w", err)
	}
	if err := bus.Publish(ctx, reqEvt); err != nil {
		return fmt.Errorf("publish workflow requested: %w", err)
	}

	fmt.Fprintf(os.Stderr, "workflow %s started (dag=%s, backend=%s)\n", aggregateID[:8], opts.dagName, opts.backendName)

	// Wait for result
	select {
	case r := <-result:
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "workflow failed: %s (phase: %s)\n", r.reason, r.phase)
			return fmt.Errorf("workflow failed: %s", r.reason)
		}
		fmt.Fprintf(os.Stderr, "workflow completed: %s\n", r.reason)
		return nil

	case <-ctx.Done():
		return fmt.Errorf("workflow interrupted")
	}
}

func resolvePrompt(ctx context.Context, opts *runOpts, args []string) (prompt, sourceRef string, err error) {
	resolver := source.NewResolver()

	if opts.sourcePath != "" {
		src, err := resolver.Resolve(ctx, opts.sourcePath)
		if err != nil {
			return "", "", fmt.Errorf("resolve source: %w", err)
		}
		return src.Content, opts.sourcePath, nil
	}

	if len(args) > 0 {
		return args[0], "raw", nil
	}

	return "", "", fmt.Errorf("provide a prompt argument or --source flag")
}

func selectWorkflowDef(name string) (engine.WorkflowDef, error) {
	var def engine.WorkflowDef
	switch name {
	case "develop-only":
		def = engine.DevelopOnlyWorkflowDef()
	case "workspace-dev":
		def = engine.WorkspaceDevWorkflowDef()
	case "pr-review":
		def = engine.PRReviewWorkflowDef()
	case "pr-feedback":
		def = engine.PRFeedbackWorkflowDef()
	case "jira-dev":
		def = engine.JiraDevWorkflowDef()
	case "ci-fix":
		def = engine.CIFixWorkflowDef()
	case "plan-btu":
		def = engine.PlanBTUWorkflowDef()
	case "jira-qa-steps":
		def = engine.JiraQAStepsWorkflowDef()
	case "plan-jira":
		def = engine.PlanJiraWorkflowDef()
	case "task-creator":
		def = engine.TaskCreatorWorkflowDef()
	default:
		return engine.WorkflowDef{}, fmt.Errorf("unknown workflow: %s (valid: develop-only, workspace-dev, pr-review, pr-feedback, jira-dev, ci-fix, plan-btu, plan-jira, task-creator, jira-qa-steps)", name)
	}

	if os.Getenv("RICK_DISABLE_QUALITY_GATE") != "" {
		def = engine.WithoutHandler(def, "quality-gate")
	}

	return def, nil
}

type workflowResult struct {
	reason string
	phase  string
	err    error
}

func subscribeForResult(bus eventbus.Bus, aggregateID string, ch chan<- workflowResult, logger *slog.Logger) func() {
	var once sync.Once

	send := func(r workflowResult) {
		once.Do(func() { ch <- r })
	}

	unsub1 := bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, env event.Envelope) error {
		if env.AggregateID != aggregateID {
			return nil
		}
		var p event.WorkflowCompletedPayload
		if err := unmarshalPayload(env.Payload, &p); err != nil {
			logger.Error("unmarshal workflow completed", slog.String("error", err.Error()))
			return nil
		}
		send(workflowResult{reason: p.Result})
		return nil
	}, eventbus.WithName("cli:completion"))

	unsub2 := bus.Subscribe(event.WorkflowFailed, func(_ context.Context, env event.Envelope) error {
		if env.AggregateID != aggregateID {
			return nil
		}
		var p event.WorkflowFailedPayload
		if err := unmarshalPayload(env.Payload, &p); err != nil {
			logger.Error("unmarshal workflow failed", slog.String("error", err.Error()))
			return nil
		}
		send(workflowResult{reason: p.Reason, phase: p.Phase, err: fmt.Errorf("workflow failed: %s", p.Reason)})
		return nil
	}, eventbus.WithName("cli:failure"))

	// Log persona transitions for visibility
	unsub3 := bus.Subscribe(event.PersonaCompleted, func(_ context.Context, env event.Envelope) error {
		if env.CorrelationID != aggregateID {
			return nil
		}
		var p event.PersonaCompletedPayload
		if err := unmarshalPayload(env.Payload, &p); err == nil {
			fmt.Fprintf(os.Stderr, "  ✓ %s completed (chain=%d, %dms)\n", p.Persona, p.ChainDepth, p.DurationMS)
		}
		return nil
	}, eventbus.WithName("cli:persona-log"))

	unsub4 := bus.Subscribe(event.VerdictRendered, func(_ context.Context, env event.Envelope) error {
		if env.CorrelationID != aggregateID {
			return nil
		}
		var p event.VerdictPayload
		if err := unmarshalPayload(env.Payload, &p); err == nil {
			fmt.Fprintf(os.Stderr, "  ← verdict: %s for %s", p.Outcome, p.Phase)
			if p.SourcePhase != "" {
				fmt.Fprintf(os.Stderr, " (from %s)", p.SourcePhase)
			}
			fmt.Fprintln(os.Stderr)
		}
		return nil
	}, eventbus.WithName("cli:verdict-log"))

	return func() {
		unsub1()
		unsub2()
		unsub3()
		unsub4()
	}
}

// unmarshalPayload is a convenience wrapper.
func unmarshalPayload(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
