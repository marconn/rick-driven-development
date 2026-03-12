package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ArchitectHandler is the plan-architect handler.
// It generates a technical implementation plan from BTU requirements and
// codebase research findings. Has tool access to RICK_REPOS_PATH for verifying
// file paths and exploring patterns directly. Uses the Hint system for human review.
type ArchitectHandler struct {
	backend         backend.Backend
	state           *PlanningState
	platformContext string   // full AGENTS.md content
	serviceNames    []string // known microservice names
	reposPath        string   // base repo path for tool access
	logger          *slog.Logger
}

// NewArchitect creates a plan-architect handler.
func NewArchitect(be backend.Backend, state *PlanningState, msMap *MicroserviceMap, logger *slog.Logger) *ArchitectHandler {
	return &ArchitectHandler{
		backend:         be,
		state:           state,
		platformContext: msMap.PlatformContext(),
		serviceNames:    msMap.Names(),
		reposPath:        msMap.ReposPath(),
		logger:          logger,
	}
}

func (a *ArchitectHandler) Name() string            { return "plan-architect" }
func (a *ArchitectHandler) Subscribes() []event.Type { return nil }

// Hint generates the draft plan and emits HintEmitted for human review.
// This is the first phase of two-phase dispatch.
func (a *ArchitectHandler) Hint(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wp := a.state.Get(env.CorrelationID)
	wp.mu.RLock()
	title := wp.BTUTitle
	content := wp.BTUContent
	userTypes := wp.UserTypes
	devices := wp.Devices
	research := wp.ResearchFindings
	wp.mu.RUnlock()

	a.logger.Info("generating draft technical plan", slog.String("btu", title))

	// Build platform context for the prompt.
	// Prefer full AGENTS.md (architecture, BFF maps, domain guide, repo index).
	// Fall back to just service names if no markdown context available.
	platformCtx := a.platformContext
	if platformCtx == "" && len(a.serviceNames) > 0 {
		platformCtx = "Microservicios disponibles: " + strings.Join(a.serviceNames, ", ")
	} else if platformCtx == "" {
		platformCtx = "No hay contexto de plataforma disponible."
	}

	prompt := renderTemplate(PlanArchitectUserPromptTemplate, map[string]string{
		"BTUTitle":           title,
		"BTUContent":         content,
		"ResearchFindings":   research,
		"UserTypes":          userTypes,
		"Devices":            devices,
		"KnownMicroservices": platformCtx,
	})

	// Append mandatory JSON output instruction
	prompt += `

---
REQUISITO OBLIGATORIO: Al final de tu respuesta, DEBES incluir un bloque JSON valido con esta estructura exacta. Sin este JSON, tu respuesta sera descartada:

{"summary":"resumen tecnico del approach","tasks":[{"description":"descripcion de la tarea","microservice":"nombre-servicio","category":"frontend|backend|infra","files":["ruta/exacta/archivo"],"notes":"consideraciones relevantes"}],"microservices":["servicio1","servicio2"],"risks":[{"description":"descripcion del riesgo","probability":"alta|media|baja","impact":"impacto","mitigation":"mitigacion"}],"dependencies":[{"name":"BTU-XXXX o servicio","description":"descripcion"}],"user_device_notes":"consideraciones por tipo de usuario y dispositivo"}

El JSON DEBE ser la ultima cosa en tu respuesta. No pongas texto despues del JSON.`

	resp, err := a.backend.Run(ctx, backend.Request{
		SystemPrompt: PlanArchitectSystemPrompt,
		UserPrompt:   prompt,
		WorkDir:      a.reposPath, // tool access to all repos for path verification
		Yolo:         true,
	})
	if err != nil {
		return nil, fmt.Errorf("plan-architect: generate: %w", err)
	}

	output := resp.Output

	// Parse structured plan from output
	plan, err := ParseTechnicalPlan(output)
	if err != nil {
		a.logger.Warn("failed to parse structured plan, using raw output", slog.Any("error", err))
	}

	// Store draft plan in shared state
	wp.mu.Lock()
	wp.Plan = plan
	wp.mu.Unlock()

	hintPayload := event.HintEmittedPayload{
		Persona:      "plan-architect",
		Confidence:   0.5,
		Plan:         output,
		TriggerEvent: string(env.Type),
		TriggerID:    string(env.ID),
	}

	return []event.Envelope{
		event.New(event.HintEmitted, 1, event.MustMarshal(hintPayload)).WithSource("handler:plan-architect"),
	}, nil
}

