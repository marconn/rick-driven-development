# package grpchandler

Wraps the `PersonaService` proto stubs to expose Rick's PersonaRunner over a bidirectional gRPC stream — service discovery, dispatch routing, event injection, watch notifications, and dynamic workflow registration.

## Layout
- `*.go` (this dir) — server, client, dispatchers, broker, injector, conversions, allowlist
- `proto/` — protobuf wire format + generated stubs → see `proto/CLAUDE.md`

## Files
- `server.go` — `Server` (PersonaService impl), `proxyHandler`, `gRPCHinter`
- `client.go` — reconnecting `Client` + `ClientConfig` + `RegisterWorkflowOption`s
- `stream_dispatcher.go` — `StreamDispatcher` (engine.Dispatcher impl over streams)
- `composite_dispatcher.go` — `CompositeDispatcher` (local-first, stream fallback)
- `notification_broker.go` — `NotificationBroker` (terminal-state push w/ catch-up)
- `injector.go` — `EventInjector` (validates + persists externally injected events)
- `convert.go` — `EnvelopeToProto` / `ProtoToEnvelope`
- `allowlist.go` — `IsInjectable` allowlist for externally injectable event types

## Key types
- `Server` — `HandleStream` is the single bidi RPC; first message must be `HandlerRegistration`. Routes `HandlerResult`/`Heartbeat`/`InjectEventRequest`/`Watch`/`Unwatch`/`RegisterWorkflowRequest` from the recv loop.
- `Client` / `ClientConfig` — `Run(ctx)` opens stream, registers, processes dispatches, auto-reconnects. `InjectEvent`, `WatchWorkflow`/`UnwatchWorkflow`, `RegisterWorkflow` multiplex over the same stream via `streamMu` + pending request maps.
- `StreamDispatcher` — `Register(name, sendCh) → token`, `Unregister(name, token)`, `Dispatch`, `DispatchHint` (sets `HintOnly=true`), `DeliverResult`, `Names`, `Has`. Per-handler `handlerStream` tracks pending dispatch result channels.
- `CompositeDispatcher` — local first; only falls back to stream on `engine.ErrHandlerNotFound`. Local errors are NOT swallowed.
- `NotificationBroker` — bus subscriber for `WorkflowCompleted/Failed/Cancelled`; routes to specific + wildcard watchers; enriches via `WorkflowStatus`/`TokenUsage`/`PhaseTimeline`/`Verdict` projections.
- `EventInjector` / `InjectRequest` — validates type via `IsInjectable`, blocks new events on terminal workflows, retries up to 3x on `ErrConcurrencyConflict`.
- `proxyHandler` — placeholder local `handler.Handler` so PersonaRunner can subscribe + evaluate joins; its `Handle` panics (real dispatch goes through `CompositeDispatcher` → `StreamDispatcher`).
- `gRPCHinter` — adapts `handler.Hinter` to `StreamDispatcher.DispatchHint` for clients with `supports_hints=true`.

## Patterns
- **Stream lifecycle = service discovery**: open registers, close deregisters (proxy handler, before-hooks, hinter, broker watches all torn down on disconnect).
- **Token-guarded Unregister**: each `Register` returns a UUID token; `Unregister(name, token)` no-ops if a newer registration replaced it. Prevents a dying displaced stream from evicting its replacement.
- **Displaced notification**: re-registering the same handler name sends `DisplacedNotification` to the old client and closes its pending result channels so its `recvLoop` errors out and reconnects.
- **Reconnecting client**: exponential backoff `BaseDelay * 2^(attempt-1)` capped at `MaxDelay` (defaults 1s → 30s). Re-sends `HandlerRegistration` and re-issues `WatchRequest` on every reconnect. `WatchAll`/`WatchCorrelations` apply on each new stream.
- **Hint dispatch**: `dispatch.HintOnly` routes the client to `cfg.HintHandler` (falls back to `cfg.Handler` for backwards compat). Server-side enabled by `reg.SupportsHints`.
- **Workflow registration merge**: `handleRegisterWorkflow` preserves an existing `Graph`/`RetriggeredBy`/`PhaseMap` if the workflow ID is already known — gRPC registrations only override `Required`/`MaxIterations` and never wipe a built-in DAG.
- **Notification enrichment**: terminal payload provides authoritative status/reason; projections add `StartedAt`/`Duration`/tokens/phases/verdicts. Catch-up on `Watch`: immediately scans `WorkflowStatusProjection` for already-terminal IDs and pushes notifications so newly-watched-but-already-finished workflows aren't lost.
- **Injection allowlist**: only operator/lifecycle/context types are injectable; system-only types like `PersonaCompleted` are blocked to preserve invariants.
- **Sticky `streamMu`**: `Client` serializes all Sends on the active stream (gRPC client streams aren't safe for concurrent Send).
- **`ErrIncomplete` plumbing**: `StreamDispatcher.Dispatch` returns `(result, handler.ErrIncomplete)` when the client sets `result.Incomplete=true` so PersonaRunner persists events but skips PersonaCompleted.

## Related
- `proto/` — wire format, see `proto/CLAUDE.md`
- `../engine` — `Dispatcher` interface, `PersonaRunner.RegisterHandler`/`RegisterHook`/`RegisterExternalHinter`, `Engine.RegisterWorkflow`, `WorkflowDef`, `ErrHandlerNotFound`
- `../handler` — `Handler`, `Hinter`, `Trigger`, `ErrIncomplete`, `Registry`
- `../projection` — `WorkflowStatusProjection`, `TokenUsageProjection`, `PhaseTimelineProjection`, `VerdictProjection` (read by `NotificationBroker`)
- `../event`, `../eventbus`, `../eventstore` — bus subscription, append/load, payload types
- Root `CLAUDE.md` "gRPC Service Discovery" + "External System Integration Guide" sections — end-to-end protocol and example clients
