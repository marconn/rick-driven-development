package eventbus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// Middleware wraps a HandlerFunc, returning a new HandlerFunc.
type Middleware func(HandlerFunc) HandlerFunc

// Chain applies middleware in order: first middleware is outermost.
func Chain(middlewares ...Middleware) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		for i := len(middlewares) - 1; i >= 0; i-- {
			next = middlewares[i](next)
		}
		return next
	}
}

// LoggingMiddleware logs event handling with duration.
func LoggingMiddleware(logger *slog.Logger) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			start := time.Now()
			err := next(ctx, env)
			duration := time.Since(start)

			attrs := []any{
				slog.String("event_type", string(env.Type)),
				slog.String("event_id", string(env.ID)),
				slog.String("aggregate_id", env.AggregateID),
				slog.Duration("duration", duration),
			}
			if err != nil {
				attrs = append(attrs, slog.String("error", err.Error()))
				logger.Error("event handler failed", attrs...)
			} else {
				logger.Debug("event handled", attrs...)
			}
			return err
		}
	}
}

// RetryMiddleware retries failed handler calls with exponential backoff.
func RetryMiddleware(maxRetries int, baseDelay time.Duration) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			var lastErr error
			for attempt := 0; attempt <= maxRetries; attempt++ {
				lastErr = next(ctx, env)
				if lastErr == nil {
					return nil
				}
				if attempt < maxRetries {
					delay := baseDelay * time.Duration(1<<uint(attempt))
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
		}
	}
}

// CircuitBreakerState tracks circuit breaker state.
type CircuitBreakerState int

const (
	CircuitClosed   CircuitBreakerState = iota // normal operation
	CircuitOpen                                 // failing, reject calls
	CircuitHalfOpen                             // testing recovery
)

// CircuitBreakerMiddleware implements the circuit breaker pattern.
func CircuitBreakerMiddleware(threshold int, resetTimeout time.Duration) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		var (
			mu           sync.Mutex
			failures     int
			state        CircuitBreakerState
			lastFailTime time.Time
		)
		return func(ctx context.Context, env event.Envelope) error {
			mu.Lock()
			switch state {
			case CircuitOpen:
				if time.Since(lastFailTime) > resetTimeout {
					state = CircuitHalfOpen
				} else {
					mu.Unlock()
					return fmt.Errorf("circuit breaker open for event %s", env.Type)
				}
			}
			mu.Unlock()

			err := next(ctx, env)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				lastFailTime = time.Now()
				if failures >= threshold {
					state = CircuitOpen
				}
				return err
			}

			// Success: reset failures in any non-open state
			failures = 0
			if state == CircuitHalfOpen {
				state = CircuitClosed
			}
			return nil
		}
	}
}

// RecoveryMiddleware catches panics in handlers and converts them to errors.
func RecoveryMiddleware() Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("handler panicked: %v", r)
				}
			}()
			return next(ctx, env)
		}
	}
}

// TimeoutMiddleware enforces a maximum duration for handler execution.
// AI calls can hang for 60+ seconds; without per-handler timeouts, goroutines leak.
func TimeoutMiddleware(d time.Duration) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, env)
		}
	}
}

// MetricsRecorder records event processing metrics.
type MetricsRecorder interface {
	RecordEventProcessed(eventType event.Type, handlerName string, duration time.Duration, err error)
}

// MetricsMiddleware records event processing metrics (counts, latency, errors).
func MetricsMiddleware(recorder MetricsRecorder, handlerName string) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			start := time.Now()
			err := next(ctx, env)
			recorder.RecordEventProcessed(env.Type, handlerName, time.Since(start), err)
			return err
		}
	}
}

// IdempotencyMiddleware prevents duplicate processing of the same event.
// At-least-once delivery + retries can cause the same event to be processed
// multiple times. This middleware tracks processed event IDs and skips duplicates.
func IdempotencyMiddleware(maxSize int) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		var mu sync.Mutex
		processed := make(map[event.ID]struct{}, maxSize)
		return func(ctx context.Context, env event.Envelope) error {
			mu.Lock()
			if _, seen := processed[env.ID]; seen {
				mu.Unlock()
				return nil // already processed, skip
			}
			processed[env.ID] = struct{}{}
			// Evict oldest entries if over capacity
			if len(processed) > maxSize {
				// Simple eviction: clear half the map
				count := 0
				for id := range processed {
					delete(processed, id)
					count++
					if count >= maxSize/2 {
						break
					}
				}
			}
			mu.Unlock()
			return next(ctx, env)
		}
	}
}
