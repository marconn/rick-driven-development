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
