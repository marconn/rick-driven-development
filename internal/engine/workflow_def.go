package engine

import (
	"os"
	"slices"
	"strconv"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// maxIterationsEnvVar is the override for every registered workflow's
// MaxIterations. When set to a positive integer, it replaces the per-workflow
// hardcoded value. Centralised so the Engine and PersonaRunner agree on the
// effective cap without each call site having to remember to apply it.
const maxIterationsEnvVar = "RICK_MAX_ITERATION"

// synchronousFeedbackEnvVar is the kill-switch for the review-consolidator
// synchronization barrier. When set to "0", every registered workflow has its
// SynchronousFeedback flag forced to false at registration time, reverting to
// the legacy per-verdict FeedbackGenerated emission path. Restart-only revert;
// in-flight workflows continue with the snapshot they were registered against.
const synchronousFeedbackEnvVar = "RICK_SYNCHRONOUS_FEEDBACK"

// applyEnvOverrides returns a copy of def with environment-driven overrides
// applied. Currently only RICK_MAX_ITERATION; future per-workflow overrides
// (e.g., RICK_MAX_ITERATION_GITHUB_DEV) can layer in here without touching
// callers. Idempotent: safe to call from both Engine.RegisterWorkflow and
// PersonaRunner.RegisterWorkflow so both internal stores are consistent.
func applyEnvOverrides(def WorkflowDef) WorkflowDef {
	if v := os.Getenv(maxIterationsEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			def.MaxIterations = n
		}
	}
	if os.Getenv(synchronousFeedbackEnvVar) == "0" {
		def.SynchronousFeedback = false
	}
	return def
}

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
	RetriggeredBy     map[string][]event.Type       // handler → extra event types that re-trigger it (e.g., developer → [FeedbackGenerated])
	// MaxChainDepth caps the reactive chain depth for dispatches in this
	// workflow. Zero means "auto": len(Required) + 5, matching the old
	// AdjustChainDepth formula so existing workflows behave identically.
	// Set an explicit value to tighten the guard for short workflows.
	MaxChainDepth int
	// PartialReviewOnFailure changes the semantic of a required-persona
	// failure: instead of emitting WorkflowFailed and cascading context
	// cancellation to siblings, the engine marks the persona as skipped,
	// records it for observability, and lets the workflow proceed. A
	// downstream consolidator (pr-consolidator) can then report which
	// reviewers were skipped and downgrade the review outcome. Intended
	// for parallel-fan-out review workflows where losing one narrow
	// reviewer does not invalidate the rest — not for feedback-loop
	// workflows where a single handler failure is load-bearing.
	PartialReviewOnFailure bool
	// SynchronousFeedback engages the review-consolidator synchronization
	// barrier for parallel review fan-outs ({reviewer, qa} after developer).
	// When true, decideVerdictRendered does NOT emit FeedbackGenerated on a
	// raw fail verdict from any persona listed in ConsolidatedReviewers — the
	// review-consolidator handler joins on those personas' VerdictRendered
	// events for the same developer iteration and emits a single merged
	// FeedbackGenerated, so the developer fires once per round instead of
	// once per reviewer. The aggregate's existing escape hatches (advisory
	// verdict, byte-identical fingerprint, max-iterations) still fire on
	// individual raw verdicts so non-convergent loops escalate immediately
	// rather than waiting for the second reviewer.
	//
	// Verdicts from personas NOT in ConsolidatedReviewers (e.g. quality-gate,
	// committer) still emit FeedbackGenerated through the existing
	// aggregate path — only the reviewer/qa fan-out is gated. Workflows that
	// set this MUST include "review-consolidator" in their Graph as a join
	// over the listed reviewers, otherwise feedback effectively dead-ends.
	// Kill switch: RICK_SYNCHRONOUS_FEEDBACK=0.
	SynchronousFeedback bool
	// ConsolidatedReviewers names the personas whose VerdictRendered events
	// the review-consolidator joins. The aggregate suppresses the per-verdict
	// FeedbackGenerated emission only for these source personas when
	// SynchronousFeedback is true. Order is not significant. Empty when
	// SynchronousFeedback is false.
	ConsolidatedReviewers []string
	// ReviewConsolidator names the handler that joins on ConsolidatedReviewers
	// and emits the merged FeedbackGenerated. When set, the workflow resolver
	// bypasses the pending_feedback join gate FOR THIS HANDLER ONLY — the
	// consolidator is the thing that clears the verdict, so it cannot wait on
	// itself. Downstream consumers (quality-gate, committer) still see the
	// gate via the standard path. Empty when SynchronousFeedback is false.
	ReviewConsolidator string
}

