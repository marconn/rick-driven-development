package planning

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// WriterHandler is the confluence-writer handler.
// It assembles the final technical plan with estimates and writes it
// to the Confluence BTU page under the "Plan Tecnico de Implementacion" heading.
type WriterHandler struct {
	confluence *confluence.Client
	state      *PlanningState
	logger     *slog.Logger
}

// NewWriter creates a confluence-writer handler.
func NewWriter(cf *confluence.Client, state *PlanningState, logger *slog.Logger) *WriterHandler {
	return &WriterHandler{confluence: cf, state: state, logger: logger}
}

// PlanHeading is the Confluence heading we write after.
const PlanHeading = "plan tecnico de implementacion"

func (w *WriterHandler) Name() string            { return "confluence-writer" }
func (w *WriterHandler) Subscribes() []event.Type { return nil }

// Handle writes the final plan to Confluence.
func (w *WriterHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wp := w.state.Get(env.CorrelationID)
	wp.mu.RLock()
	pageID := wp.PageID
	plan := wp.EstimatedPlan
	totalPoints := wp.TotalPoints
	title := wp.BTUTitle
	wp.mu.RUnlock()

	if plan == nil {
		return nil, fmt.Errorf("confluence-writer: no estimated plan for %s", env.CorrelationID)
	}
	if pageID == "" {
		return nil, fmt.Errorf("confluence-writer: no page ID for %s", env.CorrelationID)
	}

	if w.confluence == nil {
		return nil, fmt.Errorf("confluence-writer: CONFLUENCE_URL not configured")
	}

	w.logger.Info("writing plan to Confluence",
		slog.String("page_id", pageID),
		slog.Int("total_points", totalPoints),
	)

	// Build Confluence HTML for the plan section
	html := w.buildPlanHTML(plan, totalPoints)

	// Re-read the page to get current version (avoid stale version conflicts)
	page, err := w.confluence.ReadPage(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("confluence-writer: re-read page: %w", err)
	}

	// Update the plan section
	if err := w.confluence.UpdatePageSection(ctx, page, PlanHeading, html); err != nil {
		return nil, fmt.Errorf("confluence-writer: update section: %w", err)
	}

	w.logger.Info("plan written to Confluence",
		slog.String("page_id", pageID),
		slog.String("title", title),
	)

	// Clean up shared state
	w.state.Delete(env.CorrelationID)

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "confluence-writer",
		Kind:    "confluence-update",
		Summary: fmt.Sprintf("Plan escrito en Confluence page %s (%d puntos)", pageID, totalPoints),
	})).WithSource("handler:confluence-writer")

	return []event.Envelope{enrichEvt}, nil
}

