# package phases

Embedded markdown prompt templates for each persona phase, loaded by `persona.PromptBuilder` via `//go:embed phases/*.md` from the parent package.

## Files
- `research.md` — Research phase: domain analysis, risks, unknowns, constraints. Feeds architect.
- `architect.md` — Architecture phase: approach, file changes, API contracts, sequence diagram, implementation order.
- `develop.md` — Development phase: produce complete code with tests; supports Feedback/PreviousDevelop retry vars.
- `review.md` — Review phase: correctness, quality, security. Emits `VERDICT: PASS|FAIL`.
- `qa.md` — QA phase: testability, error handling, observability, regression. Emits `VERDICT: PASS|FAIL`.
- `commit.md` — Commit phase: fetch/rebase/push/PR via git + gh, message `<Ticket>: <summary>`.
- `feedback-analyze.md` — PR feedback triage: actionable / cosmetic / push-back / questions / priority.
- `feedback-verify.md` — Verifies developer addressed each actionable feedback item. Emits `VERDICT: PASS|FAIL`.
- `qa-analyze.md` — Generates Spanish "Dado/Cuando/Entonces" QA scenarios from Jira+PR context (used by `jira-qa-steps`).

## Phase verbs (template basenames)
- `research`, `architect`, `develop`, `review`, `qa`, `commit`, `feedback-analyze`, `feedback-verify`, `qa-analyze`

## Template variables (resolved by `persona.PromptContext`)
- `.Source` — original prompt / ticket text
- `.Research`, `.Architecture`, `.Develop`, `.FeedbackAnalysis`, `.PreviousDevelop` — prior phase outputs
- `.Codebase`, `.Schema`, `.GitContext`, `.Enrichments` — context injections
- `.Feedback`, `.Ticket`, `.BaseBranch` — retry / commit metadata

## Patterns
- No Go code in this directory — pure `text/template` markdown loaded via `embed.FS` in `../prompt.go`.
- Custom overrides: `PromptBuilder.SetCustomDir(dir)` checks `<dir>/phases/<phase>.md` before falling back to embedded.
- Verdict-emitting phases (`review`, `qa`, `feedback-verify`) MUST end with the literal `VERDICT: PASS` or `VERDICT: FAIL` line — parsed downstream by verdict resolution.
- Phase basename is the `phase` arg passed to `PromptBuilder.Build(phase, ctx)`; the root `PhaseMap` (`internal/engine/workflow_def.go`) maps these verbs to handler names.

## Related
- `..` — parent `persona` package: `prompt.go` (`PromptBuilder`, embed FS, custom dir override), `persona.go`
- `../prompts/` — sibling templates for handler-level prompts (e.g. `committer.md`)
- `../../engine` — `WorkflowDef.PhaseMap` consumer (verb → handler name)
- `../../handler` — handlers implementing the optional `Phased` interface to declare a custom phase verb
