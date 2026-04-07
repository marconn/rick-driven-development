# package estimation

SQLite-backed history and calibration store that feeds the BTU planning workflow's `estimator` persona with prior estimates and bias adjustments to refine Fibonacci story-point sizing.

## Files
- `storage.go` — `Store` type, schema migration, batch insert, calibration + similar-task lookup helpers

## Key types
- `Store` — wraps `*sql.DB` (modernc.org/sqlite, WAL, 5s busy timeout); constructed via `NewStore(dbPath)`, closed via `Close()`
- `Estimate` — single record: `ID, TicketID, TaskDescription, Microservice, Category, EstimatedPoints, ActualPoints (*int), CorrelationID, EstimatedAt, ActualAt (*time.Time), Notes`
- `Category` is a free-form string; convention: `"frontend" | "backend" | "infra"`

## Public API
- `NewStore(dbPath string) (*Store, error)` — opens/creates DB, runs `migrate`
- `(*Store).SaveBatch(ctx, []Estimate) error` — single-tx insert; auto-fills empty `ID` with `uuid.New()`, stamps `estimated_at` to `time.Now().Format(RFC3339)`
- `(*Store).CalibrationSummary(ctx) (string, error)` — human-readable text from `calibration_adjustments`, ordered by `sample_count DESC`; returns "No calibration data available yet..." sentinel when empty
- `(*Store).SimilarEstimates(ctx, microservice, category) (string, error)` — last 10 records with non-null `actual_points`; prefers `microservice` filter, falls back to `category`, returns empty string if both blank

## Patterns
- Schema (via `migrate`):
  - `estimation_history(id PK, ticket_id, task_description, microservice, category, estimated_points, actual_points, correlation_id, estimated_at, actual_at, notes)` + indexes on `ticket_id`, `microservice`, `category`
  - `calibration_adjustments(category PK, bias_direction, avg_deviation, sample_count, last_updated)` — written externally (no helper here); read by `CalibrationSummary`
- `SaveBatch` only inserts the estimation columns — `actual_points`/`actual_at` are filled later out-of-band (post-sprint reconciliation, not implemented in this package)
- Errors wrapped with `estimation:` prefix per repo convention
- Local `abs(float64)` helper since math.Abs would pull stdlib for one call
- Env var: `ESTIMATION_DB` — read by `internal/cli/deps.go` to choose DB path; package itself takes a path argument and is env-agnostic

## Related
- `../planning/estimator.go` — `EstimatorHandler` consumes `*estimation.Store`, calls `CalibrationSummary` + `SimilarEstimates` to enrich the estimator prompt and `SaveBatch` to persist results
- `../cli/deps.go` — wires `ESTIMATION_DB` env var into `NewStore` at startup
- `../handler/handlers.go` — registers the estimator persona in the `plan-btu` workflow DAG
- Root `CLAUDE.md` — `plan-btu` workflow section documents the estimator's place in the pipeline
