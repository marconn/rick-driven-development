# package workspace

Provisions isolated git working copies under `$RICK_REPOS_PATH` so workflows (`workspace-dev`, `jira-dev`, `pr-review`, `ci-fix`) can develop on a clean branch without disturbing the operator's checkout.

## Files
- `workspace.go` — `SetupWorkspace`, `CleanupIsolatedWorkspace`, `WorkspaceResult`, internal `runGit` / `resolveBasePath` helpers
- `workspace_test.go` — table-driven tests over a self-hosted git repo (also defines a small `validateWorkspaceParams` helper used only by tests)

## Key types / functions
- `WorkspaceResult{Path, Branch, Base, Isolated}` — return shape from `SetupWorkspace`
- `SetupWorkspace(repo, ticket, branch, base, suffix string, isolate bool) (*WorkspaceResult, error)` — single entry point that:
  - resolves `$RICK_REPOS_PATH/<repo>` (falls back to the basename if `repo` is `owner/name` and the owner segment is already part of `RICK_REPOS_PATH`)
  - if `isolate=true`, removes any stale destination then `cp -r` the source repo to the destination
  - `git fetch origin`, then `reset --hard HEAD` + `clean -fd` to guarantee a clean tree
  - if `branch != ""`, checks out that existing remote branch (PR / ci-fix path); otherwise creates a new branch named after `ticket` from `origin/<base>`
  - falls back to `checkout` + `reset --hard origin/<branch-or-base>` when the local branch already exists from a prior run
  - removes `.claude/settings.json` and marks it `assume-unchanged` so repo-level permission rules don't block backend tools
  - on any error after the copy step, removes the isolated destination (deferred cleanup)
- `CleanupIsolatedWorkspace(path string)` — best-effort `os.RemoveAll`, errors swallowed (workspaces are expendable)
- `runGit(dir, args...)` / `resolveBasePath()` — package-internal helpers; `runGit` wraps errors with the failing command + combined stderr

## Naming convention
- Isolated destination: `<repo>-<ticket>[-<suffix>]` joined with `$RICK_REPOS_PATH`
  - `repo` — the repo directory name (post owner-strip fallback)
  - `ticket` — the Jira ticket or workflow identifier; becomes the branch name when no explicit `branch` is given
  - `suffix` — optional disambiguator; callers pass the workflow correlation ID here to prevent collisions when multiple workflows touch the same ticket
- `branch` parameter overrides the branch name only — it does NOT affect the destination directory name; ticket still drives the directory

## Safety guards
- Required-arg checks: `repo` must be set; at least one of `ticket` / `branch` must be set
- Hard fail when `RICK_REPOS_PATH` is unset, when the source repo lacks `.git`, or when git operations fail (errors carry the git command + stderr)
- Stale destination removed before `cp -r` so a half-written previous run can never leak files into the new workspace
- Deferred cleanup of the isolated copy on any post-copy error so failed setups don't leave orphan directories
- `reset --hard` + `clean -fd` ensure dirty trees never block branch creation in the in-place (non-isolated) mode
- This package does NOT validate destination patterns before deletion — callers (`internal/mcp/tools_workspace.go`) own the `*-rick-ws-*` / suffix-based pattern guard before invoking `CleanupIsolatedWorkspace`

## Env vars
- `RICK_REPOS_PATH` — required; root directory holding source repos and isolated workspace clones

## Related
- `../mcp/tools_workspace.go` — `rick_workspace_setup` / `rick_workspace_cleanup` / `rick_workspace_list` MCP tools wrap this package and own the deletion pattern guard
- `../handler/workspace.go` — `workspace` handler used by `workspace-dev` and `jira-dev` DAGs; calls `SetupWorkspace` with `isolate=true` and the correlation ID as `suffix`
- `../handler/pr_workspace.go` — `pr-workspace` handler for the `pr-review` DAG; clones the PR branch via the `branch` override path
- `../handler/pr_cleanup.go` — `pr-cleanup` handler that calls `CleanupIsolatedWorkspace` at the end of `pr-review`
- `../github` — PR metadata fetching used alongside `pr-workspace` to resolve the branch name passed to `SetupWorkspace`
