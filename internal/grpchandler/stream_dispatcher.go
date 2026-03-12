package grpchandler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// StreamDispatcher implements engine.Dispatcher by routing dispatch calls
// to external handlers connected via gRPC bidirectional streams.
type StreamDispatcher struct {
	mu      sync.RWMutex
	streams map[string]*handlerStream // handler name → active stream
	logger  *slog.Logger
}

// handlerStream tracks an active gRPC stream for a registered handler.
type handlerStream struct {
	name   string
	token  string // unique per registration; guards against stale Unregister calls
	send   chan<- *pb.DispatchMessage
	result map[string]chan *pb.HandlerResult // dispatch_id → result channel
	mu     sync.Mutex
}

// NewStreamDispatcher creates a dispatcher for gRPC-connected handlers.
func NewStreamDispatcher(logger *slog.Logger) *StreamDispatcher {
	return &StreamDispatcher{
		streams: make(map[string]*handlerStream),
		logger:  logger,
	}
}

// Register adds a stream for a named handler. Returns a registration token that
// must be passed to Unregister. Called by the gRPC server when a client sends
// HandlerRegistration.
//
// If a handler with the same name is already registered, the old stream is
// evicted: a DisplacedNotification is sent to the old client (non-blocking),
// all pending dispatches are unblocked, and a warning is logged. The token
// mechanism ensures the old stream's deferred Unregister does not accidentally
// delete the new registration.
func (d *StreamDispatcher) Register(name string, send chan<- *pb.DispatchMessage) string {
	token := uuid.New().String()

	d.mu.Lock()
	defer d.mu.Unlock()

	if old, ok := d.streams[name]; ok {
		d.logger.Warn("stream dispatcher: displacing existing handler — possible duplicate process",
			slog.String("handler", name),
		)
		// Notify the displaced client so its recvLoop returns an error and
		// triggers reconnection rather than hanging as a silent zombie.
		select {
		case old.send <- &pb.DispatchMessage{
			Msg: &pb.DispatchMessage_Displaced{
				Displaced: &pb.DisplacedNotification{
					Handler: name,
					Reason:  "another client registered with the same handler name",
				},
			},
		}:
		default:
			// Send channel full — client is not draining. Proceed anyway;
			// the channel close below will unblock it.
		}
		old.mu.Lock()
		for _, ch := range old.result {
			close(ch)
		}
		old.mu.Unlock()
	}

	d.streams[name] = &handlerStream{
		name:   name,
		token:  token,
		send:   send,
		result: make(map[string]chan *pb.HandlerResult),
	}
	d.logger.Info("stream dispatcher: handler registered", slog.String("handler", name))
	return token
}

// Unregister removes a handler stream only if the token matches the current
// registration. This prevents a dying displaced stream's deferred Unregister
// from accidentally evicting the replacement registration.
func (d *StreamDispatcher) Unregister(name, token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	hs, ok := d.streams[name]
	if !ok || hs.token != token {
		return // stale unregister from a displaced stream — ignore
	}
	hs.mu.Lock()
	for _, ch := range hs.result {
		close(ch)
	}
	hs.mu.Unlock()
	delete(d.streams, name)
	d.logger.Info("stream dispatcher: handler unregistered", slog.String("handler", name))
}


// Names returns the names of all currently connected gRPC handlers.
func (d *StreamDispatcher) Names() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.streams))
	for name := range d.streams {
		names = append(names, name)
	}
	return names
}

// Has returns true if a handler is connected via stream.
func (d *StreamDispatcher) Has(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.streams[name]
	return ok
}

