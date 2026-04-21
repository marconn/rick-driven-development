package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
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
	return parseTimeoutEnv(logger, "RICK_BACKEND_TIMEOUT", handler.DefaultBackendTimeout)
}

// parseReviewBackendTimeout reads RICK_REVIEW_BACKEND_TIMEOUT from the
// environment. Falls back to handler.DefaultReviewBackendTimeout when unset
// or unparseable. Applies to reviewer/qa/feedback/committer/pr-category
// handlers — the developer phase uses the separate RICK_BACKEND_TIMEOUT.
func parseReviewBackendTimeout(logger *slog.Logger) time.Duration {
	return parseTimeoutEnv(logger, "RICK_REVIEW_BACKEND_TIMEOUT", handler.DefaultReviewBackendTimeout)
}

func parseTimeoutEnv(logger *slog.Logger, name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("timeout env var unparseable, using default",
			slog.String("var", name),
			slog.String("value", v),
			slog.Duration("default", fallback),
			slog.Any("error", err),
		)
		return fallback
	}
	return d
}

// newReviewBackend builds the backend used by all review-phase handlers.
// Reads RICK_REVIEW_BACKENDS (comma-separated list in rotation order) and
// falls back to backend.DefaultReviewBackends when unset.
//
// The passed recorder (may be nil) is attached to each inner backend's
// concurrency limiter so observability hits both the primary developer
// backend and the review rotation through the same collector.
//
// Parse/validation errors fall back to gemini (prior default) rather than
// failing startup — review handlers are critical and we'd rather run with
// a single backend than refuse to boot. The fallback is logged loudly.
func newReviewBackend(logger *slog.Logger, recorder backend.Recorder) backend.Backend {
	raw := os.Getenv("RICK_REVIEW_BACKENDS")
	names := backend.ParseReviewBackendsEnv(raw)
	be, err := backend.NewReviewBackendWithRecorder(names, recorder)
	if err == nil {
		logger.Info("review backend selected", slog.String("name", be.Name()))
		return be
	}
	logger.Warn("RICK_REVIEW_BACKENDS invalid, falling back to gemini",
		slog.String("value", raw),
		slog.Any("error", err),
	)
	fallback, _ := backend.NewWithRecorder("gemini", recorder)
	return fallback
}

// newDeveloperBackend returns the backend used by the developer phase (and
// any other single-backend AI handler). When RICK_DEVELOPER_BACKENDS is set
// and yields ≥2 valid names, it builds a RoundRobin that — combined with
// AIHandler's retry-aware sticky key — lands on a different inner CLI each
// time the engine auto-retries after an idle_timeout. When the env var is
// unset, empty, or yields a single name, the primary backend (from
// --backend) is returned unchanged so default single-backend deployments
// see no behavior change.
//
// Parse errors log and fall back to the primary: rotation is a bug-fix
// upgrade, not a hard dependency. Operators already running on one CLI
// should never fail to boot because of a typo in the rotation list.
func newDeveloperBackend(primary backend.Backend, logger *slog.Logger, recorder backend.Recorder) backend.Backend {
	raw := os.Getenv("RICK_DEVELOPER_BACKENDS")
	names := backend.ParseReviewBackendsEnv(raw) // reused — same comma-split semantics
	if len(names) == 0 {
		return primary
	}
	be, err := backend.NewReviewBackendWithRecorder(names, recorder)
	if err != nil {
		logger.Warn("RICK_DEVELOPER_BACKENDS invalid, falling back to primary backend",
			slog.String("value", raw),
			slog.String("primary", primary.Name()),
			slog.Any("error", err),
		)
		return primary
	}
	logger.Info("developer backend rotation enabled",
		slog.String("name", be.Name()),
		slog.String("primary", primary.Name()),
	)
	return be
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
