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

1. **Test Coverage**: Are critical paths tested? Are edge cases covered?
2. **Error Handling**: Do errors propagate correctly? Are they actionable?
3. **Data Integrity**: Race conditions? Partial writes? Idempotency? Are data migrations safe, tested, and reversible?
4. **Observability**: Can you debug this at 3 AM with logs alone?
5. **Regression Risk**: Could this break existing functionality?
6. **Performance**: Any unbounded queries, N+1 patterns, or missing indexes that will degrade under load?

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
