# package projection

In-memory read-model projections built from the event stream — used by MCP tools and the gRPC NotificationBroker for fast workflow status / token / timeline / verdict reads.

## Files
- `projection.go` — `Projector` interface, `Runner` (catch-up + live), shared types (`WorkflowStatus`, `TokenUsage`, `PhaseTimeline`, `VerdictRecord`, `VerdictIssue`)
- `workflow.go` — `WorkflowStatusProjection`
- `tokens.go` — `TokenUsageProjection`
- `timeline.go` — `PhaseTimelineProjection` (+ `personaFromAggregate` helper)
- `verdict.go` — `VerdictProjection`
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
