package eventstore

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// Publisher publishes events to subscribers. Matches the eventbus.Bus Publish signature.
type Publisher interface {
	Publish(ctx context.Context, env event.Envelope) error
}

// RetryResult reports the outcome of a dead letter retry batch.
type RetryResult struct {
	Attempted int
	Succeeded int
	Failed    int
	Errors    []error
}

// RetryDeadLetters loads all dead letters, re-publishes their original events,
// and removes successfully reprocessed dead letters.
func RetryDeadLetters(ctx context.Context, store Store, pub Publisher, logger *slog.Logger) (*RetryResult, error) {
	dls, err := store.LoadDeadLetters(ctx)
	if err != nil {
		return nil, fmt.Errorf("eventstore: load dead letters for retry: %w", err)
	}
	if len(dls) == 0 {
		return &RetryResult{}, nil
	}

	result := &RetryResult{Attempted: len(dls)}
	for _, dl := range dls {
		env, loadErr := store.LoadEvent(ctx, dl.EventID)
		if loadErr != nil {
			result.Failed++
			result.Errors = append(result.Errors,
				fmt.Errorf("dead letter %s: load event %s: %w", dl.ID, dl.EventID, loadErr))
			logger.Warn("dead letter retry: event not found",
				slog.String("dead_letter_id", dl.ID),
				slog.String("event_id", dl.EventID),
				slog.String("error", loadErr.Error()),
			)
			continue
		}

		if pubErr := pub.Publish(ctx, *env); pubErr != nil {
			result.Failed++
			result.Errors = append(result.Errors,
				fmt.Errorf("dead letter %s: republish event %s: %w", dl.ID, dl.EventID, pubErr))
			logger.Warn("dead letter retry: republish failed",
				slog.String("dead_letter_id", dl.ID),
				slog.String("event_id", dl.EventID),
				slog.String("error", pubErr.Error()),
			)
			continue
		}

		if delErr := store.DeleteDeadLetter(ctx, dl.ID); delErr != nil {
			result.Failed++
			result.Errors = append(result.Errors,
				fmt.Errorf("dead letter %s: delete after retry: %w", dl.ID, delErr))
			logger.Error("dead letter retry: delete failed after successful publish",
				slog.String("dead_letter_id", dl.ID),
				slog.String("error", delErr.Error()),
			)
			continue
		}

		result.Succeeded++
		logger.Info("dead letter retried successfully",
			slog.String("dead_letter_id", dl.ID),
			slog.String("event_id", dl.EventID),
			slog.String("handler", dl.Handler),
		)
	}
	return result, nil
}