// Dispatch sends an event to the external handler and waits for the result.
func (d *StreamDispatcher) Dispatch(ctx context.Context, handlerName string, env event.Envelope) (*engine.DispatchResult, error) {
	d.mu.RLock()
	hs, ok := d.streams[handlerName]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (no stream)", engine.ErrHandlerNotFound, handlerName)
	}

	dispatchID := uuid.New().String()
	resultCh := make(chan *pb.HandlerResult, 1)

	hs.mu.Lock()
	hs.result[dispatchID] = resultCh
	hs.mu.Unlock()

	defer func() {
		hs.mu.Lock()
		delete(hs.result, dispatchID)
		hs.mu.Unlock()
	}()

	// Send dispatch request to the external handler.
	req := &pb.DispatchMessage{
		Msg: &pb.DispatchMessage_Dispatch{
			Dispatch: &pb.DispatchRequest{
				DispatchId: dispatchID,
				Event:      EnvelopeToProto(env),
			},
		},
	}

	select {
	case hs.send <- req:
	case <-ctx.Done():
		return nil, fmt.Errorf("stream dispatcher: send cancelled: %w", ctx.Err())
	}

	// Wait for result.
	select {
	case res, ok := <-resultCh:
		if !ok {
			return nil, fmt.Errorf("stream dispatcher: stream closed for %q", handlerName)
		}
		if res.Error != "" {
			return nil, fmt.Errorf("stream dispatcher: handler %q: %s", handlerName, res.Error)
		}
		var events []event.Envelope
		for _, pe := range res.Events {
			env := ProtoToEnvelope(pe)
			// Assign ID if the external handler didn't set one.
			if env.ID == "" {
				env.ID = event.ID(uuid.New().String())
			}
			events = append(events, env)
		}
		result := &engine.DispatchResult{Events: events, Handler: handlerName}
		if res.Incomplete {
			return result, handler.ErrIncomplete
		}
		return result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("stream dispatcher: result cancelled: %w", ctx.Err())
	}
}

// DispatchHint sends a hint-only event to the external handler and waits for
// the result. Identical to Dispatch but sets HintOnly=true on the request.
func (d *StreamDispatcher) DispatchHint(ctx context.Context, handlerName string, env event.Envelope) (*engine.DispatchResult, error) {
	d.mu.RLock()
	hs, ok := d.streams[handlerName]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (no stream)", engine.ErrHandlerNotFound, handlerName)
	}

	dispatchID := uuid.New().String()
	resultCh := make(chan *pb.HandlerResult, 1)

	hs.mu.Lock()
	hs.result[dispatchID] = resultCh
	hs.mu.Unlock()

	defer func() {
		hs.mu.Lock()
		delete(hs.result, dispatchID)
		hs.mu.Unlock()
	}()

	req := &pb.DispatchMessage{
		Msg: &pb.DispatchMessage_Dispatch{
			Dispatch: &pb.DispatchRequest{
				DispatchId: dispatchID,
				Event:      EnvelopeToProto(env),
				HintOnly:   true,
			},
		},
	}

	select {
	case hs.send <- req:
	case <-ctx.Done():
		return nil, fmt.Errorf("stream dispatcher: hint send cancelled: %w", ctx.Err())
	}

	select {
	case res, ok := <-resultCh:
		if !ok {
			return nil, fmt.Errorf("stream dispatcher: stream closed for %q", handlerName)
		}
		if res.Error != "" {
			return nil, fmt.Errorf("stream dispatcher: handler %q hint: %s", handlerName, res.Error)
		}
		var events []event.Envelope
		for _, pe := range res.Events {
			env := ProtoToEnvelope(pe)
			if env.ID == "" {
				env.ID = event.ID(uuid.New().String())
			}
			events = append(events, env)
		}
		return &engine.DispatchResult{Events: events, Handler: handlerName}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("stream dispatcher: hint result cancelled: %w", ctx.Err())
	}
}

// DeliverResult routes a HandlerResult from the gRPC stream to the waiting
// Dispatch call. Called by the gRPC server when a client sends a result.
func (d *StreamDispatcher) DeliverResult(handlerName string, res *pb.HandlerResult) {
	d.mu.RLock()
	hs, ok := d.streams[handlerName]
	d.mu.RUnlock()
	if !ok {
		return
	}

	hs.mu.Lock()
	ch, ok := hs.result[res.DispatchId]
	hs.mu.Unlock()
	if ok {
		ch <- res
	}
}
