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
	// reviewer, qa, pr-category reviewers, feedback-analyzer, pr-replier,
	// qa-analyzer). Callers should build this via backend.NewReviewBackend()
	// so the rotation can be configured through RICK_REVIEW_BACKENDS. When
	// nil, the legacy single-backend fallback is used (d.Backend if it's
	// already gemini, otherwise a bare gemini driver). pr-consolidator does
	// NOT use this — it pins to claude+haiku internally (see
	// NewPRConsolidator).
	ReviewBackend backend.Backend
	// BackendTimeout caps how long AIHandler.backend.Run may block for the
	// developer phase (and any phase without a more specific override).
	// Zero falls back to handler.DefaultBackendTimeout. Set explicitly via
	// RICK_BACKEND_TIMEOUT in serve mode.
	BackendTimeout time.Duration
	// ReviewBackendTimeout caps review-phase handlers (reviewer, qa,
	// feedback-analyzer, pr-* reviewers, qa-analyzer, pr-replier,
	// committer). Shorter than the
	// developer timeout because review/commit runs are typically <5 min —
	// a hung reviewer should surface fast instead of blocking a workflow
	// for 20 minutes. Zero falls back to handler.DefaultReviewBackendTimeout.
	// Set via RICK_REVIEW_BACKEND_TIMEOUT.
	ReviewBackendTimeout time.Duration
}

// DefaultBackendTimeout is the fallback hard cap on the developer AI backend
// call when Deps.BackendTimeout is zero. Picked to be longer than any
// reasonable developer run we've observed (~10 min on heavy refactors) while
// still being short enough to surface a wedged subprocess in operator time.
// Override via RICK_BACKEND_TIMEOUT or by setting Deps.BackendTimeout.
const DefaultBackendTimeout = 20 * time.Minute

// DefaultReviewBackendTimeout caps review/commit/feedback handlers. Reviewer
// and qa p99 observed at ~9 min in production; 15 min leaves ~60% headroom
// above that while still surfacing a wedged CLI well under the 20-min
// developer budget. Committer and pr-* category reviewers run even shorter
// (under 7 min) but share this ceiling for simplicity.
const DefaultReviewBackendTimeout = 15 * time.Minute

// RegisterAll creates and registers all unique handlers. Each handler is
// registered once — workflow DAGs control which handlers participate in
// which workflows.
func RegisterAll(reg *Registry, d Deps) error {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Resolve the backend timeouts once: explicit Deps values win, otherwise
	// fall back to the package defaults. Zero stays zero only when callers
	// have already opted out (e.g., tests using mock backends).
	backendTimeout := d.BackendTimeout
	if backendTimeout == 0 && d.Bus != nil {
		backendTimeout = DefaultBackendTimeout
	}
	reviewTimeout := d.ReviewBackendTimeout
	if reviewTimeout == 0 && d.Bus != nil {
		reviewTimeout = DefaultReviewBackendTimeout
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

	// reviewAiCfg ensures the review backend is used for review-related
	// handlers AND applies the shorter review timeout so wedged CLIs in
	// the review phase don't block for the full developer budget.
	reviewAiCfg := func(name, phase, personaName string) AIHandlerConfig {
		cfg := aiCfg(name, phase, personaName)
		cfg.Backend = reviewBackend
		cfg.BackendTimeout = reviewTimeout
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

		// PR reply composer — text-only (PlainText, Yolo=false) so the LLM
		// cannot run `gh pr comment` itself. The matching poster handler
		// below is responsible for the actual GitHub side-effect.
		func() Handler {
			cfg := reviewAiCfg("pr-replier", "pr-reply", persona.PRReplier)
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
		// pr-consolidator owns its own backend (claude + haiku) — ignores the
		// review-rotation. See NewPRConsolidator for rationale.
		NewPRConsolidator(d),
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
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:    reviewAiCfg("pr-vendor-resilience", "pr-category-review", persona.PRVendorResilience),
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

	// PR comment poster — always registered so the pr-feedback DAG can
	// reference it unconditionally. When d.GitHub is nil the poster records
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
