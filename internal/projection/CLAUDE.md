# package projection

In-memory read-model projections built from the event stream — used by MCP tools and the gRPC NotificationBroker for fast workflow status / token / timeline / verdict reads.

## Files
- `projection.go` — `Projector` interface, `Runner` (catch-up + live), shared types (`WorkflowStatus`, `TokenUsage`, `PhaseTimeline`, `VerdictRecord`, `VerdictIssue`)
- `workflow.go` — `WorkflowStatusProjection`
- `tokens.go` — `TokenUsageProjection`
- `timeline.go` — `PhaseTimelineProjection` (+ `personaFromAggregate` helper)
- `verdict.go` — `VerdictProjection`
- `dwell.go` — `DwellProjection` (telemetry gate)
- `projection_test.go` — coverage for all four projections (skipped here)

## Runner (`projection.go`)
- `NewRunner(store, bus, logger)` → `Register(p)` → `Start(ctx)` → `Stop()`
- `Start`: `catchUp` then `subscribeLive`. Catch-up pages `store.LoadAll` in batches of `catchUpBatchSize=500`, advancing `lastPosition`
- Live subscription via `bus.SubscribeAll` with `eventbus.WithName("projection-runner")` + `WithSync()` so projections settle before `Publish` returns (MCP read-after-write consistency)
- `fanOut`: invokes every projector; logs but does not fail on per-projector errors
- `Position()` exposes last processed global position

## Projection types
- **`WorkflowStatusProjection`** — keyed by `AggregateID`. Reacts to `WorkflowRequested`, `workflow.started.*` family (via `event.IsWorkflowStarted`), `WorkflowCompleted/Failed/Cancelled/Paused/Resumed`. Tracks status, prompt, source, ticket, phases, started/completed timestamps, fail reason. Methods: `Get(aggregateID)`, `All()`
- **`TokenUsageProjection`** — keyed by `AggregateID`. Reacts to `AIResponseReceived`, accumulating `Total`, `ByPhase`, `ByBackend`. Methods: `Get(aggregateID)` (deep-copies maps), `ForWorkflow(correlationID)` aggregates across persona-scoped aggregates by matching `correlationID` or `correlationID + ":"` prefix (convention: `{correlationID}:persona:{handler}`)
- **`PhaseTimelineProjection`** — keyed by `personaKey{CorrelationID, Persona}`. Reacts to `AIRequestSent` (sets `running` + StartedAt, derives persona via `personaFromAggregate`), `PersonaCompleted` (increments `Iterations`, sets `done`, `Duration` from payload `DurationMS`, back-fills StartedAt if absent), `PersonaFailed` (sets `failed`). Methods: `Get(aggregateID, phase)`, `ForWorkflow(aggregateID)`
- **`VerdictProjection`** — keyed by `CorrelationID` → `[]VerdictRecord`. Reacts to `VerdictRendered`, **appends** every record (keeps all retry iterations). Method: `ForWorkflow(correlationID)` returns deep-copied slice
- **`DwellProjection`** — keyed by `{CorrelationID, Persona}` → `DwellRecord`. Telemetry that decides WHICH stall class strands workflows (go/no-go gate for the dispatch-projection track). Folds `DispatchDropped` (first/last drop, per-reason counts, last-held-up reason), `DispatchStarted` (execution start), `AIResponseReceived` (resolved backend for review bucketing), `PersonaCompleted/Failed` (terminal + completion time), and the workflow-started family (first-wait clock origin). `BlockedDwell()` = first-drop→completion; `ExecutionDuration()` = start→completion. Methods: `ForWorkflow`, `Records`, `SummaryByReason`, `WorkflowStartedAt`. **Live-vs-rebuild caveat:** `DispatchDropped`/`DispatchStarted` are store-only (never on the bus), so the Runner's live subscription doesn't deliver them — authoritative data comes from a fresh **catch-up rebuild** (process restart / offline replay via `LoadAll`), which is also why the projection must be a pure rebuildable fold. SQL straight over the diagnostic aggregates (no rebuild needed for ad-hoc analysis):

```sql
-- Dwell by drop reason: how long each (correlation, persona) sat blocked,
-- from its first DispatchDropped to the persona's terminal event.
WITH drops AS (
  SELECT correlation_id,
         json_extract(payload,'$.handler')     AS persona,
         json_extract(payload,'$.drop_reason') AS reason,
         MIN(timestamp)                         AS first_drop
  FROM events WHERE type='dispatch.dropped' GROUP BY 1,2,3
),
done AS (
  SELECT correlation_id,
         json_extract(payload,'$.persona') AS persona,
         MIN(timestamp)                     AS completed
  FROM events WHERE type IN ('persona.completed','persona.failed') GROUP BY 1,2
)
SELECT d.reason,
       COUNT(*)                                                   AS blocked_records,
       AVG((julianday(done.completed)-julianday(d.first_drop))*86400) AS mean_blocked_s,
       MAX((julianday(done.completed)-julianday(d.first_drop))*86400) AS max_blocked_s
FROM drops d JOIN done USING (correlation_id, persona)
GROUP BY d.reason ORDER BY mean_blocked_s DESC;

-- Execution duration for non-AI handlers (DispatchStarted → terminal).
SELECT json_extract(s.payload,'$.persona') AS persona,
       AVG((julianday(t.ts)-julianday(s.timestamp))*86400) AS mean_exec_s
FROM events s
JOIN (SELECT correlation_id, json_extract(payload,'$.persona') AS persona,
             MIN(timestamp) AS ts FROM events
      WHERE type IN ('persona.completed','persona.failed') GROUP BY 1,2) t
  ON t.correlation_id=s.correlation_id
 AND t.persona=json_extract(s.payload,'$.persona')
WHERE s.type='dispatch.started' GROUP BY persona ORDER BY mean_exec_s DESC;
```

## Patterns
- All projections are in-memory `map`s guarded by `sync.RWMutex`; no persistence (rebuilt on every process start via Runner catch-up)
- `Handle` is idempotent for switch-style updates but **VerdictProjection appends** — replays will duplicate verdicts if state isn't cleared first (Runner only runs once at startup, so safe in practice)
- Getters return copies (struct value or deep-cloned maps/slices) so callers can't mutate projection state
- `getOrCreate` helper inside each projection initializes empty entries on first event
- Persona-scoped aggregate convention `{correlationID}:persona:{handlerName}` — `personaFromAggregate` and `TokenUsageProjection.ForWorkflow` both depend on it

## Related
- `../event` — envelope types and payload structs consumed by every projector
- `../eventbus` — `Bus.SubscribeAll` with `WithSync` for live updates
- `../eventstore` — `Store.LoadAll(ctx, position, limit)` for catch-up
- `../engine` — emits the lifecycle/persona/verdict events these projections read
- `../grpchandler` — `NotificationBroker` calls `TokenUsageProjection.ForWorkflow` and `VerdictProjection.ForWorkflow` to enrich `WorkflowNotification`
- `../mcp` — workflow status / token / timeline / verdict tools read projections directly
