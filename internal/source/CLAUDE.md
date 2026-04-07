# package source

Resolves a workflow input string into a typed `Source` (raw prompt, local file, Jira key, or GitHub issue) so callers get uniform `{Type, Reference, Content}` regardless of where the prompt came from.

## Files
- `source.go` — `SourceType` constants, `Source` struct, `Resolver` with prefix-dispatch `Resolve`, plus `resolveFile` / `resolveJira` / `resolveGitHub` helpers
- `source_test.go` — table tests per scheme; the live `gh:owner/repo#N` test skips when `gh` CLI is missing

## Schemes supported
- `file:<path>` — reads the file from disk via `os.ReadFile`; empty path returns an error
- `jira:<KEY>` — stores the key in `Reference`; `Content` is set to `"jira:<KEY>"` (no Jira API call here — actual ticket fetch happens in handlers like `jira-context`)
- `gh:owner/repo#N` — shells out to `gh issue view N --repo owner/repo --json title,body` and formats `Content` as `"GitHub Issue: <title>\n\n<body>"`
- `gh:owner/repo` (no `#`) — short-circuits without calling `gh`; returns `Type=GitHub`, `Content=ref`
- anything else — `Type=Raw`, `Content=input`, empty `Reference`
- NOTE: this package does NOT understand `confluence:` or `gh:owner/repo#N` as a PR — PR sources are parsed inside the relevant handlers, and Confluence resolution lives in the confluence MCP/handlers

## Key types
- `SourceType` — string enum: `Raw`, `File`, `Jira`, `GitHub`
- `Source{Type, Reference, Content}` — `Reference` is the parsed identifier (path / key / `owner/repo#N`), `Content` is the resolved payload fed to the LLM
- `Resolver` — empty struct; constructed via `NewResolver()`; only method is `Resolve(ctx, input string) (*Source, error)` (ctx is currently unused)

## Patterns
- Prefix dispatch via `strings.HasPrefix` in `Resolve`; default branch is `Raw`
- Errors wrapped with `fmt.Errorf("source: ...: %w", err)` per repo convention
- `gh` shell-out uses `exec.Command(...).Output()` — failure surfaces as a wrapped error, not a fallback
- Stateless resolver — safe to share; no I/O at construction

## Related
- `../mcp` — `rick_run_workflow` and friends accept the source string that ends up here
- `../handler` — `jira-context`, `pr-jira-context`, `pr-workspace`, `confluence-reader` etc. do the heavy fetching once a `Source` has been classified
- `../engine` — tags `source` on `WorkflowRequested` so `store.LoadByTag` can find workflows by their resolved reference