// buildPlanHTML converts the TechnicalPlan into Confluence storage format HTML.
func (w *WriterHandler) buildPlanHTML(plan *TechnicalPlan, totalPoints int) string {
	var sb strings.Builder

	// Header with total points
	fmt.Fprintf(&sb, "<h2>Plan T&eacute;cnico de Implementaci&oacute;n</h2>\n")
	fmt.Fprintf(&sb, "<p><span style=\"color: rgb(255,86,48);\"><strong>Estimado: %d puntos</strong></span></p>\n", totalPoints)
	sb.WriteString("<hr />\n")

	// Technical summary
	sb.WriteString("<h3>Resumen T&eacute;cnico del Concepto</h3>\n")
	if plan.Summary != "" {
		// If summary is long and tasks are empty, this is a raw AI output fallback.
		// Render the full summary as the plan content.
		if len(plan.Tasks) == 0 && len(plan.Summary) > 200 {
			sb.WriteString("<p><em>Plan generado por IA (formato libre):</em></p>\n")
			for _, para := range splitParagraphs(plan.Summary) {
				fmt.Fprintf(&sb, "<p>%s</p>\n", escapeHTML(para))
			}
			sb.WriteString("<hr />\n")
			// Skip the empty structured sections -- the summary IS the plan
			sb.WriteString("<h3>Decisiones del Ingeniero (obligatorio)</h3>\n")
			sb.WriteString("<ul>\n")
			sb.WriteString("<li><p><strong>Trade-offs elegidos:</strong> [qu&eacute; alternativas descartaste y por qu&eacute;]</p></li>\n")
			sb.WriteString("<li><p><strong>Dependencias:</strong> [BTUs o servicios que bloquean esto]</p></li>\n")
			sb.WriteString("<li><p><strong>Riesgos que acepto:</strong> [qu&eacute; riesgos identificados son aceptables]</p></li>\n")
			sb.WriteString("</ul>\n")
			return sb.String()
		}
		for _, para := range splitParagraphs(plan.Summary) {
			fmt.Fprintf(&sb, "<p>%s</p>\n", escapeHTML(para))
		}
	}

	// Tasks by category
	sb.WriteString("<h3>Tareas T&eacute;cnicas Propuestas</h3>\n")
	w.writeTasksByCategory(&sb, plan.Tasks, "frontend", "Frontend (VueJS 2.0)")
	w.writeTasksByCategory(&sb, plan.Tasks, "backend", "Backend (Go)")
	w.writeTasksByCategory(&sb, plan.Tasks, "infra", "Infraestructura / Otros")

	// Microservices
	sb.WriteString("<h3>Microservicios Involucrados</h3>\n")
	sb.WriteString("<ul>\n")
	for _, ms := range plan.Microservices {
		fmt.Fprintf(&sb, "<li><p>%s</p></li>\n", escapeHTML(ms))
	}
	sb.WriteString("</ul>\n")

	// Risks
	sb.WriteString("<h3>Riesgos T&eacute;cnicos</h3>\n")
	if len(plan.Risks) > 0 {
		sb.WriteString("<table>\n<tr><th>Riesgo</th><th>Probabilidad</th><th>Impacto</th><th>Mitigaci&oacute;n</th></tr>\n")
		for _, risk := range plan.Risks {
			fmt.Fprintf(&sb, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				escapeHTML(risk.Description), escapeHTML(risk.Probability),
				escapeHTML(risk.Impact), escapeHTML(risk.Mitigation))
		}
		sb.WriteString("</table>\n")
	} else {
		sb.WriteString("<p>Sin riesgos t&eacute;cnicos identificados.</p>\n")
	}

	// Dependencies
	sb.WriteString("<h3>Dependencias</h3>\n")
	if len(plan.Dependencies) > 0 {
		sb.WriteString("<ul>\n")
		for _, dep := range plan.Dependencies {
			fmt.Fprintf(&sb, "<li><p><strong>%s</strong>: %s</p></li>\n",
				escapeHTML(dep.Name), escapeHTML(dep.Description))
		}
		sb.WriteString("</ul>\n")
	} else {
		sb.WriteString("<p>Sin dependencias bloqueantes identificadas.</p>\n")
	}

	// User/device notes
	sb.WriteString("<h3>Consideraciones por Tipo de Usuario y Dispositivo</h3>\n")
	if plan.UserDeviceNotes != "" {
		for _, para := range splitParagraphs(plan.UserDeviceNotes) {
			fmt.Fprintf(&sb, "<p>%s</p>\n", escapeHTML(para))
		}
	} else {
		sb.WriteString("<p>Sin consideraciones espec&iacute;ficas.</p>\n")
	}

	sb.WriteString("<hr />\n")

	// Engineer decisions (placeholders for the engineer to fill)
	sb.WriteString("<h3>Decisiones del Ingeniero (obligatorio)</h3>\n")
	sb.WriteString("<ul>\n")
	sb.WriteString("<li><p><strong>Trade-offs elegidos:</strong> [qu&eacute; alternativas descartaste y por qu&eacute;]</p></li>\n")
	sb.WriteString("<li><p><strong>Dependencias:</strong> [BTUs o servicios que bloquean esto]</p></li>\n")
	sb.WriteString("<li><p><strong>Riesgos que acepto:</strong> [qu&eacute; riesgos identificados son aceptables]</p></li>\n")
	sb.WriteString("</ul>\n")

	return sb.String()
}

func (w *WriterHandler) writeTasksByCategory(sb *strings.Builder, tasks []Task, category, heading string) {
	var filtered []Task
	for _, t := range tasks {
		if strings.EqualFold(t.Category, category) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		return
	}

	fmt.Fprintf(sb, "<h4>%s</h4>\n", heading)
	sb.WriteString("<ul>\n")
	for _, t := range filtered {
		var parts []string
		if t.Microservice != "" {
			parts = append(parts, fmt.Sprintf("[%s]", t.Microservice))
		}
		if t.Points > 0 {
			parts = append(parts, fmt.Sprintf("(%d pts)", t.Points))
		}
		parts = append(parts, t.Description)

		fmt.Fprintf(sb, "<li><p>%s</p>\n", escapeHTML(strings.Join(parts, " ")))

		// Sub-details
		if len(t.Files) > 0 {
			fmt.Fprintf(sb, "<ul><li><p><em>Archivos:</em> %s</p></li></ul>\n", escapeHTML(strings.Join(t.Files, ", ")))
		}
		if t.Notes != "" {
			fmt.Fprintf(sb, "<ul><li><p><em>Notas:</em> %s</p></li></ul>\n", escapeHTML(t.Notes))
		}
		if t.Justification != "" {
			fmt.Fprintf(sb, "<ul><li><p><em>Justificaci&oacute;n:</em> %s</p></li></ul>\n", escapeHTML(t.Justification))
		}
		sb.WriteString("</li>\n")
	}
	sb.WriteString("</ul>\n")
}
