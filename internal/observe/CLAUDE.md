# package observe

Thread-safe in-memory metrics collector for event processing observations, designed to plug into eventbus middleware.

## Files
- `metrics.go` — `Metrics` collector + `EventMetric` aggregate
- `metrics_test.go` — compile-time assertion that `*Metrics` satisfies `eventbus.MetricsRecorder`, plus unit coverage

## Key types / functions
- `Metrics` — RWMutex-guarded `map[metricKey]*EventMetric`; implements `eventbus.MetricsRecorder`
- `NewMetrics() *Metrics` — constructor (no options, no config)
- `(*Metrics).RecordEventProcessed(eventType, handlerName, duration, err)` — single observation; increments `Count`, adds to `TotalNanos`, bumps `Errors` on non-nil err, tracks min/max nanos
- `(*Metrics).Get(eventType, handler) (EventMetric, bool)` — lookup one bucket
- `(*Metrics).All() []EventMetric` — snapshot of every bucket (copies values, safe to read)
- `EventMetric` — `{EventType, Handler, Count, Errors, TotalNanos, MinNanos, MaxNanos}`
- `(EventMetric).AvgDuration() time.Duration` — guards against zero count
- `(EventMetric).ErrorRate() float64` — `Errors / Count`, zero on empty bucket
- `metricKey` — unexported `{EventType, Handler}` composite map key

## Env vars
- None. The package reads zero environment state. (`RICK_LOG_LEVEL` lives in `internal/cli/serve.go`, not here.)

## Patterns
- Buckets are keyed by `(event.Type, handlerName)` — separates the same event observed by different handlers
- First observation seeds `MinNanos`/`MaxNanos` from the incoming sample (avoids zero-init bias)
- `Get`/`All` return value copies, never the internal pointer — callers cannot mutate state
- No background goroutines, no flushing, no eviction — fully synchronous and bounded by the cardinality of `(eventType, handler)` pairs
- No dependency on `slog` or any logging stack; this package is metrics-only despite the name

## Related
- Implements `eventbus.MetricsRecorder` (see `../eventbus/middleware.go:155`); intended to be passed into `eventbus.MetricsMiddleware(recorder, handlerName)`
- Currently NOT wired into any production code path — `internal/observe` has no importers outside its own test file. Wiring belongs in whoever constructs the `ChannelBus`/`OutboxBus` middleware chain (today `internal/cli/serve.go` or `internal/engine` setup)
- Consumes `event.Type` from `../event`
