package handler

import (
	"log/slog"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/estimation"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	gh "github.com/marconn/rick-event-driven-development/internal/github"
	"github.com/marconn/rick-event-driven-development/internal/jira"
	"github.com/marconn/rick-event-driven-development/internal/jiraplanner"
	"github.com/marconn/rick-event-driven-development/internal/persona"
	"github.com/marconn/rick-event-driven-development/internal/planning"
	"github.com/marconn/rick-event-driven-development/internal/pluginstore"
)

// Deps bundles the shared dependencies needed by all handlers.
type Deps struct {
	Backend    backend.Backend
	Store      eventstore.Store
	// Bus is the in-process event bus. AI handlers use it to publish
	// AIRequestSent before backend.Run so a hung subprocess still leaves a
	// forensic trail (incident 2d8b4b99). May be nil in tests / deprecated
	// CLI run mode — handlers will fall back to bundling AIRequestSent with
	// the response.
	Bus        eventbus.Bus
	Personas   *persona.Registry
	Builder    *persona.PromptBuilder
	Jira        *jira.Client              // nil when JIRA env vars are unset (non-fatal)
	Confluence  *confluence.Client        // nil when CONFLUENCE env vars are unset (non-fatal)
	Estimation  *estimation.Store         // nil when estimation DB is unavailable (non-fatal)
	MsMap       *planning.MicroserviceMap // nil when RICK_REPOS_PATH is unset (non-fatal)
	GitHub      *gh.Client               // nil when GITHUB_TOKEN is unset (non-fatal)
	PluginStore *pluginstore.Store        // nil when plugin DB is unavailable (non-fatal)
	Logger      *slog.Logger
	WorkDir    string // working directory for AI backend execution
	Yolo       bool   // skip AI backend permission checks
	// ReviewBackend is the backend used for review-related handlers (e.g.,
	// reviewer, qa, pr-consolidator). When nil, defaults to a Gemini backend.
	ReviewBackend backend.Backend
	// BackendTimeout caps how long AIHandler.backend.Run may block.
	// Zero falls back to handler.DefaultBackendTimeout. Set explicitly via
	// RICK_BACKEND_TIMEOUT in serve mode.
	BackendTimeout time.Duration
}

// DefaultBackendTimeout is the fallback hard cap on AI backend calls when
// Deps.BackendTimeout is zero. Picked to be longer than any reasonable AI
// run we've observed in practice (~5 min for the heaviest reviewer pass)
// while still being short enough to surface a wedged subprocess in operator
// time. Override via RICK_BACKEND_TIMEOUT or by setting Deps.BackendTimeout.
const DefaultBackendTimeout = 20 * time.Minute

