# package confluence

Thin REST client for Confluence Cloud — reads pages, updates page sections by heading, and provides HTML helpers used by the BTU planning workflow and `rick_confluence_*` MCP tools.

## Files
- `client.go` — `Client` struct, `Page` model, `ReadPage`/`UpdatePageSection`, and HTML utilities (`SplitAtHeading`, `NormalizeEntities`, `ExtractTextContent`).
- `client_test.go` — unit tests for the HTML helpers and section-splitting logic.

## Key types
- `Client` — wraps `*http.Client` with base URL + basic-auth credentials. Constructed via `NewClient(baseURL, email, token)` or `NewClientFromEnv()` (returns `nil` when required env vars are missing).
- `Page` — `{ID, Title, Body, Version, SpaceKey}`. `Body` is Confluence HTML "storage format".

## Key functions
- `ReadPage(ctx, pageID)` — `GET /rest/api/content/{id}?expand=body.storage,version,space`. Returns `*Page` or wrapped error with HTTP status + body.
- `UpdatePageSection(ctx, page, heading, newContent)` — finds the heading, replaces everything between it and the next same-level heading (or EOF) with `newContent`, bumps version, then PUTs the full page back.
- `SplitAtHeading(html, headingText)` — case-insensitive scan across `<h1>`/`<h2>`/`<h3>`, returns `(before, section, after, found)`. Maps positions from normalized lower-case back to the original HTML so the rewrite preserves casing and entities.
- `NormalizeEntities(s)` — replaces common HTML entities (Spanish accents `&eacute;` etc., plus `&amp;`/`&lt;`/`&gt;`/`&quot;`/`&nbsp;`) so heading matching works against accented text like "🛠️ Plan Técnico".
- `ExtractTextContent(html)` — naive tag stripper, returns trimmed plain text.

## Patterns
- **Auth**: HTTP Basic with email + API token via `req.SetBasicAuth`. Set on every request through `setAuth`.
- **Env vars**: `CONFLUENCE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN`. `NewClientFromEnv` falls back to `JIRA_EMAIL`/`JIRA_TOKEN` when the Confluence-specific vars are unset, since Atlassian Cloud uses the same credential pair.
- **Storage format**: Confluence uses an XHTML "storage" representation, not ADF. Updates round-trip the full body — callers must preserve untouched markup.
- **Optimistic versioning**: `updatePage` sends `version.number = page.Version + 1`; concurrent edits will fail at the API.
- **Errors**: All wrapped with `confluence:` prefix and include HTTP status + response body for debugging.
- **No retries / no rate limiting**: Caller is responsible. The `httpClient` is a bare `&http.Client{}` with default timeouts.

## Related
- `../mcp/tools_confluence.go` — `rick_confluence_read` / `rick_confluence_write` MCP tools that wrap this client.
- `../planning` — BTU planning workflow's `confluence-reader` and `confluence-writer` handlers consume this client; the writer uses `UpdatePageSection` to inject the generated plan after the "🛠️ Plan Técnico" heading.
- Root `/CLAUDE.md` — `plan-btu` and `plan-jira` workflow descriptions and env-var requirements.
