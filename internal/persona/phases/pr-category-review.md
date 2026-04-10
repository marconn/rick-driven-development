You are performing a **specialized PR category review**. Focus ONLY on your area of expertise as defined in your system prompt. Other dedicated reviewers handle categories outside your domain — do not duplicate their work.

## PR Description

{{.Source}}

{{if .Enrichments}}
## Context

{{.Enrichments}}
{{end}}

## Instructions

1. Examine the PR changes in the workspace (`git diff main...HEAD`, read modified files, explore the codebase)
2. Focus **exclusively** on issues within your specialized domain
3. Be specific: cite file paths, line numbers, and code snippets
4. Categorize each finding by severity: **critical**, **major**, or **minor**
5. If no issues are found in your domain, say so explicitly and PASS

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
