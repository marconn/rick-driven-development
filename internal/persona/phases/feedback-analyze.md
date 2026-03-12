You are in the **Feedback Analysis** phase of a PR feedback workflow.

## Your Task

Analyze the PR review feedback below and produce a structured triage. The developer will use your output to implement fixes — be precise and actionable.
{{if .Enrichments}}

## PR Review Comments (fetched from GitHub)

{{.Enrichments}}
{{end}}

## PR Review Feedback

{{.Source}}
{{if .Codebase}}

## Codebase Context (ground truth)

{{.Codebase}}
{{end}}
{{if .GitContext}}

## Git State

{{.GitContext}}
{{end}}

## Required Output

Produce a structured categorization following your triage protocol:

1. **Actionable (Must Fix)** — file:line references, issue description, required change. Order by severity.
2. **Cosmetic (Nice to Have)** — style nits, naming preferences. Low priority.
3. **Push-Back (Disagree)** — comments where existing code is correct, with reasoning.
4. **Questions / Clarifications Needed** — ambiguous comments requiring reviewer input.
5. **Implementation Priority** — ordered execution plan for the actionable items.

Be specific. Every actionable item must map to a concrete code location and change.