// RegisterAll creates and registers all unique handlers. Each handler is
// registered once — workflow DAGs control which handlers participate in
// which workflows.
func RegisterAll(reg *Registry, d Deps) error {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Resolve the backend timeout once: explicit Deps value wins, otherwise
	// fall back to the package default. Zero stays zero only when callers
	// have already opted out (e.g., tests using mock backends).
	backendTimeout := d.BackendTimeout
	if backendTimeout == 0 && d.Bus != nil {
		backendTimeout = DefaultBackendTimeout
	}

	// aiCfg builds an AIHandlerConfig with the shared deps wired in once,
	// so each handler registration only specifies what's actually different.
	aiCfg := func(name, phase, personaName string) AIHandlerConfig {
		return AIHandlerConfig{
			Name:           name,
			Phase:          phase,
			Persona:        personaName,
			Backend:        d.Backend,
			Store:          d.Store,
			Bus:            d.Bus,
			Personas:       d.Personas,
			Builder:        d.Builder,
			WorkDir:        d.WorkDir,
			Yolo:           d.Yolo,
			BackendTimeout: backendTimeout,
		}
	}

	// resolveReviewBackend returns the backend for review handlers:
	// 1. d.ReviewBackend if provided
	// 2. d.Backend if it's already gemini
	// 3. A new gemini backend
	resolveReviewBackend := func() backend.Backend {
		if d.ReviewBackend != nil {
			return d.ReviewBackend
		}
		if d.Backend.Name() == "gemini" {
			return d.Backend
		}
		g, err := backend.New("gemini")
		if err == nil {
			return g
		}
		return d.Backend
	}
	reviewBackend := resolveReviewBackend()

	// reviewAiCfg ensures the gemini backend is used for review-related handlers.
	reviewAiCfg := func(name, phase, personaName string) AIHandlerConfig {
		cfg := aiCfg(name, phase, personaName)
		cfg.Backend = reviewBackend
		return cfg
	}

	handlers := []Handler{
		// Core AI handlers — used across default, workspace-dev, jira-dev, pr-review,
		// pr-feedback, ci-fix workflows via DAG scoping.
		NewAIHandler(aiCfg("researcher", "research", persona.Researcher)),
		NewAIHandler(aiCfg("architect", "architect", persona.Architect)),
		NewDeveloperHandler(aiCfg("developer", "develop", persona.Developer)),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("reviewer", "review", persona.Reviewer),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("qa", "qa", persona.QA),
			TargetPhase: "develop",
		}),
		NewCommitterHandler(aiCfg("committer", "commit", persona.Committer)),

		// Feedback-specific AI handler.
		NewAIHandler(reviewAiCfg("feedback-analyzer", "feedback-analyze", persona.FeedbackAnalyzer)),

		// PR reply / summary composers — text-only (PlainText, Yolo=false) so
		// the LLM cannot run `gh pr comment` itself. The matching poster
		// handlers below are responsible for the actual GitHub side-effect.
		func() Handler {
			cfg := reviewAiCfg("pr-replier", "pr-reply", persona.PRReplier)
			cfg.PlainText = true
			cfg.Yolo = false
			return NewAIHandler(cfg)
		}(),
		func() Handler {
			cfg := reviewAiCfg("pr-summarizer", "pr-summary", persona.PRSummarizer)
			cfg.PlainText = true
			cfg.Yolo = false
			return NewAIHandler(cfg)
		}(),

		// Non-AI handlers.
		NewWorkspace(d),
		NewContextSnapshot(d),
		NewQualityGate(d),

		// PR-specific handlers.
		NewPRWorkspace(d),
		NewPRJiraContext(d),
		NewPRConsolidator(Deps{
			Backend:    reviewBackend,
			Store:      d.Store,
			Bus:        d.Bus,
			Personas:   d.Personas,
			Builder:    d.Builder,
			Jira:       d.Jira,
			Confluence: d.Confluence,
			Estimation: d.Estimation,
			MsMap:      d.MsMap,
			GitHub:     d.GitHub,
			Logger:     d.Logger,
			WorkDir:    d.WorkDir,
			Yolo:       d.Yolo,
		}),
		NewPRCleanup(d),

		// PR category reviewers — dedicated single-concern reviewers for pr-review workflow.
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-security", "pr-category-review", persona.PRSecurity),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-concurrency", "pr-category-review", persona.PRConcurrency),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-error-handling", "pr-category-review", persona.PRErrorHandling),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-observability", "pr-category-review", persona.PRObservability),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-api-contract", "pr-category-review", persona.PRAPIContract),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-idempotency", "pr-category-review", persona.PRIdempotency),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-testing", "pr-category-review", persona.PRTesting),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-integration", "pr-category-review", persona.PRIntegration),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-performance", "pr-category-review", persona.PRPerformance),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-data", "pr-category-review", persona.PRData),
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-hygiene", "pr-category-review", persona.PRHygiene),
			TargetPhase: "develop",
		}),

		// Jira context handler (jira-dev workflow).
		NewJiraContext(d),

		// GitHub issue context handler (github-dev workflow).
		NewGithubContext(d),

		// QA-steps-specific handlers.
		NewQAContext(d),
		func() Handler {
			cfg := reviewAiCfg("qa-analyzer", "qa-analyze", persona.QAAnalyzer)
			cfg.PlainText = true
			return NewAIHandler(cfg)
		}(),
		NewQAJiraWriter(d),
	}

	// GitHub PR fetcher — always registered so the pr-feedback DAG can reference
	// it unconditionally. When d.GitHub is nil (GITHUB_TOKEN unset) the handler
	// short-circuits inside Handle() with an empty enrichment instead of
	// silently being absent from the registry.
	handlers = append(handlers,
		gh.NewFetcherHandler(d.GitHub, d.Store, d.PluginStore, logger),
	)

	// PR comment posters — always registered so the pr-feedback DAG can
	// reference them unconditionally. When d.GitHub is nil the poster records
	// a PRCommentPosted{skipped=true} event instead of calling GitHub, so the
	// DAG still advances in token-less environments.
	var posterClient prCommentClient
	if d.GitHub != nil {
		posterClient = PRCommentClientAdapter{Client: d.GitHub}
	}
	handlers = append(handlers,
		NewPRCommentPoster(PRCommentPosterConfig{
			Name:     "pr-reply-poster",
			Upstream: "pr-replier",
			Kind:     "reply",
			GitHub:   posterClient,
			Store:    d.Store,
		}),
		NewPRCommentPoster(PRCommentPosterConfig{
			Name:     "pr-summary-poster",
			Upstream: "pr-summarizer",
			Kind:     "summary",
			GitHub:   posterClient,
			Store:    d.Store,
		}),
	)

	// plan-btu workflow handlers.
	planState := planning.NewPlanningState()
	msMap := d.MsMap
	if msMap == nil {
		msMap = planning.NewMicroserviceMap("")
	}
	handlers = append(handlers,
		planning.NewReader(d.Confluence, d.Store, planState, logger),
		planning.NewResearcher([]backend.Backend{d.Backend}, msMap, planState, logger),
		planning.NewArchitect(d.Backend, planState, msMap, logger),
		planning.NewEstimator(d.Backend, d.Estimation, planState, logger),
		planning.NewWriter(d.Confluence, planState, logger),
	)

	// plan-jira + task-creator workflow handlers.
	jpState := jiraplanner.NewPlanningState()
	handlers = append(handlers,
		jiraplanner.NewPageReader(d.Confluence, d.Store, jpState, logger),
		jiraplanner.NewManager(d.Backend, jpState, logger),
		jiraplanner.NewTaskCreator(d.Jira, jpState, logger),
		jiraplanner.NewStandaloneCreator(d.Backend, d.Jira, d.Store, logger),
	)

	for _, h := range handlers {
		if err := reg.Register(h); err != nil {
			return err
		}
	}
	return nil
}
