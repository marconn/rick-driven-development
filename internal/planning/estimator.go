package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/estimation"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// EstimatorHandler is the estimation persona handler.
// It estimates Fibonacci story points for each task in the technical plan,
// using calibration data and historical estimates.
type EstimatorHandler struct {
	backend backend.Backend
	store   *estimation.Store
	state   *PlanningState
	logger  *slog.Logger
}

// NewEstimator creates an estimator handler.
func NewEstimator(be backend.Backend, store *estimation.Store, state *PlanningState, logger *slog.Logger) *EstimatorHandler {
	return &EstimatorHandler{backend: be, store: store, state: state, logger: logger}
}

func (e *EstimatorHandler) Name() string            { return "estimator" }
func (e *EstimatorHandler) Subscribes() []event.Type { return nil }

// Hint generates estimates and presents them for human review.
func (e *EstimatorHandler) Hint(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wp := e.state.Get(env.CorrelationID)
	wp.mu.RLock()
	title := wp.BTUTitle
	plan := wp.Plan
	wp.mu.RUnlock()

	if plan == nil {
		return nil, fmt.Errorf("estimator: no plan found for %s", env.CorrelationID)
	}

	e.logger.Info("estimating tasks", slog.String("btu", title), slog.Int("tasks", len(plan.Tasks)))

	// Load calibration context
	calibrationData, err := e.store.CalibrationSummary(ctx)
	if err != nil {
		e.logger.Warn("failed to load calibration", slog.Any("error", err))
		calibrationData = "No calibration data available."
	}

	// Load similar estimates for context
	similarEstimates := e.loadSimilarEstimates(ctx, plan.Tasks)

	planJSON, _ := json.Marshal(plan)
	prompt := renderTemplate(EstimatorUserPromptTemplate, map[string]string{
		"BTUTitle":         title,
		"Plan":             string(planJSON),
		"CalibrationData":  calibrationData,
		"SimilarEstimates": similarEstimates,
	})

	// Append mandatory JSON output instruction
	prompt += `

---
REQUISITO OBLIGATORIO: Al final de tu respuesta, DEBES incluir un bloque JSON valido con esta estructura exacta. Sin este JSON, tu respuesta sera descartada:

{"tasks":[{"description":"desc","microservice":"nombre","category":"frontend|backend|infra","files":["ruta"],"notes":"notas","points":3,"justification":"razon concisa"}],"total_points":10}

El JSON DEBE ser la ultima cosa en tu respuesta. No pongas texto despues del JSON.`

	resp, err := e.backend.Run(ctx, backend.Request{
		SystemPrompt: EstimatorSystemPrompt,
		UserPrompt:   prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("estimator: generate: %w", err)
	}

	output := resp.Output

	// Parse estimated tasks from output
	estimatedPlan, err := e.parseEstimation(output, plan)
	if err != nil {
		e.logger.Warn("failed to parse estimation JSON, using plan with no points", slog.Any("error", err))
		estimatedPlan = plan
	}

	totalPoints := 0
	for _, t := range estimatedPlan.Tasks {
		totalPoints += t.Points
	}

	// Store in shared state
	wp.mu.Lock()
	wp.EstimatedPlan = estimatedPlan
	wp.TotalPoints = totalPoints
	wp.mu.Unlock()

	hintPayload := event.HintEmittedPayload{
		Persona:      "estimator",
		Confidence:   0.5,
		Plan:         output,
		TriggerEvent: string(env.Type),
		TriggerID:    string(env.ID),
	}

	return []event.Envelope{
		event.New(event.HintEmitted, 1, event.MustMarshal(hintPayload)).WithSource("handler:estimator"),
	}, nil
}

// Handle is called after HintApproved -- saves estimates to SQLite.
// Falls back to generating estimates directly if Hint phase was skipped.
func (e *EstimatorHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wp := e.state.Get(env.CorrelationID)
	wp.mu.RLock()
	title := wp.BTUTitle
	estimatedPlan := wp.EstimatedPlan
	totalPoints := wp.TotalPoints
	wp.mu.RUnlock()

	// Fallback: generate estimates if Hint phase was skipped.
	if estimatedPlan == nil {
		e.logger.Info("no estimates in state, generating directly (hint phase skipped)")
		if _, err := e.Hint(ctx, env); err != nil {
			return nil, fmt.Errorf("estimator: generate estimates: %w", err)
		}
		wp.mu.RLock()
		title = wp.BTUTitle
		estimatedPlan = wp.EstimatedPlan
		totalPoints = wp.TotalPoints
		wp.mu.RUnlock()
	}

	if estimatedPlan == nil {
		return nil, fmt.Errorf("estimator: no estimated plan for %s", env.CorrelationID)
	}

	// Extract ticket ID from BTU title (e.g., "BTU-1724: ...")
	ticketID := extractTicketID(title)

	// Persist estimates to SQLite
	var estimates []estimation.Estimate
	for _, task := range estimatedPlan.Tasks {
		estimates = append(estimates, estimation.Estimate{
			TicketID:        ticketID,
			TaskDescription: task.Description,
			Microservice:    task.Microservice,
			Category:        task.Category,
			EstimatedPoints: task.Points,
			CorrelationID:   env.CorrelationID,
			Notes:           task.Justification,
		})
	}

	if err := e.store.SaveBatch(ctx, estimates); err != nil {
		e.logger.Error("failed to persist estimates", slog.Any("error", err))
		// Non-fatal -- continue with workflow
	} else {
		e.logger.Info("estimates persisted",
			slog.String("ticket", ticketID),
			slog.Int("tasks", len(estimates)),
			slog.Int("total_points", totalPoints),
		)
	}

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "estimator",
		Kind:    "estimation",
		Summary: fmt.Sprintf("Estimacion: %d puntos en %d tareas", totalPoints, len(estimatedPlan.Tasks)),
	})).WithSource("handler:estimator")

	return []event.Envelope{enrichEvt}, nil
}

