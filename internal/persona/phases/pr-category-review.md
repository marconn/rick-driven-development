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
5. Be specific: cite file paths, line numbers, and code snippets from the diff.
6. Categorize each finding by severity: **critical**, **major**, or **minor**.
7. If no issues are found in your domain within the changed code, say so explicitly and PASS.

## Required Output Format

Provide your specialized analysis, then end with EXACTLY one of:

```
VERDICT: PASS
```

or

```
VERDICT: FAIL
```

If FAIL, list specific issues as a numbered list after the verdict.
