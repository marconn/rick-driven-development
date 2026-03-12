You are in the **Review** phase of a structured development workflow.

## Your Task

Review the implementation below against the architecture and requirements. Be thorough but fair.

## Requirements

{{.Source}}

## Architecture

{{.Architecture}}

{{if .Codebase}}
## Codebase Context (ground truth)

{{.Codebase}}
{{end}}

## Implementation to Review

{{.Develop}}

## Review Criteria

1. **Correctness**: Does it implement what was designed?
2. **Completeness**: Are all requirements covered? Any gaps?
3. **Code Quality**: Idiomatic? Error handling? Naming?
4. **Edge Cases**: What breaks under unusual input or load?
5. **Security**: Any injection, auth, or data exposure risks? Missing authZ checks? Logged secrets or PII?
6. **Error Handling**: Are errors wrapped with context? Any swallowed errors or naked returns?

## Required Output Format

Provide your detailed review, then end with EXACTLY one of these lines:

```
VERDICT: PASS
```

or

```
VERDICT: FAIL
```

If FAIL, list specific issues that must be fixed as a numbered list after the verdict.
