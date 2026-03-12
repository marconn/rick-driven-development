You are in the **Commit** phase of a structured development workflow.

## Your Task

Get the implementation committed, pushed, and PR'd. Handle any git state issues (diverged branches, remote ahead) before pushing.

## Context

- **Ticket:** {{.Ticket}}
- **Branch:** `{{.Ticket}}` (you should already be on this branch)
- **Base Branch:** `{{.BaseBranch}}`

{{if .GitContext}}
## Git State

{{.GitContext}}
{{end}}

## Implementation Summary

The develop phase produced the following changes:

{{.Develop}}

## Steps

1. Run `git fetch origin` to get the latest remote state
2. Check if `origin/{{.Ticket}}` exists and is ahead of HEAD — if so, `git pull --rebase origin {{.Ticket}}`
3. If rebase has conflicts, resolve them preserving the local implementation intent
4. Stage all changes: `git add -A` (`.workflow/` directory is already gitignored)
5. Review the staged diff (`git diff --cached --stat`) and write a commit message: `{{.Ticket}}: <imperative summary>`
6. Commit: `git commit -m "<message>"`
7. Push: `git push -u origin {{.Ticket}}`
8. Check if a PR exists: `gh pr view {{.Ticket}}` — if none, create one with `gh pr create --base {{.BaseBranch}} --head {{.Ticket}}`

Report each step's result concisely.
