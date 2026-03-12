You are in the **Development** phase of a structured development workflow.

## Your Task

Produce a complete, structured implementation based on the architecture below. Your output will be reviewed — make it solid.

## Requirements

{{.Source}}

{{if .Architecture}}
## Architecture

{{.Architecture}}
{{end}}
{{if .Codebase}}

## Codebase Context (ground truth)

{{.Codebase}}
{{end}}
{{if .Schema}}

## Schema Definitions

{{.Schema}}
{{end}}
{{if .GitContext}}

## Git State

{{.GitContext}}
{{end}}

{{if .FeedbackAnalysis}}
## PR Feedback Analysis

The following analysis categorizes the PR review comments. Address ALL actionable items:

{{.FeedbackAnalysis}}
{{end}}

{{if .Enrichments}}
## Enrichment Context (from external systems)

The following libraries, components, or patterns have been recommended by external analysis systems. Use them where appropriate — they've been validated against the architecture plan.

{{.Enrichments}}
{{end}}

{{if .Feedback}}
## Feedback from Previous Iteration

The following issues were identified in your previous implementation. Address ALL of them:

{{.Feedback}}

## Previous Implementation

{{.PreviousDevelop}}
{{end}}

## Expected Output

For each file change, produce:

1. **File path** and action (create/modify)
2. **Complete code** — no TODOs, no ellipsis, no placeholders
3. **Brief rationale** for non-obvious decisions
4. **Automated tests** — unit or integration tests that validate the change

End your response with a **Manual Verification Checklist**: numbered steps to confirm the implementation works beyond what automated tests cover.

Produce changes in dependency order: schemas → interfaces → implementations → tests.
