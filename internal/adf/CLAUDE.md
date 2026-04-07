# package adf

Converts Markdown text into Atlassian Document Format (ADF) JSON for use with Jira REST API v3 fields (descriptions, comments, custom fields).

## Files
- `convert.go` — goldmark-based Markdown parser, walks AST and emits ADF node maps
- `convert_test.go` — coverage for headings, bold/italic, strike, code, lists, blockquotes, tables, autolinks, rules, JSON-validity round-trip

## Key types
- `FromMarkdown(md string) map[string]any` — sole exported entry point; returns an ADF doc `{version:1, type:"doc", content:[...]}`

## Supported nodes
- Block: `heading` (level 1-6), `paragraph`, `bulletList`/`orderedList` + `listItem`, `blockquote`, `codeBlock` (with `language` attr for fenced), `rule`, `table` (`tableHeader`/`tableCell` rows wrapped in `tableRow`)
- Inline marks: `strong`, `em`, `strike`, `code`, `link` (also via autolink `<url>`)
- `hardBreak` emitted after `ast.Text` nodes flagged `HardLineBreak()`

## Patterns
- Pure functional: no state, no errors returned — malformed Markdown still produces a valid ADF doc
- Returns `map[string]any` rather than typed structs — caller marshals with `encoding/json`
- Marks accumulate down the AST via `renderMarkedChildren` (parent marks copied + new mark appended) so nested emphasis works
- Unknown AST nodes fall back to extracting leaf `*ast.Text` children via `textFromSegments` and emitting a plain text/paragraph node — no panics on unsupported syntax
- Empty paragraphs/blockquotes return `nil` and are dropped from output
- Table cells always contain at least one text node (empty string fallback) to satisfy ADF schema
- Uses `goldmark` with `extension.GFM` — strikethrough, tables, autolinks, task lists come from GFM

## Dependencies
- `github.com/yuin/goldmark` (+ `extension`, `extension/ast`, `parser`, `text`)

## Related
- `../handler/qa_jira_writer.go` — only in-tree consumer; calls `adf.FromMarkdown` to format QA steps before writing them to a Jira custom field
