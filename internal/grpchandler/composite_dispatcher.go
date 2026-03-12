package grpchandler

import (
	"context"
	"errors"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// CompositeDispatcher tries local dispatch first, falls back to stream
// dispatch for external handlers. Built-in personas stay in-process (fast),
// external enrichers use gRPC streams.
type CompositeDispatcher struct {
	local  engine.Dispatcher
	stream *StreamDispatcher
}

// NewCompositeDispatcher creates a dispatcher that routes to local handlers
// first and falls back to stream-connected external handlers.
func NewCompositeDispatcher(local engine.Dispatcher, stream *StreamDispatcher) *CompositeDispatcher {
	return &CompositeDispatcher{local: local, stream: stream}
}

// Dispatch tries local first. If handler not found locally, tries the stream dispatcher.
func (d *CompositeDispatcher) Dispatch(ctx context.Context, handlerName string, env event.Envelope) (*engine.DispatchResult, error) {
	result, err := d.local.Dispatch(ctx, handlerName, env)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, engine.ErrHandlerNotFound) {
		return nil, err // local handler exists but failed — don't fallback
	}
	return d.stream.Dispatch(ctx, handlerName, env)
}
