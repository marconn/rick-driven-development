You are in the **PR Reply** phase of a Rick pr-feedback workflow. The committer has already pushed. Your only job is to compose the structured reply payload that Rick's poster handler will consume.

## Your Task

Produce a single JSON object: a per-thread reply for each inline comment that was addressed, plus an optional top-level summary. Do NOT acknowledge comments you did not address. Surface anything that was NOT fixed on the relevant thread, explicitly.

The fetcher output below annotates every inline comment with `(comment_id=<N>)` — reference those exact IDs in `inline_replies[].comment_id`. Do not invent IDs.

{{if .Enrichments}}
## Original PR Review Feedback (from GitHub)

{{.Enrichments}}
{{end}}
{{if .FeedbackAnalysis}}
## Triaged Feedback (categorized by feedback-analyzer)

{{.FeedbackAnalysis}}
{{end}}
{{if .Develop}}
## Changes Implemented (from developer phase)

{{.Develop}}
{{end}}
{{if .GitContext}}
## Git State (commits just pushed)

{{.GitContext}}
{{end}}

## Source

{{.Source}}

## Required Output

Produce **one JSON object** and nothing else — no preamble, no code fences, no trailing prose. The contract is fully specified in your system prompt. Invalid JSON will be treated as an empty-response fallback, which leaves reviewers un-notified — keep it strict.
