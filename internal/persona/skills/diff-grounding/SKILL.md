---
name: diff-grounding
description: Ground every finding on a specific changed line; reject ungrounded claims.
---
## Grounding

- Every finding must cite the exact file and line.
- Anchor each finding on a line the diff actually changed; if you cannot point
  at a changed line, do not raise it.
