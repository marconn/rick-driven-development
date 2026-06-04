# Pre-Implementation Review Findings

Two independent CLI reviewers (codex-cli, antigravity) reviewed the Spec, plan,
and tasks against the actual code. Every finding below was **re-verified against
the source** before acceptance (file:line in the table). This document records
the findings and how each is resolved in the docs/tasks.

## Critical

| # | Finding | Verified | Resolution |
|---|---|---|---|
| F1 | Projection is **not** a pure fold over existing events — `WorkflowStartedPayload` has only `Phases []string`, not Graph/`RetriggeredBy`/`PartialReviewOnFailure`/consolidator config. | `internal/event/payload.go` (`WorkflowStartedPayload`) | **Persist a WorkflowDef/admission snapshot** at `WorkflowStarted` (follow the `WorkflowRetried` precedent that bakes `InvalidatedPersonas` into the payload). Spec 3.5.1 corrected; task 0010 updated. The earlier "no new durable events" pushback is withdrawn. |
| F2 | `WorkflowRetried` missing from the fold; its `InvalidatedPersonas` is durable readiness state. | `internal/engine/aggregate.go` (`case WorkflowRetried`) | Add `WorkflowRetried` to 0010's input set; add a barrier-sibling-retry test. |
| F3 | `0003` consumes `DispatchDropped`, which is **never published on the bus** (separate aggregate). | `internal/engine/persona_runner.go` (DispatchDropped persist comment) | 0003 reads the diagnostic **aggregate via the store** (or a store tailer), not a bus projection. Clock-start from `WorkflowStarted`/prior completion (also fixes the first-predecessor-wait blind spot). |
| F4 | Backend attribution + `required`-knowledge pin need the selected backend **before** `Run`; selection happens inside `RoundRobin.Run`; `Response` has no backend field. | `internal/handler/ai.go`, `internal/backend/round_robin.go` | Add an explicit `Select(ctx) Backend` / capability-filtered selection API. 0001 attributes from it (or only on `AIResponseReceived`); 0008 depends on it. |
| F5 | `0002` intersection-capabilities contradict `0008` mixed-rotation pinning (`[codex,opencode,claude]` → no MCP though claude present). | `internal/backend/factory.go` (default rotation) | 0002 exposes **per-candidate** capabilities + filtered selection, not just an aggregate intersection. |
| F6 | Shadow compare in `wrap` races the projection apply (typed subscribers fire before all-subscribers). | `internal/eventbus/channel.go`, `internal/projection/projection.go` | Compute divergence **inside the projection apply** (after watermark), not in the dispatch hot path. 0011 updated. |
| F7 | `0013` deletes legacy, so rollback is "revert PR" — contradicts blanket "seconds-fast rollback." | doc contradiction | Keep legacy **dormant** one extra release; scope the seconds-fast guarantee to pre-retirement. README + 0013 updated. |

## Important

| # | Finding | Verified | Resolution |
|---|---|---|---|
| F8 | Track A boundary not clean: `PRConsolidator` calls `LoadSystemPrompt` directly + pins its own backend. | `internal/handler/pr_consolidator.go` | Route persona resolution through a **shared resolver path** used by both `AIHandler` and `PRConsolidator`, or explicitly scope `pr-consolidator` out of manifests in Phase 2. 0007 updated. |
| F9 | `0008` assumes `MCPConfig` is already passed; `AIHandler.Run` does not set it. | `internal/handler/ai.go` | 0008 must **thread** resolver output into `backend.Request.MCPConfig` (define ownership/merge). |
| F10 | `0002` misses the `limitedBackend` wrapper. | `internal/backend/limited.go` | 0002 adds delegation for **all** wrappers (limited, round-robin) + wrapper tests. |
| F11 | `0014` can't add a new participant (registry rejects duplicate names; no DAG node). | `internal/handler/registry.go` | Rescope 0014 to "replace an existing binding"; a genuinely new participant needs **workflow-topology manifests** (L3, out of scope). |
| F12 | Partial-review semantics inconsistent: resolver any-failed-satisfied vs aggregate category-only. | `internal/engine/workflow_resolver.go` vs `aggregate.go` | **Define the intended contract first** (separate clarification task); projection must encode the decided behavior, not inherit the ambiguity. Add non-category failure tests. |
| F13 | `IsReady` must take `requestingHandler` (consolidator bypass is reader-relative). | `internal/engine/workflow_resolver.go` (`isConsolidatorBypass`) | 0010 signature `IsReady(requestingHandler, target)`; store raw verdict state, apply bypass at read time. |
| F14 | Poison-pill quarantine inside `Handle` insufficient (`catchUp` can fail first). | `internal/projection/projection.go` | Quarantine at the **runner/store boundary**, not only inside the projector. 0010 updated. |
| F15 | Phase 2d rollback breaks once embedded prompts are gutted. | design logic | Keep embedded `pr-*.md` **fully intact** through Phase 2; remove boilerplate only in a later cleanup after manifests are locked. 0009 updated. |

## Minor

| # | Finding | Resolution |
|---|---|---|
| F16 | `DispatchStarted` adds a synchronous store write to the dispatch hot path. | Emit best-effort/async, or only for non-AI handlers; measure. 0004 noted. |
| F17 | Projections also registered in `mcp.go`, not just `serve.go`. | 0003/0011 register in **all** active entrypoints (`serve.go` + `mcp.go`). |

## Net effect

The reviews did not change the strategy (three decoupled tracks, telemetry-gated
projection, manifest personas). They hardened it: Track B's central claim was
wrong (F1/F2/F3) and is now corrected to a **snapshot-backed** projection, the
RoundRobin selection API became a real shared dependency (F4/F5), shadow-mode
moved off the hot path (F6), and several task-level infeasibilities were caught
before any code was written.
