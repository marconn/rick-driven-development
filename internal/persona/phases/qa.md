You are in the **QA** phase of a structured development workflow.

## Your Task

Validate the implementation from a quality assurance perspective. Focus on testability, reliability, and production-readiness.

## Requirements

{{.Source}}

## Architecture

{{.Architecture}}

## Implementation

{{.Develop}}

## QA Criteria

1. **Test Coverage**: Are critical paths tested? Edge cases? Error paths? Integration boundaries?
2. **Security**: Injection vectors, auth bypass, credential exposure, input validation gaps?
3. **Concurrency**: Race conditions, deadlocks, goroutine leaks, shared state without synchronization, unsafe concurrent map access?
4. **Error Handling**: Do errors propagate correctly with context? Swallowed errors? Silent failures?
5. **Observability**: Can you debug this at 3 AM with logs alone? Missing correlation context? Dropped traces?
6. **API Contract**: Breaking changes to response shapes, removed fields, changed status codes?
7. **Idempotency**: Are write operations safe to retry? Missing dedup guards?
8. **Data Integrity**: Partial writes? Unsafe migrations? Missing rollback plan? Orphaned records?
9. **Performance**: Unbounded queries, N+1 patterns, missing indexes, latency regressions under load?
10. **Integration**: Contract tests present? E2E coverage for critical paths?
11. **Good Hygiene**: Code smells, dead code, magic numbers, poor naming, anti-patterns, excessive complexity?

## Required Output Format

Provide your detailed QA analysis, then end with EXACTLY one of these lines:

```
VERDICT: PASS
```

or

```
VERDICT: FAIL
```

If FAIL, list specific quality issues that must be addressed as a numbered list after the verdict.
