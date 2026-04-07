package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/estimation"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	gh "github.com/marconn/rick-event-driven-development/internal/github"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	"github.com/marconn/rick-event-driven-development/internal/jirapoller"
	"github.com/marconn/rick-event-driven-development/internal/planning"
	"github.com/marconn/rick-event-driven-development/internal/pluginstore"
)

// parseBackendTimeout reads RICK_BACKEND_TIMEOUT from the environment.
// Falls back to handler.DefaultBackendTimeout when unset or unparseable.
// Setting it to "0" disables the timeout entirely (legacy behavior — only
// useful for debugging long-running AI runs).
func parseBackendTimeout(logger *slog.Logger) time.Duration {
	v := os.Getenv("RICK_BACKEND_TIMEOUT")
	if v == "" {
		return handler.DefaultBackendTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("RICK_BACKEND_TIMEOUT unparseable, using default",
			slog.String("value", v),
			slog.Duration("default", handler.DefaultBackendTimeout),
			slog.Any("error", err),
		)
		return handler.DefaultBackendTimeout
	}
	return d
}

// openEstimationStore opens the estimation SQLite DB.
// Returns nil (non-fatal) when the DB path is unavailable.
func openEstimationStore(logger *slog.Logger) *estimation.Store {
	dbPath := os.Getenv("ESTIMATION_DB")
	if dbPath == "" {
		dataDir := os.Getenv("XDG_DATA_HOME")
		if dataDir == "" {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".local", "share")
		}
		dbPath = filepath.Join(dataDir, "rick", "planning-estimates.db")
	}

	store, err := estimation.NewStore(dbPath)
	if err != nil {
		logger.Warn("estimation store unavailable", slog.Any("error", err))
		return nil
	}
	return store
}

// loadMicroserviceMap loads the microservice mapping from RICK_REPOS_PATH.
// Returns a minimal map (non-fatal) when RICK_REPOS_PATH is unset.
func loadMicroserviceMap(logger *slog.Logger) *planning.MicroserviceMap {
	reposPath := os.Getenv("RICK_REPOS_PATH")
	if reposPath == "" {
		return nil
	}

	msMap := planning.NewMicroserviceMap(reposPath)

	// Try AGENTS.md or CLAUDE.md in RICK_REPOS_PATH for platform context.
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := filepath.Join(reposPath, name)
		if err := msMap.LoadFromFile(path); err == nil {
			logger.Info("loaded microservice map", slog.String("source", path))
			return msMap
		}
	}

	// Try explicit MICROSERVICES_FILE env var.
	if msFile := os.Getenv("MICROSERVICES_FILE"); msFile != "" {
		if err := msMap.LoadFromFile(msFile); err == nil {
			logger.Info("loaded microservice map", slog.String("source", msFile))
			return msMap
		}
	}

	logger.Info("no microservice map loaded, using RICK_REPOS_PATH auto-discovery")
	return msMap
}

// newGitHubClient creates a GitHub API client from GITHUB_TOKEN.
// Returns nil when the token is unset.
func newGitHubClient() *gh.Client {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil
	}
	if base := os.Getenv("GITHUB_API_URL"); base != "" {
		return gh.NewClientWithBase(base, token)
	}
	return gh.NewClient(token)
}

// openPluginStore opens the shared plugin SQLite DB.
// Returns nil (non-fatal) when unavailable.
func openPluginStore(logger *slog.Logger) *pluginstore.Store {
	dbPath := os.Getenv("PLUGIN_DB")
	if dbPath == "" {
		dataDir := os.Getenv("XDG_DATA_HOME")
		if dataDir == "" {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".local", "share")
		}
		dbPath = filepath.Join(dataDir, "rick", "plugins.db")
	}

	store, err := pluginstore.New(dbPath)
	if err != nil {
		logger.Warn("plugin store unavailable", slog.Any("error", err))
		return nil
	}
	return store
}

// startOptionalServices starts background services if their env vars are configured.
func startOptionalServices(ctx context.Context, bus eventbus.Bus, jiraClient *jira.Client, ghClient *gh.Client, pstore *pluginstore.Store, logger *slog.Logger) {
	// GitHub reporter: posts PR comments on workflow completion.
	if ghClient != nil && pstore != nil {
		reporter := gh.NewReporter(ghClient, pstore, logger)

		// CI poller: polls GitHub Actions after successful workflow completions.
		if os.Getenv("CI_POLL_ENABLED") == "true" {
			ciPoller := gh.NewCIPoller(ghClient, bus, pstore, gh.CIPollerConfig{}, logger)
			reporter.WithCIPoller(ciPoller)
			logger.Info("ci poller enabled")
		}

		unsub := reporter.Start(bus)
		go func() {
			<-ctx.Done()
			unsub()
		}()
		logger.Info("github reporter started")
	}

	// Jira poller: polls Jira for new tickets and injects workflows.
	if jql := os.Getenv("JIRA_JQL"); jql != "" && jiraClient != nil && pstore != nil {
		interval := 60 * time.Second
		if v := os.Getenv("JIRA_POLL_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				interval = d
			}
		}
		workflowID := os.Getenv("JIRA_POLL_WORKFLOW")
		if workflowID == "" {
			workflowID = "jira-dev"
		}
		poller := jirapoller.NewPoller(jiraClient, pstore, bus, jirapoller.Config{
			JQL:          jql,
			PollInterval: interval,
			WorkflowID:   workflowID,
			Logger:       logger,
		})
		go func() {
			if err := poller.Run(ctx); err != nil {
				logger.Error("jira poller exited", slog.Any("error", err))
			}
		}()
		logger.Info("jira poller started", slog.String("jql", jql), slog.Duration("interval", interval))
	}
}
