# package proto

Protobuf wire format (`rick.handler.v1`) for the bidirectional gRPC stream between rick-server and external handler clients.

## Files
- `handler.proto` — source of truth. No Makefile/buf target found; regen via project scripts using `protoc-gen-go v1.36.11` + `protoc-gen-go-grpc v1.6.1` (stamped in generated headers, protoc v6.33.4).
- `handler.pb.go` — generated message types, DO NOT EDIT.
- `handler_grpc.pb.go` — generated service stubs, DO NOT EDIT.

## Service
- `PersonaService.HandleStream(stream HandlerMessage) returns (stream DispatchMessage)` — single bidi RPC; stream open = registration, stream close = deregistration. All dispatch, injection, watch, and workflow registration flows multiplex over this one stream.

## Messages (oneof)

### Client to Server (`HandlerMessage.msg`)
- `HandlerRegistration` — first message: name, event_types, after_personas, before_hook_targets, supports_hints
- `HandlerResult` — response to a DispatchRequest (dispatch_id, events, error, incomplete)
- `Heartbeat` — keep-alive (timestamp_ms)
- `InjectEventRequest` — push event into a running workflow
- `WatchRequest` — subscribe to workflow terminal notifications (empty = watch all)
- `UnwatchRequest` — unsubscribe from notifications
- `RegisterWorkflowRequest` — register a custom workflow definition (required handlers, max_iterations, escalate_on_max_iter)

### Server to Client (`DispatchMessage.msg`)
- `RegistrationAck` — confirms registration (name, status)
- `DispatchRequest` — event to process, with `hint_only` flag for two-phase hint/execute
- `InjectEventResult` — response to InjectEventRequest (success, error, event_id)
- `WorkflowNotification` — terminal state push: status, tokens, phase summaries, verdict details
- `RegisterWorkflowResult` — response with available_handlers / missing_handlers
- `DisplacedNotification` — sent when another client registers with the same handler name; treat as terminal and reconnect

### Shared types
- `EventEnvelope` — wire mirror of `event.Envelope` (id, type, aggregate_id, version, schema_version, timestamp_ms, causation_id, correlation_id, source, payload bytes)
- `PhaseSummary`, `VerdictDetail`, `IssueSummary` — nested in WorkflowNotification

## Regeneration
- No buf config or Makefile target in repo; regen via project scripts using the versions stamped in the generated file headers (`protoc v6.33.4`, `protoc-gen-go v1.36.11`, `protoc-gen-go-grpc v1.6.1`). Go package alias: `handlerpb`.

## Related
- `..` — parent `internal/grpchandler` wraps these stubs (server, client, stream_dispatcher, notification_broker)
- Root CLAUDE.md "gRPC Service Discovery" section documents the end-to-end protocol and client usage
