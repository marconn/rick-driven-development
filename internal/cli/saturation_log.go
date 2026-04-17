package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/observe"
)

// saturationLogInterval is the cadence at which the periodic dump runs.
// 30s is a compromise: short enough to catch a saturation window inside a
// review-iteration feedback loop, long enough that idle servers don't produce
// log noise.
const saturationLogInterval = 30 * time.Second

// logSaturation runs until ctx is cancelled, emitting a single slog.Info line
// per tick when any saturation signal is active:
//   - Any backend with inflight > 0 or non-zero wait totals
//   - Throttle running or queued count > 0
//   - Runner active dispatches > 0
//
// Completely idle ticks emit nothing — no cost, no log noise.
func logSaturation(ctx context.Context, logger *slog.Logger, sat *observe.Saturation, eng *engine.Engine, runner *engine.PersonaRunner) {
	ticker := time.NewTicker(saturationLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emitSaturationSnapshot(logger, sat, eng, runner)
		}
	}
}

func emitSaturationSnapshot(logger *slog.Logger, sat *observe.Saturation, eng *engine.Engine, runner *engine.PersonaRunner) {
	throttle := eng.ThrottleSnapshot()
	run := runner.Snapshot()
	backends := sat.Snapshot()

	// Suppress silent ticks. Either an active backend, a queued/running
	// workflow, or any dispatcher activity qualifies as "worth logging".
	anyBackendActive := false
	for _, b := range backends {
		if b.Inflight > 0 || b.Waited > 0 {
			anyBackendActive = true
			break
		}
	}
	if !anyBackendActive && throttle.Running == 0 && throttle.Queued == 0 && run.Active == 0 {
		return
	}

	attrs := []any{
		slog.Int("runner_active", int(run.Active)),
		slog.Int("runner_max", int(run.MaxActive)),
	}
	if throttle.Enabled {
		attrs = append(attrs,
			slog.Int("throttle_running", throttle.Running),
			slog.Int("throttle_queued", throttle.Queued),
			slog.Int("throttle_max", throttle.MaxConcurrent),
		)
	}
	for _, b := range backends {
		attrs = append(attrs, slog.Group("backend_"+b.Name,
			slog.Int64("inflight", b.Inflight),
			slog.Int64("acquires", b.Acquires),
			slog.Int64("waited", b.Waited),
			slog.Duration("avg_wait", b.AvgWait()),
			slog.Duration("max_wait", b.MaxWait()),
		))
	}
	logger.Info("saturation", attrs...)
}
