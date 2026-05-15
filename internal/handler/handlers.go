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

// personaEffort maps handler names to the Claude CLI --effort reasoning
// level. Unmapped names fall through to claude.go's "high" default so adding
// a new handler can't silently regress its reasoning budget. Only the Claude
// backend honors --effort; Gemini and Codex ignore Request.Effort entirely
// (no equivalent flag), so this map is a no-op under those backends.
//
// Tuning rationale:
//   - architect (max) / researcher (xhigh): planning work where the cost of
//     a wrong plan dwarfs the token cost of deeper reasoning.
//   - qa / reviewer (high): verdict-bearing reviewers need strong reasoning
//     to catch the subtle defects developer iterations miss.
//   - developer (medium): bounded reasoning keeps iteration latency in
//     check; the feedback loop drives correctness rather than per-iter
//     thinking depth.
//   - committer (low): mechanical step — write the commit message, push.
//     No analysis required, the work is already merged.
var personaEffort = map[string]string{
	"architect":  "max",
	"researcher": "xhigh",
	"qa":         "high",
	"reviewer":   "high",
	"developer":  "medium",
	"committer":  "low",
}

// isVerdictBearingReviewer returns true for handler names that emit prose
// VERDICT lines (reviewer, qa, the 12 pr-category reviewers). These need
// PlainText=true so ExtractJSON does not greedily steal an in-prose JSON
// snippet and discard the VERDICT tail (the 2026-04-22 default-optimistic
// pass class). pr-replier and pr-consolidator are intentionally excluded —
// pr-replier writes plain text and is gated separately at its construction
// site; pr-consolidator emits structured inline-comments JSON.
func isVerdictBearingReviewer(name string) bool {
	switch name {
	case "reviewer", "qa",
		"pr-security", "pr-concurrency", "pr-error-handling",
		"pr-observability", "pr-api-contract", "pr-idempotency",
		"pr-testing", "pr-integration", "pr-performance",
		"pr-data", "pr-hygiene", "pr-vendor-resilience":
		return true
	}
	return false
}

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
	// AIHandler resolves the prompt-template name from the handler name via
	// defaultTemplate; callers that need a non-default mapping set Template
	// explicitly on the returned config.
	aiCfg := func(name, personaName string) AIHandlerConfig {
		return AIHandlerConfig{
			Name:           name,
			Persona:        personaName,
			Backend:        d.Backend,
			Store:          d.Store,
			Bus:            d.Bus,
			Personas:       d.Personas,
			Builder:        d.Builder,
			WorkDir:        d.WorkDir,
			Yolo:           d.Yolo,
			BackendTimeout: backendTimeout,
			Effort:         personaEffort[name],
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
	reviewAiCfg := func(name, personaName string) AIHandlerConfig {
		cfg := aiCfg(name, personaName)
		cfg.Backend = reviewBackend
		cfg.BackendTimeout = reviewTimeout
		// VERDICT-bearing reviewers produce prose, not JSON. Without
		// PlainText=true, ExtractJSON greedily captures the first JSON
		// literal the LLM cites (e.g. a dashboard filter snippet) and
		// discards the rest of the output — including the VERDICT line —
		// causing ParseVerdict to default to VerdictSourceDefaultOptimistic
		// and silently drop the real review findings.
		// feedback-analyzer and pr-consolidator are intentionally excluded:
		// feedback-analyzer uses structured JSON for issue extraction, and
		// pr-consolidator emits structured inline-comments JSON.
		if isVerdictBearingReviewer(name) {
			cfg.PlainText = true
		}
		return cfg
	}

	handlers := []Handler{
		// Core AI handlers — used across default, workspace-dev, jira-dev, pr-review,
		// pr-feedback, ci-fix workflows via DAG scoping.
		NewAIHandler(aiCfg("researcher", persona.Researcher)),
		NewAIHandler(aiCfg("architect", persona.Architect)),
		NewDeveloperHandler(aiCfg("developer", persona.Developer)),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("reviewer", persona.Reviewer),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("qa", persona.QA),
			TargetPersona: "developer",
		}),
		NewCommitterHandler(aiCfg("committer", persona.Committer)),

		// Feedback-specific AI handler.
		NewAIHandler(reviewAiCfg("feedback-analyzer", persona.FeedbackAnalyzer)),

		// PR reply composer — text-only (PlainText, Yolo=false) so the LLM
		// cannot run `gh pr comment` itself. The matching poster handler
		// below is responsible for the actual GitHub side-effect.
		func() Handler {
			cfg := reviewAiCfg("pr-replier", persona.PRReplier)
			cfg.PlainText = true
			cfg.Yolo = false
			return NewAIHandler(cfg)
		}(),

		// Non-AI handlers.
		NewWorkspace(d),
		NewContextSnapshot(d),
		NewQualityGate(d),
		// Review consolidator — synchronization barrier for parallel reviewer
		// + qa fan-out. Stateless: rebuilds round state from the event store
		// each dispatch. Active only in workflows that set
		// WorkflowDef.SynchronousFeedback and list this handler in their
		// Graph (see WorkspaceDevWorkflowDef).
		NewReviewConsolidator(ReviewConsolidatorConfig{
			Reviewers:     []string{"reviewer", "qa"},
			TargetPersona: "developer",
			Store:         d.Store,
			Logger:        logger,
		}),

		// PR-specific handlers.
		NewPRWorkspace(d),
		NewPRJiraContext(d),
		// pr-consolidator owns its own backend (claude + haiku) — ignores the
		// review-rotation. See NewPRConsolidator for rationale.
		NewPRConsolidator(d),
		NewPRCleanup(d),

		// PR category reviewers — dedicated single-concern reviewers for pr-review workflow.
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-security", persona.PRSecurity),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-concurrency", persona.PRConcurrency),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-error-handling", persona.PRErrorHandling),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-observability", persona.PRObservability),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-api-contract", persona.PRAPIContract),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-idempotency", persona.PRIdempotency),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-testing", persona.PRTesting),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-integration", persona.PRIntegration),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-performance", persona.PRPerformance),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-data", persona.PRData),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-hygiene", persona.PRHygiene),
			TargetPersona: "developer",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig:      reviewAiCfg("pr-vendor-resilience", persona.PRVendorResilience),
			TargetPersona: "developer",
		}),

		// Jira context handler (jira-dev workflow).
		NewJiraContext(d),

		// GitHub issue context handler (github-dev workflow).
		NewGithubContext(d),

		// QA-steps-specific handlers.
		NewQAContext(d),
		func() Handler {
			cfg := reviewAiCfg("qa-analyzer", persona.QAAnalyzer)
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
