# package engine

Workflow lifecycle, DAG-based dispatch, and unhandled-event detection — the central nervous system for Rick's event-sourced orchestrator.

## Files

- `engine.go` — `Engine` struct: subscribes to lifecycle events, serializes them through a single FIFO goroutine (prevents Verdict→PersonaCompleted ordering races from `ChannelBus`), loads the workflow aggregate, runs `Decide`, persists+publishes results. Also indexes business-key tags (`source`, `ticket`, `repo`, `repo_branch`, `workflow_id`) on `WorkflowRequested` and writes a storage-only `PersonaTracked` copy onto the workflow aggregate when a `PersonaCompleted` arrives from a persona-scoped aggregate. Owns the workflow definition registry (`RegisterWorkflow`, `GetWorkflowDef`, `RegisteredWorkflows`) and the `OnWorkflowRegistered` callback hook used by `PersonaRunner.AdjustChainDepth`.
- `aggregate.go` — `WorkflowAggregate` (Apply/Decide). Apply is side-effect-free state rebuild; Decide produces lifecycle events. Handles `WorkflowRequested → WorkflowStarted`, `PersonaCompleted → WorkflowCompleted` (when all `Required` are done), `PersonaFailed → WorkflowFailed` (only for required personas — non-required hook failures are ignored), `VerdictRendered{fail} → FeedbackGenerated` (with phase→persona resolution and `isRequiredPersona` guard so review-only workflows don't corrupt state), `TokenBudgetExceeded → WorkflowFailed`, `WorkflowResumed` re-emits `FeedbackGenerated` and bumps `MaxIterations`, `HintEmitted` auto-approves or pauses based on confidence threshold, `HintRejected` skips or fails. Tracks `FeedbackPending` as a stale-completion guard so in-flight `PersonaCompleted` events from prior iterations don't re-mark cleared personas.
- `persona_runner.go` — `PersonaRunner`, the **sole dispatcher** for all persona handlers. Subscribes handlers based on the workflow Graph (not handler-declared triggers), enforces all safety guards (self-trigger prevention, chain depth, width, idempotency, join-gate dedup, pause), runs the per-(handler, correlation) priority queue, owns the two-phase Hinter dispatch, handles dynamic `RegisterHandler`/`RegisterHook`/`RegisterExternalHinter` for gRPC clients with generation counters to make stale unsubscribes safe. ~1000 lines, the largest file in the package.
- `workflow_def.go` — `WorkflowDef` struct + every built-in workflow factory (`DevelopOnlyWorkflowDef`, `WorkspaceDevWorkflowDef`, `PRReviewWorkflowDef`, `PRFeedbackWorkflowDef`, `JiraDevWorkflowDef`, `PlanBTUWorkflowDef`, `PlanJiraWorkflowDef`, `JiraQAStepsWorkflowDef`, `TaskCreatorWorkflowDef`, `CIFixWorkflowDef`). Includes `DownstreamOf` (BFS over reverse adjacency for stale-event invalidation), `ResolvePhase` (phase verb → handler name via `PhaseMap`), the shared `corePhaseMap` (`develop→developer`, etc.), and `WithoutHandler` for runtime DAG surgery (used by `RICK_DISABLE_QUALITY_GATE`).
- `workflow_resolver.go` — `workflowResolver`: encapsulates DAG resolution. Owns the `correlationID → workflowID` cache, `resolveEventsFromDAG` (computes subscription set from Graph predecessors — empty deps subscribe to `workflow.started.<id>`, non-empty subscribe to `PersonaCompleted`, `RetriggeredBy` adds extra event types), `isDAGRelevant` (predecessor membership check), `isRetriggerable`, `effectiveAfterPersonas` (DAG deps + hooks), and `checkJoinCondition` — the load-by-correlation join check with `pendingStale` tracking that filters out late-arriving `PersonaCompleted` events from the previous iteration of a feedback loop. Falls back to `handler.TriggeredHandler.Trigger()` for handlers absent from every Graph (gRPC backward compat).
- `sentinel.go` — `Sentinel` watches the bus via `SubscribeAll` and emits `UnhandledEventDetected` for any event type that has no registered handler subscriber, skipping the ~30 internal lifecycle/AI/context types in `internalEvents`. Catches misconfigured workflows and orphan events.
- `dispatcher.go` — `Dispatcher` interface (`Dispatch(ctx, handlerName, env) → (*DispatchResult, error)`), `LocalDispatcher` (in-process registry-backed impl), `DispatchResult`, sentinel `ErrHandlerNotFound`. The seam that lets `CompositeDispatcher` (in `../grpchandler`) layer remote handlers on top of local ones. Honors `handler.ErrIncomplete` by returning result events alongside the error.
- `correlation_contexts.go` — `correlationContexts`: per-correlation `context.Context` map for cancellation propagation. Created lazily on first dispatch, cancelled on `WorkflowCancelled`/terminal events/`Close`, propagates to in-flight AI subprocesses.
- `pause_controller.go` — `pauseController`: thread-safe pause/resume/cancel state plus a `blocked` map of dispatches deferred until resume.
- `hook_registry.go` — `hookRegistry` + `hookLookup` interface: persona → before-hook personas. Used by `workflowResolver` to merge hooks into DAG predecessor sets for join-condition checks.
- `result_persister.go` — `resultPersister.persistAndPublish`: shared persist-then-publish helper (3-attempt retry with version reload) used by `executeDispatch`, `executeHint`, and `executeHintApprovedDispatch` to write to persona-scoped aggregates `{correlationID}:persona:{handler}`.
- `dispatch_queues.go` — `dispatchQueue` + `dispatchQueues`: per-(handler, correlation) priority queue. `push` is stable insertion-sort by priority (FIFO within same priority), `pop` removes head, single drain goroutine per queue ensures handler instances never run concurrently on the same workflow.

## Key types

- `Engine` — Lifecycle manager. Holds the registered `WorkflowDef` map, the FIFO `eventCh`, and orchestrates `loadAggregate → tryProcessDecision → store.Append → bus.Publish`. Zero dispatch logic — only emits lifecycle events.
- `WorkflowAggregate` — Domain aggregate. Fields: `Status`, `WorkflowDef` (attached at load time from registry), `CompletedPersonas`, `FeedbackCount`, `FeedbackPending` (stale guard), `TokensUsed`, `MaxIterations`, plus `WorkflowID`/`Source`/`Ticket` indexed at `WorkflowRequested`. `Apply` rebuilds from events, `Decide` produces new ones — these are the only two methods callers should invoke.
- `PersonaRunner` — Sole handler dispatcher. Maintains `handlers` map, `hinters` map, `hooks`, `pauser`, `corrCtxs`, `queues`, `seen` (idempotency cache), `resolver`. Configurable via `WithDrainTimeout`, `WithMaxChainDepth`, `WithMaxActive`, `WithBeforeHook`. `Start(ctx, registry)` subscribes everything; `Close` drains.
- `WorkflowDef` / `Graph` — `Graph` is `map[string][]string` (handler → predecessors). Empty deps = root (subscribes to `workflow.started.<id>`). Required = completion manifest. `RetriggeredBy` adds non-PersonaCompleted re-trigger events (e.g., developer on FeedbackGenerated). `PhaseMap` translates verdict phase verbs to handler names. `HintThreshold` controls auto-approve sensitivity (default 0.7). `EscalateOnMaxIter` switches max-iteration behavior from fail to pause.
- `Dispatcher` interface — Single-method seam that decouples PersonaRunner from execution. `LocalDispatcher` is the in-process impl; `grpchandler.CompositeDispatcher` chains local + remote.
- `Sentinel` — Bus watcher that emits `UnhandledEventDetected` when no handler subscribes to an event type. Skips internal types.
- `workflowResolver` — Internal helper that owns DAG-based dispatch decisions (subscription resolution, relevance checks, join checks). Not exported.
- `dispatchQueue` / `dispatchItem` — Priority-ordered serial queue per (handler, correlation). Priorities: `PriorityOperatorGuidance`(0) > `PriorityFeedbackGenerated`(10) > `PriorityPersonaCompleted`(20) > `PriorityDefault`(30).
- `idempotencyCache` — Bounded LRU dedup keyed by `handler|eventID`. Default size 10K. Used both for raw event dedup and join-gate fingerprint dedup.

## Dispatch model (brief)

- Subscriptions are computed from `WorkflowDef.Graph` at handler-registration time, not from `Subscribes()`. A handler not in any Graph falls back to `TriggeredHandler.Trigger()` for gRPC backward compat.
- On `PersonaCompleted`, `wrap` runs the gauntlet: self-trigger guard → chain-depth → idempotency → width → join condition (`checkJoinCondition` loads by correlation, applies stale-event filtering with `pendingStale`, returns a fingerprint for join-gate dedup) → pause check → enqueue.
- A single drain goroutine per (handler, correlation) key consumes the priority queue serially — different handlers and different workflows still run in parallel. Result events go to persona-scoped aggregate `{correlationID}:persona:{handler}`; `PersonaCompleted`/`PersonaFailed` is appended last.
- Two-phase Hinter dispatch: if `r.hinters[name]` exists and the event isn't `HintApproved`, run `Hint()` instead of `Handle()`. Engine auto-approves above `HintThreshold` with no blockers, otherwise emits `WorkflowPaused` for operator review. `HintApproved` reloads the original trigger event from the correlation chain and replays it through `executeHintApprovedDispatch`.

## Patterns

- Handlers return result events only — they never call `store.Append` or `bus.Publish`. The runner owns atomicity via `resultPersister`.
- Aggregate `Apply` is side-effect-free; only `Decide` produces events. This keeps replay deterministic.
- All write paths are version-checked (`store.Append(ctx, aggID, baseVersion, events)`) — concurrent updates to the same aggregate fail with `ErrConcurrencyConflict` and retry via `resultPersister`.
- Persona-scoped aggregates (`{correlation}:persona:{handler}`) keep handler outputs isolated; the workflow aggregate gets a `PersonaTracked` mirror so `CompletedPersonas` survives replay without cross-aggregate reads.
- Workflow lookup by business key uses `store.SaveTags`/`LoadByTag` — Engine indexes them on `WorkflowRequested`.
- Generation counters on `RegisterHandler` make repeated gRPC reconnects safe: only the active generation can unsubscribe.
- `WithoutHandler` mutates a `WorkflowDef` copy at construction time (used by `RICK_DISABLE_QUALITY_GATE`); the runtime never edits a registered Graph.

## Related

- `../handler` — `Handler` interface, `Registry`, `Hinter`, `TriggeredHandler` (legacy fallback), `ErrIncomplete` sentinel.
- `../event` — Event types, envelopes, payload structs (`PersonaCompletedPayload`, `VerdictPayload`, `FeedbackGeneratedPayload`, `HintEmittedPayload`, etc.) and `WorkflowStartedFor(id)`.
- `../eventbus` — `Bus` interface, `Subscribe`/`SubscribeAll`/`Publish`, middleware. The runner uses `WithName` for traceability.
- `../eventstore` — `Store` interface, `Append`/`Load`/`LoadFrom`/`LoadByCorrelation`/`SaveTags`/`LoadByTag`/snapshots.
- `../grpchandler` — `Server`, `Client`, `CompositeDispatcher`, `StreamDispatcher`, `NotificationBroker`. External handlers register here and end up driven by this package's `PersonaRunner`.
- `../projection` — Workflow status, token usage, phase timeline, verdict projections that consume the events this package emits.