// IsConsolidatedReviewer returns true when the named source persona's verdict
// is joined by the review-consolidator and the aggregate should therefore
// suppress its per-verdict FeedbackGenerated. False when SynchronousFeedback
// is off or the persona is outside the consolidated set (quality-gate,
// committer, etc., whose feedback still flows through the aggregate).
func (d *WorkflowDef) IsConsolidatedReviewer(sourcePersona string) bool {
	if d == nil || !d.SynchronousFeedback || sourcePersona == "" {
		return false
	}
	return slices.Contains(d.ConsolidatedReviewers, sourcePersona)
}

// EffectiveMaxChainDepth returns the chain-depth limit for this workflow.
// When MaxChainDepth is zero (the default), the limit is auto-computed as
// len(Required) + 5, which mirrors the old global AdjustChainDepth formula
// and gives every workflow headroom for the developer→reviewer→qa feedback
// loop on top of its declared phase count.
func (d *WorkflowDef) EffectiveMaxChainDepth() int {
	if d.MaxChainDepth > 0 {
		return d.MaxChainDepth
	}
	return len(d.Required) + 5
}

// PersonasToInvalidateFor returns the set of personas whose completed state
// must be cleared when re-dispatching fromPhase via WorkflowRetried. It is the
// retry-time invalidation primitive — wider than DownstreamOf when fromPhase
// is a member of a synchronization barrier (ConsolidatedReviewers).
//
// The returned set always includes:
//  1. fromPhase itself.
//  2. Every persona DAG-downstream of fromPhase.
//  3. Parallel siblings of fromPhase under the sync-feedback barrier — i.e.
//     when fromPhase ∈ ConsolidatedReviewers, the OTHER ConsolidatedReviewers
//     too, plus each sibling's own downstream set.
//
// (3) is load-bearing under SynchronousFeedback: the review-consolidator joins
// barrier members by dev_trigger_id, so a retry that refreshes only one
// member's verdict produces an unsatisfiable join (one fresh verdict, one
// stale-or-missing). Manifested as the silent wedge on workflow
// 3555adef-bd2b-4210-891e-3ec2a82dba10 (2026-05-15): retry from_phase=reviewer
// left qa's dev_trigger_id unpaired; the consolidator's join.dropped fired
// once and the workflow sat for 1h50m with no recovery.
//
// When SynchronousFeedback is false or fromPhase is outside the consolidated
// set, this method returns exactly DownstreamOf(fromPhase) — no behavioral
// change for non-barrier workflows.
func (d *WorkflowDef) PersonasToInvalidateFor(fromPhase string) []string {
	downstream := d.DownstreamOf(fromPhase)
	if !d.SynchronousFeedback || !slices.Contains(d.ConsolidatedReviewers, fromPhase) {
		return downstream
	}
	// Barrier expansion: every other consolidated reviewer must also be
	// invalidated so the join can re-pair on the new dev_trigger_id, and
	// each sibling's downstream is dragged along for the same reason
	// fromPhase's was. Dedup via map; preserve no particular order — call
	// sites store the result on an event payload and downstream consumers
	// compare via slices.Contains.
	seen := make(map[string]struct{}, len(downstream))
	for _, p := range downstream {
		seen[p] = struct{}{}
	}
	for _, sibling := range d.ConsolidatedReviewers {
		if sibling == fromPhase {
			continue
		}
		for _, p := range d.DownstreamOf(sibling) {
			seen[p] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for p := range seen {
		result = append(result, p)
	}
	return result
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
	}
}

// WorkspaceDevWorkflowDef returns a workflow that provisions a git workspace
// first, then runs the full development pipeline.
//
// Reviewer + qa fire in parallel after the developer and feed into the
// review-consolidator synchronization barrier (SynchronousFeedback: true).
// The consolidator emits a single merged FeedbackGenerated when either
// reviewer or qa fails, so the developer fires exactly once per review
// round instead of once per reviewer. Quality-gate's predecessor is the
// consolidator (not the raw reviewers) so it only runs after the round
// settles to "all pass" — when feedback is needed the consolidator
// clears its own CompletedPersonas via SourcePersona, gating quality-gate
// behind the next round.
func WorkspaceDevWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID:       "workspace-dev",
		Required: []string{"workspace", "context-snapshot", "developer", "quality-gate", "reviewer", "qa", "review-consolidator", "committer"},
		Graph: map[string][]string{
			"workspace":           {},
			"context-snapshot":    {"workspace"},
			"developer":           {"context-snapshot"},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"quality-gate":        {"review-consolidator"},
			"committer":           {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
	}
}

// prCategoryReviewers lists the dedicated single-concern reviewers
// for the pr-review workflow. Order matches the consolidator output.
var prCategoryReviewers = []string{
	"pr-security", "pr-concurrency", "pr-error-handling",
	"pr-observability", "pr-api-contract", "pr-idempotency",
	"pr-testing", "pr-integration", "pr-performance",
	"pr-data", "pr-hygiene", "pr-vendor-resilience",
}

// PRReviewWorkflowDef returns the pr-review workflow definition.
// Flow: pr-workspace → pr-jira-context → N category reviewers (parallel)
// → pr-consolidator (posts consolidated GitHub comment) → pr-cleanup.
func PRReviewWorkflowDef() WorkflowDef {
	required := []string{"pr-workspace", "pr-jira-context"}
	required = append(required, prCategoryReviewers...)
	required = append(required, "pr-consolidator", "pr-cleanup")

	graph := map[string][]string{
		"pr-workspace":    {},
		"pr-jira-context": {"pr-workspace"},
		"pr-consolidator": prCategoryReviewers,
		"pr-cleanup":      {"pr-consolidator"},
	}
	for _, reviewer := range prCategoryReviewers {
		graph[reviewer] = []string{"pr-jira-context"}
	}

	return WorkflowDef{
		ID:            "pr-review",
		Required:      required,
		Graph:         graph,
		MaxIterations: 1,
		// One narrow reviewer's failure (CLI crash, stall, YOLO tool-loop)
		// shouldn't throw away the other eleven. Absorb the failure as a
		// skip, let the rest finish, and let pr-consolidator report what
		// was skipped. See the hulilabs/huli#802 incident (correlation
		// 154ce63a-42d3-41b0-b008-b8c083e538bc, 2026-04-24) where pr-data's
		// claude-exit-1 cancelled three mid-flight gemini siblings via
		// workflow.failed cascade.
		PartialReviewOnFailure: true,
	}
}

// PRFeedbackWorkflowDef returns a workflow for addressing external PR review
// feedback. Workspace provisioning and GitHub PR fetch run in parallel on
// WorkflowStarted; the analyzer joins on both so it has the workspace ready
// and the raw review payload (top-level reviews + inline diff comments + diff
// summary) as ContextEnrichment before triaging. Then context-snapshot
// captures codebase state, developer implements fixes, reviewer+qa validate,
// quality-gate lints/tests, committer pushes.
//
// After the committer, Rick posts a single reply comment via the text-only
// pr-replier persona piped through the non-AI pr-reply-poster handler. The
// committer LLM is barred from running `gh pr comment` directly — see the
// duplicate-post incident on hulilabs/huli#689 (2026-04-17) and the analogous
// pr-consolidator comment.
//
// github-pr-fetcher is always in the Required set even when GITHUB_TOKEN is
// unset — the handler itself short-circuits with an empty enrichment in that
// case (see internal/github/fetcher.go). This keeps the DAG authoritative.
func PRFeedbackWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID: "pr-feedback",
		Required: []string{
			"workspace", "github-pr-fetcher", "feedback-analyzer", "context-snapshot",
			"developer", "reviewer", "qa", "review-consolidator", "quality-gate", "committer",
			"pr-replier", "pr-reply-poster",
		},
		Graph: map[string][]string{
			"github-pr-fetcher":   {},
			"workspace":           {"github-pr-fetcher"},
			"feedback-analyzer":   {"workspace"},
			"context-snapshot":    {"feedback-analyzer"},
			"developer":           {"context-snapshot"},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"quality-gate":        {"review-consolidator"},
			"committer":           {"quality-gate"},
			"pr-replier":          {"committer"},
			"pr-reply-poster":     {"pr-replier"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
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
			"review-consolidator", "quality-gate", "reviewer", "qa", "committer",
		},
		Graph: map[string][]string{
			"jira-context":        {},
			"workspace":           {"jira-context"},
			"context-snapshot":    {"workspace"},
			"researcher":          {"context-snapshot"},
			"architect":           {"researcher"},
			"developer":           {"architect"},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"quality-gate":        {"review-consolidator"},
			"committer":           {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
	}
}

// GithubDevWorkflowDef returns a workflow that reads a GitHub issue, provisions
// a workspace, snapshots the codebase, then runs the full development pipeline.
// Mirrors jira-dev but sources ticket context from GitHub Issues instead of Jira.
func GithubDevWorkflowDef() WorkflowDef {
	return WorkflowDef{
		ID: "github-dev",
		Required: []string{
			"github-context", "workspace", "context-snapshot",
			"researcher", "architect", "developer",
			"review-consolidator", "quality-gate", "reviewer", "qa", "committer",
		},
		Graph: map[string][]string{
			"github-context":      {},
			"workspace":           {"github-context"},
			"context-snapshot":    {"workspace"},
			"researcher":          {"context-snapshot"},
			"architect":           {"researcher"},
			"developer":           {"architect"},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"quality-gate":        {"review-consolidator"},
			"committer":           {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
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
		Required: []string{"workspace", "developer", "review-consolidator", "quality-gate", "reviewer", "qa", "committer"},
		Graph: map[string][]string{
			"workspace":           {},
			"developer":           {"workspace"},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"quality-gate":        {"review-consolidator"},
			"committer":           {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:         2,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
	}
}