// Handle is called after HintApproved -- finalizes the plan.
// Falls back to generating the plan directly if Hint wasn't called.
func (a *ArchitectHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wp := a.state.Get(env.CorrelationID)
	wp.mu.RLock()
	plan := wp.Plan
	wp.mu.RUnlock()

	// Fallback: generate plan if Hint phase was skipped.
	if plan == nil {
		a.logger.Info("no draft plan in state, generating directly (hint phase skipped)")
		hintResult, err := a.Hint(ctx, env)
		if err != nil {
			return nil, fmt.Errorf("plan-architect: generate plan: %w", err)
		}
		// Hint stores the plan in state; re-read it.
		wp.mu.RLock()
		plan = wp.Plan
		wp.mu.RUnlock()
		if plan == nil {
			return nil, fmt.Errorf("plan-architect: plan generation produced no result")
		}
		_ = hintResult // hint events not used in single-phase mode
	}

	a.logger.Info("plan finalized",
		slog.Int("tasks", len(plan.Tasks)),
		slog.Int("risks", len(plan.Risks)),
	)

	// Check if operator guidance was provided (adjustments)
	guidance := extractGuidance(env)
	if guidance != "" {
		a.logger.Info("applying operator adjustments to plan")
		adjustedPlan, err := a.applyAdjustments(ctx, plan, guidance)
		if err != nil {
			a.logger.Warn("failed to apply adjustments, keeping original", slog.Any("error", err))
		} else {
			plan = adjustedPlan
			wp.mu.Lock()
			wp.Plan = plan
			wp.mu.Unlock()
		}
	}

	// Emit plan as enrichment for downstream handlers
	planJSON, _ := json.Marshal(plan)
	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "plan-architect",
		Kind:    "technical-plan",
		Summary: fmt.Sprintf("Plan tecnico: %d tareas, %d riesgos, %d dependencias", len(plan.Tasks), len(plan.Risks), len(plan.Dependencies)),
	})).WithSource("handler:plan-architect")

	planEvt := event.New("plan.generated", 1, planJSON).WithSource("handler:plan-architect")

	return []event.Envelope{enrichEvt, planEvt}, nil
}

// applyAdjustments uses AI to incorporate operator feedback into the plan.
func (a *ArchitectHandler) applyAdjustments(ctx context.Context, plan *TechnicalPlan, guidance string) (*TechnicalPlan, error) {
	planJSON, _ := json.Marshal(plan)

	prompt := fmt.Sprintf(`El operador ha proporcionado los siguientes ajustes al plan tecnico:

## Ajustes solicitados
%s

## Plan actual (JSON)
%s

Aplica los ajustes solicitados y devuelve el plan actualizado en el mismo formato JSON.
Solo modifica lo que el operador pidio, manten todo lo demas igual.`, guidance, string(planJSON))

	resp, err := a.backend.Run(ctx, backend.Request{
		SystemPrompt: PlanArchitectSystemPrompt,
		UserPrompt:   prompt,
		WorkDir:      a.reposPath,
		Yolo:         true,
	})
	if err != nil {
		return nil, err
	}

	return ParseTechnicalPlan(resp.Output)
}

func extractGuidance(env event.Envelope) string {
	var payload map[string]any
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ""
	}
	if guidance, ok := payload["guidance"].(string); ok {
		return guidance
	}
	return ""
}
