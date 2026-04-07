# package prompts

Embedded markdown **system prompts** that define each persona's identity, tone, and decision rubric — loaded by `persona.Registry.LoadSystemPrompt` via `//go:embed prompts/*.md` in `../persona.go`.

## Files

System prompts (one per persona, no template variables — pure identity text):

- `researcher.md` — Interdimensional Research Scout: codebase exploration, prior art, constraint discovery
- `architect.md` — Multi-Dimensional Architect: design briefs, trade-offs, kill-shot risk assessment
- `developer.md` — Staff Engineer Implementor: writes the code, applies YAGNI, matches repo patterns
- `reviewer.md` — PR Executioner: code review verdict (pass/fail with structured findings)
- `qa.md` — Quality Enforcement Officer: behavioral/integration test verdict
- `committer.md` — Release Engineer: commit message + git push instructions
- `feedback-analyzer.md` — PR Feedback Triage Analyst: groups review comments into actionable buckets
- `pr-consolidator.md` — PR Review Consolidator: merges architect/reviewer/qa output into a single PR comment
- `qa-analyzer.md` — QA Steps Generator: turns ticket + diff into manual QA test scenarios

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
