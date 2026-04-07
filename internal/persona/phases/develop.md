{{if .WorkspacePath}}
## Workspace Constraints — READ THIS FIRST

You are running inside an **isolated Rick workspace**. A marker file
`.rick/workspace.yaml` exists in your current directory to confirm this.

- **Current working directory:** `{{.WorkspacePath}}`
- **Active branch:** `{{.Ticket}}` (already checked out, branched from `{{.BaseBranch}}`)
- **Remote:** same `origin` as the source repo — pushes land on GitHub normally

### Hard rules — violations will corrupt the operator's main checkout

1. **Do ALL work in your current working directory.** Read, write, edit, and run commands from here. Never use absolute paths that point outside `{{.WorkspacePath}}`.
2. **Do NOT `cd` out of this directory.** Not to the parent, not to any sibling clone of this repo, not anywhere. If you need to navigate, use paths relative to cwd.
3. **The branch is already created and checked out.** Do not run `git checkout -b {{.Ticket}}` or switch to `main`. Verify with `git status` if uncertain.
4. **Commits belong in this workspace only.** Never navigate elsewhere to commit, even if another clone looks more convenient.
5. **Safety check:** if `pwd` ever returns anything other than `{{.WorkspacePath}}`, STOP IMMEDIATELY and report an error. Do not continue.

If the task text references the repo by name (e.g., "work on hulihealth-web"), that name refers to **this workspace** — not any other clone on disk.

---

{{end}}
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
