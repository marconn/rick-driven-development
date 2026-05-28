# package prompts

Embedded markdown **system prompts** that define each persona's identity, tone, and decision rubric — loaded by `persona.Registry.LoadSystemPrompt` via `//go:embed prompts/*.md` in `../persona.go`.

## Files

System prompts (one per persona, no template variables — pure identity text):

- `researcher.md` — requirements and codebase researcher: extracts domain model, risks, unknowns, constraints
- `architect.md` — Multi-Dimensional Architect: turns research into an executable design
- `developer.md` — Staff Engineer Implementor: delivers minimal correct implementation aligned with repo patterns
- `reviewer.md` — implementation reviewer: reports grounded high-signal defects only
- `qa.md` — QA reviewer: evaluates release confidence, coverage, and validation gaps
- `committer.md` — Release Engineer: safe git/push/PR execution
- `feedback-analyzer.md` — PR Feedback Triage Analyst: categorizes review comments into actionable buckets
- `pr-consolidator.md` — PR Review Consolidator: merges category-review output into one PR comment
- `pr-<category>.md` (13 files: `pr-security`, `pr-concurrency`, `pr-error-handling`, `pr-observability`, `pr-api-contract`, `pr-idempotency`, `pr-testing`, `pr-integration`, `pr-performance`, `pr-data`, `pr-hygiene`, `pr-vendor-resilience`, `pr-docs-concordance`) — narrow single-concern PR reviewers with explicit `## Your Domain (ONLY these)` scope and cross-persona boundary rules. `pr-vendor-resilience` is polyglot (Go / JS-TS / PHP / network vendors) and self-scopes from the diff. `pr-docs-concordance` owns doc/comment-vs-code drift introduced by the diff (existing docs that now lie), distinct from `pr-hygiene` which owns *missing* docs and dead code.
- `pr-replier.md` — text-only composer for PR-feedback reply posts (runs with Yolo=false)
- `qa-analyzer.md` — QA scenario generator: turns ticket + diff into manual QA scenarios in Spanish

## Template variables

None. These are **system prompts** (persona role definition) loaded as raw strings. The per-phase **user prompts** with `{{.Source}}`, `{{.Codebase}}`, `{{.Feedback}}` etc. live in the sibling `../phases/` directory and are rendered by `PromptBuilder.Build` in `../prompt.go`.

## Adding a new prompt

- Drop `<persona-name>.md` in this directory — the `//go:embed prompts/*.md` glob in `../persona.go` picks it up automatically
- Register the persona in `DefaultRegistry()` (`../persona.go`) with the matching `Name` constant
- File basename must equal the persona name passed to `LoadSystemPrompt(name)` — no extension, no path
- For per-phase user prompts (with template variables), add to `../phases/` instead

## Override mechanism

Operators can shadow any embedded prompt by setting `Registry.SetCustomDir(dir)` and dropping `<dir>/<persona>.md` — the loader checks the custom dir first, falls back to embedded.

## Related

- `../persona.go` — `Registry`, `LoadSystemPrompt`, `//go:embed` declaration, persona name constants
- `../phases/` — per-phase user prompt templates (`{{.Source}}`, `{{.Feedback}}`, etc.) rendered by `PromptBuilder`
- `../prompt.go` — `PromptBuilder` that combines system prompts (here) + phase templates + event-store context
