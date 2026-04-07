package engine

import "github.com/marconn/rick-event-driven-development/internal/event"

// WorkflowDef defines a workflow's execution topology and completion criteria.
// Graph declares the DAG — each handler maps to its predecessors. Required
// lists the handlers that must all complete for the workflow to succeed.
// Ordering comes from Graph, not from handler-declared triggers.
type WorkflowDef struct {
	ID                string                       // workflow identifier (e.g., "workspace-dev", "jira-dev")
	Required          []string                     // persona names that must emit PersonaCompleted
	Graph             map[string][]string           // handler → predecessors that must complete before it (DAG)
	MaxIterations     int                           // max feedback loop iterations (default: 3)
	EscalateOnMaxIter bool                          // pause instead of fail when max iterations reached
	HintThreshold     float64                       // auto-approve hints above this confidence (0 = always ask, 1 = never ask, default: 0.7)
	PhaseMap          map[string]string             // phase verb → handler name (e.g., "develop" → "developer")
	RetriggeredBy     map[string][]event.Type       // handler → extra event types that re-trigger it (e.g., developer → [FeedbackGenerated])
}

// DownstreamOf returns all personas that transitively depend on the given
// persona in the Graph, including the persona itself. Used to invalidate
// stale completions after a feedback loop re-triggers a persona.
func (d *WorkflowDef) DownstreamOf(persona string) []string {
	// Build reverse adjacency: for each node, who depends on it?
	dependents := make(map[string][]string)
	for h, deps := range d.Graph {
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], h)
		}
	}

	// BFS from persona.
	visited := map[string]bool{persona: true}
	queue := []string{persona}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, dep := range dependents[current] {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}

	result := make([]string, 0, len(visited))
	for p := range visited {
		result = append(result, p)
	}
	return result
}

// ResolvePhase maps a verdict phase name to the corresponding required persona
// name. Falls back to the phase name itself if no mapping exists (handles cases
// where phase == persona, e.g., "qa" → "qa").
func (d *WorkflowDef) ResolvePhase(phase string) string {
	if d.PhaseMap != nil {
		if persona, ok := d.PhaseMap[phase]; ok {
			return persona
		}
	}
	return phase
}

// corePhaseMap maps phase verbs used by built-in AI handlers to their
// persona (handler) names. Only includes entries where the two differ.
var corePhaseMap = map[string]string{
	"research": "researcher",
	"develop":  "developer",
	"review":   "reviewer",
	"commit":   "committer",
}

// DevelopOnlyWorkflowDef returns a minimal workflow for quick dev tasks.
// Provisions a workspace first, then developer → reviewer → committer.
// RetriggeredBy enables the feedback loop: a VerdictRendered{fail} from the
// committer (e.g. no changes detected) causes FeedbackGenerated which
// re-triggers developer rather than deadlocking the workflow.
func DevelopOnlyWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "develop-only",
		Required: []string{"workspace", "developer", "reviewer", "committer"},
		Graph: map[string][]string{
			"workspace": {},
			"developer": {"workspace"},
			"reviewer":  {"developer"},
			"committer": {"reviewer"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations: 3,
		PhaseMap:      corePhaseMap,
	}
}

// WorkspaceDevWorkflowDef returns a workflow that provisions a git workspace
// first, then runs the full development pipeline.
func WorkspaceDevWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "workspace-dev",
		Required: []string{"workspace", "context-snapshot", "developer", "quality-gate", "reviewer", "qa", "committer"},
		Graph: map[string][]string{
			"workspace":        {},
			"context-snapshot": {"workspace"},
			"developer":        {"context-snapshot"},
			"reviewer":         {"developer"},
			"qa":               {"developer"},
			"quality-gate":     {"reviewer", "qa"},
			"committer":        {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		PhaseMap:          corePhaseMap,
	}
}

// PRReviewWorkflowDef returns the pr-review v2 workflow definition.
// Flow: pr-workspace → pr-jira-context → architect + reviewer + qa (parallel)
// → pr-consolidator (posts consolidated GitHub comment) → pr-cleanup.
func PRReviewWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID: "pr-review",
		Required: []string{
			"pr-workspace", "pr-jira-context",
			"architect", "reviewer", "qa",
			"pr-consolidator", "pr-cleanup",
		},
		Graph: map[string][]string{
			"pr-workspace":    {},
			"pr-jira-context": {"pr-workspace"},
			"architect":       {"pr-jira-context"},
			"reviewer":        {"pr-jira-context"},
			"qa":              {"pr-jira-context"},
			"pr-consolidator": {"architect", "reviewer", "qa"},
			"pr-cleanup":      {"pr-consolidator"},
		},
		MaxIterations: 1,
	}
}

// PRFeedbackWorkflowDef returns a workflow for addressing external PR review
// feedback. Provisions workspace first, analyzer triages comments, context-snapshot
// captures codebase state for dependency validation, developer implements fixes,
// reviewer checks fixes, committer pushes.
func PRFeedbackWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "pr-feedback",
		Required: []string{"workspace", "feedback-analyzer", "context-snapshot", "developer", "reviewer", "qa", "quality-gate", "committer"},
		Graph: map[string][]string{
			"workspace":         {},
			"feedback-analyzer": {"workspace"},
			"context-snapshot":  {"feedback-analyzer"},
			"developer":         {"context-snapshot"},
			"reviewer":          {"developer"},
			"qa":                {"developer"},
			"quality-gate":      {"reviewer", "qa"},
			"committer":         {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		PhaseMap: map[string]string{
			"feedback-analyze": "feedback-analyzer",
			"develop":          "developer",
			"feedback-verify":  "reviewer",
			"commit":           "committer",
		},
	}
}

