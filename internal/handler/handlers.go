package handler

import (
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/estimation"
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
}

// RegisterAll creates and registers all unique handlers. Each handler is
// registered once — workflow DAGs control which handlers participate in
// which workflows.
func RegisterAll(reg *Registry, d Deps) error {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	handlers := []Handler{
		// Core AI handlers — used across default, workspace-dev, jira-dev, pr-review,
		// pr-feedback, ci-fix workflows via DAG scoping.
		NewAIHandler(AIHandlerConfig{
			Name:     "researcher",
			Phase:    "research",
			Persona:  persona.Researcher,
			Backend:  d.Backend,
			Store:    d.Store,
			Personas: d.Personas,
			Builder:  d.Builder,
			WorkDir:  d.WorkDir,
			Yolo:     d.Yolo,
		}),
		NewAIHandler(AIHandlerConfig{
			Name:     "architect",
			Phase:    "architect",
			Persona:  persona.Architect,
			Backend:  d.Backend,
			Store:    d.Store,
			Personas: d.Personas,
			Builder:  d.Builder,
			WorkDir:  d.WorkDir,
			Yolo:     d.Yolo,
		}),
		NewAIHandler(AIHandlerConfig{
			Name:     "developer",
			Phase:    "develop",
			Persona:  persona.Developer,
			Backend:  d.Backend,
			Store:    d.Store,
			Personas: d.Personas,
			Builder:  d.Builder,
			WorkDir:  d.WorkDir,
			Yolo:     d.Yolo,
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig: AIHandlerConfig{
				Name:     "reviewer",
				Phase:    "review",
				Persona:  persona.Reviewer,
				Backend:  d.Backend,
				Store:    d.Store,
				Personas: d.Personas,
				Builder:  d.Builder,
				WorkDir:  d.WorkDir,
				Yolo:     d.Yolo,
			},
			TargetPhase: "develop",
		}),
		NewReviewHandler(ReviewHandlerConfig{
			AIConfig: AIHandlerConfig{
				Name:     "qa",
				Phase:    "qa",
				Persona:  persona.QA,
				Backend:  d.Backend,
				Store:    d.Store,
				Personas: d.Personas,
				Builder:  d.Builder,
				WorkDir:  d.WorkDir,
				Yolo:     d.Yolo,
			},
			TargetPhase: "develop",
		}),
		NewAIHandler(AIHandlerConfig{
			Name:     "committer",
			Phase:    "commit",
			Persona:  persona.Committer,
			Backend:  d.Backend,
			Store:    d.Store,
			Personas: d.Personas,
			Builder:  d.Builder,
			WorkDir:  d.WorkDir,
			Yolo:     d.Yolo,
		}),

		// Feedback-specific AI handler.
		NewAIHandler(AIHandlerConfig{
			Name:     "feedback-analyzer",
			Phase:    "feedback-analyze",
			Persona:  persona.FeedbackAnalyzer,
			Backend:  d.Backend,
			Store:    d.Store,
			Personas: d.Personas,
			Builder:  d.Builder,
			WorkDir:  d.WorkDir,
			Yolo:     d.Yolo,
		}),

		// Non-AI handlers.
		NewWorkspace(d),
		NewContextSnapshot(d),
		NewQualityGate(d),

		// PR-specific handlers.
		NewPRWorkspace(d),
		NewPRJiraContext(d),
		NewPRConsolidator(d),
		NewPRCleanup(d),

		// Jira context handler (jira-dev workflow).
		NewJiraContext(d),

		// QA-steps-specific handlers.
		NewQAContext(d),
		NewAIHandler(AIHandlerConfig{
			Name:      "qa-analyzer",
			Phase:     "qa-analyze",
			Persona:   persona.QAAnalyzer,
			Backend:   d.Backend,
			Store:     d.Store,
			Personas:  d.Personas,
			Builder:   d.Builder,
			WorkDir:   d.WorkDir,
			Yolo:      d.Yolo,
			PlainText: true,
		}),
		NewQAJiraWriter(d),
	}

	// GitHub PR fetcher (before-hook for feedback-analyzer in pr-feedback workflow).
	if d.GitHub != nil {
		handlers = append(handlers,
			gh.NewFetcherHandler(d.GitHub, d.Store, d.PluginStore, logger),
		)
	}

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
