You are performing a **specialized PR category review**. Focus ONLY on your area of expertise as defined in your system prompt. Other dedicated reviewers handle categories outside your domain — do not duplicate their work.

## PR Description

{{.Source}}

{{if .Enrichments}}
## Context

{{.Enrichments}}
{{end}}

## Instructions

1. Review **ONLY the code changes shown in the PR diff above**. Do NOT review code outside the diff — other files in the repository are out of scope even if you can access them.
2. If the enrichments above include a "PR Changed Files" list, those are the ONLY files in scope. If a file is not in that list, do not flag issues in it.
3. You may read unchanged surrounding code for understanding context, but only flag issues in lines that were added or modified in this PR.
4. Focus **exclusively** on issues within your specialized domain.
5. Every FAIL finding must be grounded in the diff itself. Cite the exact changed file, the exact changed line number, and quote the exact changed token/snippet in backticks.
6. Categorize each finding by severity: **critical**, **major**, or **minor**.
7. If you cannot point to an exact changed line and exact changed snippet, do NOT report the issue.
8. If no issues are found in your domain within the changed code, say so explicitly and PASS.

## Output Constraints

- Keep the response compact. Do not write long analysis sections, plans, or repeated explanations.
- Do not invent snippets, commands, or files that are not present in the diff.
- Do not speculate about hidden files, local workspace state, or "likely" code outside the patch.
- Prefer 0-3 findings. If you have more, report only the highest-signal grounded ones.

## Required Output Format

Write at most one short paragraph, then end with EXACTLY one of:

```
VERDICT: PASS
```

or

```
VERDICT: FAIL
```

If FAIL, list specific grounded issues as a numbered list after the verdict.
Each numbered item must follow this shape:

`severity` `file:line` `snippet` — why this is a problem in your domain
