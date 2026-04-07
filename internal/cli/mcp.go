package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	"github.com/marconn/rick-event-driven-development/internal/mcp"
	"github.com/marconn/rick-event-driven-development/internal/persona"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

type mcpOpts struct {
	dbPath      string
	backendName string
	yolo        bool
	workDir     string
}

func newMCPCmd() *cobra.Command {
	opts := &mcpOpts{}

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start MCP server (JSON-RPC over stdio)",
		Long: `Start a Model Context Protocol server that exposes Rick's
event-driven orchestration as MCP tools. Communicates via
newline-delimited JSON-RPC 2.0 on stdin/stdout.

Use this with Claude Desktop, Cursor, or any MCP-compatible client.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.dbPath, "db", "rick.db", "SQLite database path")
	cmd.Flags().StringVarP(&opts.backendName, "backend", "b", "claude", "AI backend (claude, gemini)")
	cmd.Flags().BoolVar(&opts.yolo, "yolo", false, "Skip AI backend permission checks")
	cmd.Flags().StringVarP(&opts.workDir, "workdir", "w", ".", "Working directory for AI backends")

	return cmd
}

func runMCP(ctx context.Context, opts *mcpOpts) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	// Log to stderr — stdout is reserved for MCP protocol
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	store, err := eventstore.NewSQLiteStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	bus := eventbus.NewChannelBus(eventbus.WithLogger(logger))
	defer func() { _ = bus.Close() }()

	be, err := backend.New(opts.backendName)
	if err != nil {
		return err
	}

	personas := persona.DefaultRegistry()
	builder := persona.NewPromptBuilder()

	reg := handler.NewRegistry()
	deps := handler.Deps{
		Backend:        be,
		Store:          store,
		Bus:            bus,
		Personas:       personas,
		Builder:        builder,
		Jira:           jira.NewClientFromEnv(),
		Confluence:     confluence.NewClientFromEnv(),
		Estimation:     openEstimationStore(logger),
		MsMap:          loadMicroserviceMap(logger),
		Logger:         logger,
		WorkDir:        opts.workDir,
		Yolo:           opts.yolo,
		BackendTimeout: parseBackendTimeout(logger),
	}
	if err := handler.RegisterAll(reg, deps); err != nil {
		return fmt.Errorf("register handlers: %w", err)
	}

	eng := engine.NewEngine(store, bus, logger)

	// Register all workflow definitions
	for _, name := range []string{"develop-only", "workspace-dev", "pr-review", "pr-feedback"} {
		if def, err := selectWorkflowDef(name); err == nil {
			eng.RegisterWorkflow(def)
		}
	}

	// Create persona runner — sole dispatcher for all handlers
	dispatcher := engine.NewLocalDispatcher(reg)
	personaRunner := engine.NewPersonaRunner(store, bus, dispatcher, logger)
	// Register workflow defs with PersonaRunner for DAG-based dispatch.
	for _, name := range []string{"develop-only", "workspace-dev", "pr-review", "pr-feedback"} {
		if def, defErr := selectWorkflowDef(name); defErr == nil {
			personaRunner.RegisterWorkflow(def)
		}
	}

	eng.Start()
	defer eng.Stop()

	personaRunner.Start(ctx, reg)
	defer func() { _ = personaRunner.Close() }()

	// Set up projections
	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()
	verdicts := projection.NewVerdictProjection()

	runner := projection.NewRunner(store, bus, logger)
	runner.Register(workflows)
	runner.Register(tokens)
	runner.Register(timelines)
	runner.Register(verdicts)
	if err := runner.Start(ctx); err != nil {
		return fmt.Errorf("start projections: %w", err)
	}
	defer runner.Stop()

	mcpDeps := mcp.Deps{
		Store:          store,
		Bus:            bus,
		Engine:         eng,
		Workflows:      workflows,
		Tokens:         tokens,
		Timelines:      timelines,
		Verdicts:       verdicts,
		SelectWorkflow: selectWorkflowDef,
		BackendName:    opts.backendName,
		WorkDir:        opts.workDir,
		Yolo:           opts.yolo,
		Backend:        be,
		Jira:           deps.Jira,
		Confluence:     deps.Confluence,
	}

	server := mcp.NewServer(mcpDeps, logger)
	defer server.Close()
	return server.Serve(ctx, os.Stdin, os.Stdout)
}