// parseEstimation extracts estimated tasks from AI output and merges with original plan.
func (e *EstimatorHandler) parseEstimation(output string, originalPlan *TechnicalPlan) (*TechnicalPlan, error) {
	jsonStr := extractJSON(output)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON found in estimator output")
	}

	var parsed struct {
		Tasks       []Task `json:"tasks"`
		TotalPoints int    `json:"total_points"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, fmt.Errorf("parse estimation JSON: %w", err)
	}

	// Merge: use estimated tasks if available, otherwise keep original
	result := *originalPlan
	if len(parsed.Tasks) > 0 {
		result.Tasks = parsed.Tasks
	}
	return &result, nil
}

func (e *EstimatorHandler) loadSimilarEstimates(ctx context.Context, tasks []Task) string {
	if e.store == nil {
		return "No historical data available."
	}

	seen := make(map[string]bool)
	var allSimilar string

	for _, task := range tasks {
		key := task.Microservice + ":" + task.Category
		if seen[key] {
			continue
		}
		seen[key] = true

		similar, err := e.store.SimilarEstimates(ctx, task.Microservice, task.Category)
		if err == nil && similar != "" {
			allSimilar += similar + "\n"
		}
	}

	if allSimilar == "" {
		return "No similar historical estimates found."
	}
	return allSimilar
}

func extractTicketID(title string) string {
	// Look for BTU-XXXX or ING-XXXX pattern
	for _, part := range splitWords(title) {
		if len(part) > 4 && (part[:4] == "BTU-" || part[:4] == "ING-") {
			// Remove trailing punctuation
			clean := part
			for len(clean) > 0 && (clean[len(clean)-1] == ':' || clean[len(clean)-1] == ',' || clean[len(clean)-1] == '.') {
				clean = clean[:len(clean)-1]
			}
			return clean
		}
	}
	return title
}
