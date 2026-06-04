# Architectural Code Review: Persona Extensibility & Dispatch Stability

**Target Document:** `docs/persona-extensibility-and-dispatch-redesign.md`

## 1. What was done well
The proposal is exceptionally structured. Deliberately decoupling the three tracks (Persona Registry, Dispatch projection, Telemetry) to avoid "Scope Fusion" is the exact right architectural move. The shadow-mode rollout strategy for the projection (via the `JoinDivergence` event) and the principled rejection of the Claude Agent SDK (preserving multi-backend constraints and our single Go binary) demonstrate strong operational maturity.

---

## 2. Review across 5 Dimensions

### A. Plan Alignment
- The design perfectly targets the original problems (boilerplate reduction, silent wedges, knowledge extension) without violating our core constraints (maintaining $p99 < 200ms$ via O(1) reads, multi-backend support, choreographic discipline).
- Using capability negotiation (`backend.Capabilities`) cleanly models the reality of vendor divergence without breaking the unified `backend.Request` boundary.

### B. Design Quality
- Exposing explicit states (`Pending`, `Ready`, `Running`, `Stale`) directly resolves the opaqueness of the legacy `checkJoinCondition`.
- The addition of the `StaleSince` field explicitly answers the problem of silent stranding, allowing for proper recoverability.

### C. Rollout & Safety (Coverage Equivalent)
- Telemetry-first routing (deferring the eager-inline policy until `knowledge_unavailable` metrics are gathered) is an excellent data-driven approach.
- Shadow rollout for Track B effectively mitigates the system's highest risk factor on the hot path.

### D. Architecture & Design
- Moving from O(N) full-chain recalculation to an O(1) materialized read model (`WorkflowRuntimeState`) is the optimal architectural pattern for this choreography.
- **Risk:** Eventual consistency dependencies. Dispatch will now entirely trust a single projection. If that projection's updater stalls, the system fails silently.

### E. Documentation
- The document is concise. The "Kill Shot" analysis is honest, and the Mermaid diagrams effectively convey the sequence and state boundaries.

---

## 3. Reported Issues & Required Fixes

### 🔴 Critical (Must fix before proceeding)
- **Projection Poison Pill Handling:** You are shifting the single point of failure to the `WorkflowRuntimeState` event applicator. If a malformed event (poison pill) or SQLite lock contention causes the projection updater to panic or stall, the O(1) read will be indefinitely stale. The correlation will strand, and it will bypass your new `StaleSince` timeouts because the projection itself has stopped updating. 
  **Fix:** The design must explicitly define a quarantine/skip mechanism for bad events in the projection updater.

### 🟡 Important (Should fix)
- **Strict Idempotency:** The proposal must mandate that the projection's event handlers are strictly idempotent. If the event store replays a batch (e.g., due to an unexpected restart), counters like `FeedbackCount` or state transitions must not drift.

### 🟢 Suggestion (Nice to have)
- **Schema Validation:** Define a `schema_version` for the `SKILL.md` YAML frontmatter from day one, and ensure the startup linter enforces it strictly to prevent parser drifts across environments.
- **SLA Threshold:** Explicitly state that the `StaleSince` timeout threshold will be derived statistically from the baseline `dispatch_dwell_seconds` telemetry, rather than guessed.
