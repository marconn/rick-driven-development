# package eventbus

In-process event pub/sub plumbing for Rick: defines the `Bus` interface, two implementations (in-memory channels and a polling outbox), and a stack of middleware applied at subscribe-time.

## Files
- `bus.go` — `Bus` interface, `HandlerFunc`, `SubscribeOption` (`WithName`, `WithSync`)
- `channel.go` — `ChannelBus` (in-memory async dispatch), `DeadLetter`/`DeadLetterRecorder`
- `outbox.go` — `OutboxBus` (polls event store via `PollFunc`, durable replay)
- `middleware.go` — `Middleware` type, `Chain`, and the 7 built-in wrappers
- `errors.go` — `ErrBusClosed` sentinel
- `channel_test.go`, `outbox_test.go`, `middleware_test.go` — unit tests (skim only if behavior is unclear)

## Key types
- `Bus` interface: `Publish(ctx, env)`, `Subscribe(type, handler, opts...) func()`, `SubscribeAll(handler, opts...) func()`, `Close()`
- `HandlerFunc func(ctx, env.Envelope) error` — non-nil error triggers dead-letter recording
- `ChannelBus` — fan-out via goroutines (`go dispatch`), per-sub `WithSync()` runs in publisher's goroutine to preserve FIFO order (used by Engine). Holds `subscribers map[event.Type][]*subscription` + `allSubs []*subscription` under `sync.RWMutex`. Tracks in-flight goroutines via `sync.WaitGroup` for graceful `Close`. Options: `WithMiddleware`, `WithLogger`, `WithDeadLetterRecorder`.
- `OutboxBus` — polling-based, durability through the event store. `Publish` is just a hint that nudges `notify chan struct{}` (buffered 1); the real work happens in `pollLoop` which calls `PollFunc(ctx, lastPos, batchSize)` on each tick or notify. Single-goroutine dispatch guarantees per-subscriber ordering. Options: `WithPollInterval` (default 100ms), `WithBatchSize` (default 100), `WithStartPosition` (resume checkpoint), `WithOutboxMiddleware`, `WithOutboxLogger`, `WithOutboxDeadLetterRecorder`. Expose `LastPosition()` for checkpointing. Caller must invoke `Start(ctx)` before use.
- `PollFunc func(ctx, afterPosition, limit) ([]PollResult, error)` — injected by caller (typically wraps `eventstore.Store.LoadAll`) to avoid an import cycle on eventstore.
- `DeadLetter` / `DeadLetterRecorder` — `RecordDeadLetter` is implemented by `eventstore.SQLiteStore`.

## Middleware (built-ins, all in `middleware.go`)
- `LoggingMiddleware(logger)` — debug on success, error on failure, with duration
- `RetryMiddleware(maxRetries, baseDelay)` — exponential backoff (`baseDelay << attempt`), respects `ctx.Done()`
- `CircuitBreakerMiddleware(threshold, resetTimeout)` — Closed/Open/HalfOpen state machine, per-handler `sync.Mutex`
- `RecoveryMiddleware()` — `defer recover()` converts panics to errors
- `TimeoutMiddleware(d)` — wraps ctx with `context.WithTimeout` (critical: AI calls can hang 60s+)
- `MetricsMiddleware(recorder, handlerName)` — calls `MetricsRecorder.RecordEventProcessed`
- `IdempotencyMiddleware(maxSize)` — bounded `map[event.ID]struct{}`, evicts half when full

## Patterns
- **Middleware composition**: `Chain(mw1, mw2, mw3)` makes mw1 outermost (mw1 wraps mw2 wraps mw3 wraps handler). Wired once via `WithMiddleware`/`WithOutboxMiddleware` and applied at subscribe-time, not per-dispatch.
- **Subscribe returns unsubscribe** — closure that locks and removes the `*subscription` by `id` (atomic counter). Safe to call repeatedly.
- **Sync vs async (ChannelBus only)**: default is goroutine-per-event. `WithSync()` runs inline; the Engine relies on this to keep its single-goroutine FIFO contract.
- **OutboxBus durability**: events are already in SQLite before `Publish` is called, so a crash mid-dispatch is recoverable — restart with `WithStartPosition(LastPosition())`.
- **Dead letters**: any handler error is logged AND persisted via `DeadLetterRecorder` (if configured). One DL per failed delivery; retries inside `RetryMiddleware` only generate a DL on the final failure.
- **Close semantics**: `closed atomic.Bool` + `wg.Wait()` (ChannelBus) or `cancel()` + `wg.Wait()` (OutboxBus). Publishing post-close returns `ErrBusClosed`.

## Related
- `../event` — `Envelope`, `Type`, `ID` (consumed by every handler)
- `../engine` — primary subscriber; uses `WithSync()` for FIFO lifecycle decisions and configures the middleware stack at startup
- `../eventstore` — provides `LoadAll` (wrapped as `PollFunc` for `OutboxBus`) and implements `DeadLetterRecorder`
- `../handler` — `HandlerFunc` here is distinct from `handler.Handler`; PersonaRunner adapts the latter onto bus subscriptions