// JiraDevWorkflowDef returns a workflow that reads a Jira ticket, provisions
// a workspace, snapshots the codebase, then runs the full development pipeline.
func JiraDevWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID: "jira-dev",
		Required: []string{
			"jira-context", "workspace", "context-snapshot",
			"researcher", "architect", "developer",
			"quality-gate", "reviewer", "qa", "committer",
		},
		Graph: map[string][]string{
			"jira-context":     {},
			"workspace":        {"jira-context"},
			"context-snapshot": {"workspace"},
			"researcher":       {"context-snapshot"},
			"architect":        {"researcher"},
			"developer":        {"architect"},
			"reviewer":         {"developer"},
			"qa":               {"developer"},
			"quality-gate":     {"reviewer", "qa"},
			"committer":        {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		PhaseMap:          corePhaseMap,
	}
}

// PlanBTUWorkflowDef returns a workflow for technical planning from Confluence
// BTU documents.
func PlanBTUWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "plan-btu",
		Required: []string{"confluence-reader", "codebase-researcher", "plan-architect", "estimator", "confluence-writer"},
		Graph: map[string][]string{
			"confluence-reader":   {},
			"codebase-researcher": {"confluence-reader"},
			"plan-architect":      {"codebase-researcher"},
			"estimator":           {"plan-architect"},
			"confluence-writer":   {"estimator"},
		},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		HintThreshold:     0,
	}
}

// JiraQAStepsWorkflowDef returns a workflow that reads a Jira ticket, finds
// the associated PR, generates QA test scenarios via AI, and writes them back
// to the Jira ticket's QA Steps field. Single pass, no feedback loops.
func JiraQAStepsWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "jira-qa-steps",
		Required: []string{"qa-context", "qa-analyzer", "qa-jira-writer"},
		Graph: map[string][]string{
			"qa-context":     {},
			"qa-analyzer":    {"qa-context"},
			"qa-jira-writer": {"qa-analyzer"},
		},
		MaxIterations: 1,
		PhaseMap: map[string]string{
			"qa-analyze": "qa-analyzer",
		},
	}
}

// PlanJiraWorkflowDef returns a workflow that reads a Confluence page, uses AI
// to generate a structured project plan, then creates Jira epic + tasks.
func PlanJiraWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "plan-jira",
		Required: []string{"page-reader", "project-manager", "jira-task-creator"},
		Graph: map[string][]string{
			"page-reader":       {},
			"project-manager":   {"page-reader"},
			"jira-task-creator": {"project-manager"},
		},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		HintThreshold:     0,
	}
}

// TaskCreatorWorkflowDef returns a standalone workflow that generates Jira
// epic + tasks from a plain text prompt without Confluence.
func TaskCreatorWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "task-creator",
		Required: []string{"task-creator"},
		Graph: map[string][]string{
			"task-creator": {},
		},
		MaxIterations: 1,
	}
}

// WithoutHandler returns a copy of def with the named handler removed from
// Required and Graph. Dependents of the removed handler are rewired to point
// at its predecessors, preserving the DAG structure.
// Returns the original def unchanged if the handler is not in the Graph.
func WithoutHandler(def WorkflowDef, handler string) WorkflowDef {
	preds, exists := def.Graph[handler]
	if !exists {
		return def
	}

	// Copy Required, excluding the handler.
	newReq := make([]string, 0, len(def.Required))
	for _, r := range def.Required {
		if r != handler {
			newReq = append(newReq, r)
		}
	}
	def.Required = newReq

	// Copy Graph, rewire dependents.
	newGraph := make(map[string][]string, len(def.Graph)-1)
	for h, deps := range def.Graph {
		if h == handler {
			continue
		}
		var newDeps []string
		replaced := false
		for _, d := range deps {
			if d == handler {
				replaced = true
			} else {
				newDeps = append(newDeps, d)
			}
		}
		if replaced {
			newDeps = append(newDeps, preds...)
			// Deduplicate.
			seen := make(map[string]bool, len(newDeps))
			deduped := make([]string, 0, len(newDeps))
			for _, d := range newDeps {
				if !seen[d] {
					seen[d] = true
					deduped = append(deduped, d)
				}
			}
			newDeps = deduped
		}
		newGraph[h] = newDeps
	}
	def.Graph = newGraph

	return def
}

// CIFixWorkflowDef returns a workflow for fixing CI failures detected after
// a committer push. Provisions workspace, developer fixes the issue, reviewer
// + qa validate, committer pushes again.
func CIFixWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "ci-fix",
		Required: []string{"workspace", "developer", "quality-gate", "reviewer", "qa", "committer"},
		Graph: map[string][]string{
			"workspace":    {},
			"developer":    {"workspace"},
			"reviewer":     {"developer"},
			"qa":           {"developer"},
			"quality-gate": {"reviewer", "qa"},
			"committer":    {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:     2,
		EscalateOnMaxIter: true,
		PhaseMap:          corePhaseMap,
	}
}
