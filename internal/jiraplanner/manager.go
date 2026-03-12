package jiraplanner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ManagerHandler runs the project-manager AI to generate a structured project
// plan from Confluence page content. Implements Hinter for two-phase dispatch:
// Hint() generates the plan and pauses for operator review, Handle() confirms.
type ManagerHandler struct {
	backend backend.Backend
	state   *PlanningState
	logger  *slog.Logger
}

// NewManager creates a project-manager handler.
func NewManager(be backend.Backend, state *PlanningState, logger *slog.Logger) *ManagerHandler {
	return &ManagerHandler{backend: be, state: state, logger: logger}
}

func (m *ManagerHandler) Name() string            { return "project-manager" }
func (m *ManagerHandler) Subscribes() []event.Type { return nil }

// Hint generates the project plan and emits HintEmitted for operator review.
func (m *ManagerHandler) Hint(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wd := m.state.Get(env.CorrelationID)
	wd.mu.RLock()
	title := wd.PageTitle
	content := wd.PageContent
	wd.mu.RUnlock()

	m.logger.Info("generating project plan", slog.String("page", title))

	prompt := renderTemplate(ProjectManagerUserPromptTemplate, map[string]string{
		"PageTitle":   title,
		"PageContent": content,
	})

	resp, err := m.backend.Run(ctx, backend.Request{
		SystemPrompt: ProjectManagerSystemPrompt,
		UserPrompt:   prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("project-manager: generate: %w", err)
	}
	output := resp.Output

	plan, err := ParseProjectPlan(output)
	if err != nil {
		m.logger.Warn("failed to parse project plan, using raw output as fallback", slog.Any("error", err))
		plan = &ProjectPlan{Goal: title, EpicTitle: title, EpicDesc: output}
	}

	wd.mu.Lock()
	wd.Plan = plan
	wd.mu.Unlock()

	hintPayload := event.HintEmittedPayload{
		Persona:      "project-manager",
		Confidence:   0.5,
		Plan:         output,
		TriggerEvent: string(env.Type),
		TriggerID:    string(env.ID),
	}

	return []event.Envelope{
		event.New(event.HintEmitted, 1, event.MustMarshal(hintPayload)).WithSource("handler:project-manager"),
	}, nil
}

// Handle confirms the approved plan and emits a context enrichment summary.
// If no plan exists in state (hint phase was skipped), generates one inline.
func (m *ManagerHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wd := m.state.Get(env.CorrelationID)
	wd.mu.RLock()
	plan := wd.Plan
	wd.mu.RUnlock()

	if plan == nil {
		m.logger.Info("no plan in state, generating directly (hint phase skipped)")
		if _, err := m.Hint(ctx, env); err != nil {
			return nil, fmt.Errorf("project-manager: fallback generate: %w", err)
		}
		wd.mu.RLock()
		plan = wd.Plan
		wd.mu.RUnlock()
		if plan == nil {
			return nil, fmt.Errorf("project-manager: plan generation produced no result")
		}
	}

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "project-manager",
		Kind:    "project-plan",
		Summary: fmt.Sprintf("Plan aprobado: %s — %d tareas, %d riesgos", plan.EpicTitle, len(plan.Tasks), len(plan.Risks)),
	})).WithSource("handler:project-manager")

	return []event.Envelope{enrichEvt}, nil
}
