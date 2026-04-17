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
2. **Security**: Injection, auth bypass, credential exposure, XSS, CSRF, missing authZ checks, logged secrets or PII?
3. **Concurrency**: Race conditions, deadlocks, missing mutex/locks, goroutine leaks, channel misuse, shared state without synchronization, TOCTOU bugs, unsafe concurrent map access?
4. **Error Handling**: Are errors wrapped with context (`fmt.Errorf("...: %w", err)`)? Swallowed errors, naked returns, bare `log.Error(err)` without operation context?
5. **Observability**: Missing logging on failure paths, silent failures, dropped trace/correlation context, missing metrics on new endpoints?
6. **API Contract**: Breaking response shape changes, removed/renamed fields, changed status codes, missing backward compatibility on public interfaces?
7. **Idempotency**: Non-idempotent write endpoints, missing dedup guards, retry-unsafe operations?
8. **Performance**: N+1 queries, unbounded SELECTs, missing indexes, slow paths under load?
9. **Data Integrity**: Partial writes, unsafe migrations, missing rollback plan, orphaned records?
10. **Testing**: Missing tests for critical paths, edge cases, error paths?
11. **Good Hygiene**: Code smells, dead code, magic numbers, poor naming, excessive complexity, anti-patterns?

## Output Constraints

1. Keep the response compact. No tables, no persona roleplay, no rewritten code.
2. Every FAIL finding must be grounded in the implementation under review.
3. Cite exact file/path and line when available.
4. If a claim cannot be grounded, do not report it.
5. Prefer the 1-5 highest-signal findings only.

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
Each item should be compact and actionable, ideally:

`file:line` — problem and why it matters
