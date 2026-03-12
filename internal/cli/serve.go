package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/grpchandler"
	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/mcp"
	"github.com/marconn/rick-event-driven-development/internal/persona"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

type serveOpts struct {
	dbPath      string
	addr        string
	grpcAddr    string
	backendName string
	workDir     string
	yolo        bool
}

func newServeCmd() *cobra.Command {
	opts := &serveOpts{}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start Rick server with MCP over HTTP",
		Long: `Start the Rick engine as a long-running daemon that exposes MCP tools
over HTTP. Suitable for systemd deployment.

The HTTP endpoint accepts JSON-RPC 2.0 POST requests at /mcp and returns
JSON-RPC responses. GET /mcp returns server info and available tools.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.dbPath, "db", "rick.db", "SQLite database path")
	cmd.Flags().StringVar(&opts.addr, "addr", ":8077", "HTTP listen address")
	cmd.Flags().StringVar(&opts.grpcAddr, "grpc-addr", ":9077", "gRPC listen address for external handlers")
	cmd.Flags().StringVarP(&opts.backendName, "backend", "b", "claude", "AI backend (claude, gemini)")
	cmd.Flags().StringVarP(&opts.workDir, "workdir", "w", ".", "Working directory for AI backends")
	cmd.Flags().BoolVar(&opts.yolo, "yolo", true, "Skip AI backend permission checks (default true in serve mode)")

	return cmd
}

func runServe(ctx context.Context, opts *serveOpts) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	ghClient := newGitHubClient()
	pstore := openPluginStore(logger)

	reg := handler.NewRegistry()
	deps := handler.Deps{
		Backend:     be,
		Store:       store,
		Personas:    personas,
		Builder:     builder,
		Jira:        jira.NewClientFromEnv(),
		Confluence:  confluence.NewClientFromEnv(),
		Estimation:  openEstimationStore(logger),
		MsMap:       loadMicroserviceMap(logger),
		GitHub:      ghClient,
		PluginStore: pstore,
		Logger:      logger,
		WorkDir:     opts.workDir,
		Yolo:        opts.yolo,
	}
	if err := handler.RegisterAll(reg, deps); err != nil {
		return fmt.Errorf("register handlers: %w", err)
	}

	// Dispatchers: local (in-process) + stream (gRPC external handlers).
	streamD := grpchandler.NewStreamDispatcher(logger)
	localD := engine.NewLocalDispatcher(reg)
	compositeD := grpchandler.NewCompositeDispatcher(localD, streamD)

	personaRunner := engine.NewPersonaRunner(store, bus, compositeD, logger)

	eng := engine.NewEngine(store, bus, logger)
	// Auto-scale chain depth whenever a workflow is registered (startup or runtime gRPC).
	eng.OnWorkflowRegistered(func(def engine.WorkflowDef) {
		personaRunner.AdjustChainDepth(len(def.Required))
	})

	// Register workflow defs with both Engine (lifecycle) and PersonaRunner (DAG dispatch).
	for _, name := range []string{"develop-only", "workspace-dev", "pr-review", "pr-feedback", "jira-dev", "ci-fix", "plan-btu", "plan-jira", "task-creator", "jira-qa-steps"} {
		if def, defErr := selectWorkflowDef(name); defErr == nil {
			eng.RegisterWorkflow(def)
			personaRunner.RegisterWorkflow(def)
		}
	}

	eng.Start()
	defer eng.Stop()

	// Projections.
	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()
	verdicts := projection.NewVerdictProjection()

	projRunner := projection.NewRunner(store, bus, logger)
	projRunner.Register(workflows)
	projRunner.Register(tokens)
	projRunner.Register(timelines)
	projRunner.Register(verdicts)
	if err := projRunner.Start(ctx); err != nil {
		return fmt.Errorf("start projections: %w", err)
	}
	defer projRunner.Stop()

	// Notification broker + gRPC server.
	broker := grpchandler.NewNotificationBroker(bus, workflows, tokens, timelines, verdicts, logger)
	broker.Start()
	defer broker.Stop()

	injector := grpchandler.NewEventInjector(store, bus, logger)
	grpcSrv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterPersonaServiceServer(grpcSrv, grpchandler.NewServer(streamD, personaRunner, injector, broker, eng, reg, logger))

	grpcLis, err := net.Listen("tcp", opts.grpcAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	go func() {
		logger.Info("gRPC server listening", slog.String("addr", grpcLis.Addr().String()))
		if serveErr := grpcSrv.Serve(grpcLis); serveErr != nil {
			logger.Error("gRPC server error", slog.Any("error", serveErr))
		}
	}()
	defer grpcSrv.GracefulStop()

	personaRunner.Start(ctx, reg)
	defer func() { _ = personaRunner.Close() }()

	// Optional services — start if configured.
	startOptionalServices(ctx, bus, deps.Jira, ghClient, pstore, logger)

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
	return server.ServeHTTP(ctx, opts.addr)
}
