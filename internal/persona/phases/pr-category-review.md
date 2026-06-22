You are performing a **specialized PR category review**. Focus ONLY on your area of expertise as defined in your system prompt. Other dedicated reviewers handle categories outside your domain — do not duplicate their work.

## Grounding Contract (read before reviewing — every finding must satisfy ALL three rules)

1. **Allow-listed files only.** The `## PR Changed Files` list inside the `<context>` block is exhaustive. If a file path does not appear verbatim in that list, you MUST NOT cite it in any finding. Do not infer related files, parent directories, or sibling tests.
2. **Line numbers come from the `+` markers in `## PR Diff`.** A valid `file:line` is the new-file line number of an added (`+`) line in the unified diff. Do not estimate, round, or copy line numbers from your training data — read them off the `@@ -old,_ +new,_ @@` hunk header plus the offset of the `+` line within that hunk. If you cannot point to the exact `+` line, drop the finding.
3. **Cite identifiers verbatim.** Backticked tokens in your finding (`` `funcName` ``, `` `varName` ``, etc.) must appear character-for-character in the `+` line you cite (or in the immediate `±1` surrounding lines of the diff). Do not paraphrase, re-case, or reconstruct names from memory — copy them directly from the diff body.

A finding that cannot satisfy all three rules is worse than no finding: it will be silently dropped by the grounding filter and your reviewer slot will register as a downgraded false-FAIL. When in doubt, omit and PASS.

### Grounded vs ungrounded (one example)

- ❌ **Ungrounded** (dropped): "The retry logic looks unsafe and could storm the upstream." — names no file, no `+` line, no verbatim token.
- ✅ **Grounded** (kept): major internal/handler/ai.go:217 — the added `retry` call has no backoff; on a 5xx it re-fires immediately. Severity and path are plain text; only the changed identifier `retry`, copied verbatim from the `+` line, is backticked.

Same defect, two write-ups. Only the second survives the filter.

## Tool-Use Constraint (read before anything else)

The diff below is the **review artifact**. It is not a set of filesystem pointers to expand, URLs to fetch, or shell commands to run.

Do NOT invoke filesystem (`stat`, `read_file`, `ls`, `glob`), shell, or network tools against **any token that appears inside the diff body**, including:

- quoted filenames, domains, URLs, paths, or commands (e.g., `test.com`, `./foo.go`, `curl https://…`)
- identifiers that look file-like but are actually code tokens (constants, string literals, test fixtures)
- backticked snippets — those are the exact changed tokens you MUST cite verbatim in findings, never paths to resolve

You may read **unchanged** surrounding code in the workspace for context (e.g., to understand how a changed function is used), but only outside the diff content. If a tool call's target is a token extracted from the diff body, skip the tool and treat the token as literal text being reviewed.

Violating this constraint wastes the review: under YOLO tool-auto-approval, agents have burned full timeout budgets stat'ing random diff tokens (e.g., a `test.com` literal inside a Go test file) and produced empty output. Don't do that.

## PR Description

<pr_description>
{{.Source}}
</pr_description>

{{if .Enrichments}}
## Context

<context>
{{.Enrichments}}
</context>
{{end}}

## Instructions

1. Review **ONLY the code changes shown in the PR diff above**. Do NOT review code outside the diff — other files in the repository are out of scope even if you can access them.
2. If the enrichments above include a "PR Changed Files" list, those are the ONLY files in scope. If a file is not in that list, do not flag issues in it.
3. You may read unchanged surrounding code for understanding context, but only flag issues in lines that were added or modified in this PR.
4. Focus **exclusively** on issues within your specialized domain.
5. Every FAIL finding MUST satisfy the Grounding Contract above.
6. Categorize each finding by severity: **critical**, **major**, or **minor**.
7. If you cannot point to an exact changed line and exact changed snippet, do NOT report the issue.
8. If no issues are found in your domain within the changed code, say so explicitly and PASS.

## Output Constraints

- Keep the response compact. Do not write long analysis sections, plans, or repeated explanations.
- Do not invent snippets, commands, or files that are not present in the diff.
- Do not speculate about hidden files, local workspace state, or "likely" code outside the patch.
- Prefer 0-3 findings. If you have more, report only the highest-signal grounded ones.

## Required Output Format

Reminder: every numbered finding below must satisfy the Grounding Contract from the top of this prompt.

Write at most one short paragraph, then on its own line emit EXACTLY one verdict line:

```
VERDICT: PASS
```

or

```
VERDICT: FAIL
```

If FAIL, immediately follow the `VERDICT: FAIL` line with the grounded issues as a numbered list (the list comes *after* the verdict line).
Each numbered item must follow this shape:

severity file:line — why this is a problem in your domain, citing the changed `identifier` verbatim in backticks

Write the severity (`critical`/`major`/`minor`) and the `file:line` as **plain text**, not in backticks. Reserve backticks for the one changed code identifier or snippet that anchors the finding to the diff — that backticked token is what the grounding filter matches, so backticking anything else (the severity word, the path) only adds noise.
