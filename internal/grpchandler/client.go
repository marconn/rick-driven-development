package grpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

const (
	defaultBaseDelay = 1 * time.Second
	defaultMaxDelay  = 30 * time.Second
)

// ClientConfig holds the configuration for a reconnecting gRPC client.
type ClientConfig struct {
	// Name is the handler name (e.g., "frontend-enricher").
	Name string

	// EventTypes are the event types this handler subscribes to.
	EventTypes []string

	// AfterPersonas is the join condition — handler fires only after all
	// listed personas have completed for the same correlation.
	AfterPersonas []string

	// BeforeHookTargets are personas that should wait for this handler
	// before dispatching (e.g., ["developer"]).
	BeforeHookTargets []string

	// Handler processes dispatched events and returns result events.
	// The signature mirrors handler.Handler.Handle but takes event.Envelope
	// directly, decoupled from the internal handler.Handler interface.
	Handler func(ctx context.Context, env event.Envelope) ([]event.Envelope, error)

	// NotificationHandler receives workflow lifecycle push notifications.
	// If nil, notifications are silently discarded.
	NotificationHandler func(ctx context.Context, notif *pb.WorkflowNotification)

	// WatchCorrelations are correlation IDs to watch on connect.
	// Empty slice = watch all workflows. Nil = no watching.
	WatchCorrelations []string

	// HintHandler processes hint requests (hint_only=true dispatches).
	// If nil, falls back to Handler for hint dispatches (backwards compatible).
	HintHandler func(ctx context.Context, env event.Envelope) ([]event.Envelope, error)

	// WatchAll if true, watches all workflow completions.
	WatchAll bool

	// Logger for reconnection and lifecycle events.
	Logger *slog.Logger

	// MaxRetries is the maximum reconnection attempts (0 = unlimited).
	MaxRetries int

	// BaseDelay is the initial reconnection backoff delay (default: 1s).
	BaseDelay time.Duration

	// MaxDelay is the maximum reconnection backoff delay (default: 30s).
	MaxDelay time.Duration
}

// Client manages a resilient gRPC stream connection to Rick's PersonaService.
// When the stream drops for any reason other than context cancellation, Client
// retries with exponential backoff, re-sends the HandlerRegistration, and
// resumes processing dispatches — all transparently to the caller.
type Client struct {
	cfg    ClientConfig
	conn   *grpc.ClientConn
	logger *slog.Logger

	// pendingInjects tracks in-flight InjectEvent calls keyed by request_id.
	pendingInjects sync.Map // string → chan *pb.InjectEventResult

	// pendingWorkflows tracks in-flight RegisterWorkflow calls keyed by request_id.
	pendingWorkflows sync.Map // string → chan *pb.RegisterWorkflowResult

	// streamMu serializes concurrent Send calls on the active stream.
	streamMu     sync.Mutex
	activeStream pb.PersonaService_HandleStreamClient
}

// NewClient creates a new reconnecting Client. The conn is owned by the caller
// — Client will not close it. Call Run to start the stream.
func NewClient(conn *grpc.ClientConn, cfg ClientConfig) *Client {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = defaultBaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultMaxDelay
	}
	return &Client{
		cfg:    cfg,
		conn:   conn,
		logger: logger,
	}
}

// Run connects to Rick's PersonaService, registers this handler, and processes
// dispatched events. It blocks until ctx is cancelled or MaxRetries is
// exceeded.
//
// On stream failure (not ctx cancellation) it retries with exponential backoff:
//
//	delay = min(BaseDelay * 2^attempt, MaxDelay)
//
// Returns nil on clean ctx cancellation, error when MaxRetries is exceeded.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		err := c.runOnce(ctx)

		// Clean shutdown — ctx was cancelled.
		if ctx.Err() != nil {
			c.logger.Info("grpc client: context cancelled, shutting down",
				slog.String("handler", c.cfg.Name),
			)
			return nil
		}

		// Stream error — decide whether to retry.
		attempt++
		if c.cfg.MaxRetries > 0 && attempt > c.cfg.MaxRetries {
			return fmt.Errorf("grpc client: %s: exceeded max retries (%d): %w",
				c.cfg.Name, c.cfg.MaxRetries, err)
		}

		delay := c.backoff(attempt)
		c.logger.Warn("grpc client: stream error, reconnecting",
			slog.String("handler", c.cfg.Name),
			slog.Int("attempt", attempt),
			slog.Duration("backoff", delay),
			slog.Any("error", err),
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// runOnce opens a single stream, registers, and processes dispatches until
// the stream closes or an error occurs.
func (c *Client) runOnce(ctx context.Context) error {
	pbClient := pb.NewPersonaServiceClient(c.conn)

	stream, err := pbClient.HandleStream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Step 1: Send HandlerRegistration as the first message.
	reg := &pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{
				Name:              c.cfg.Name,
				EventTypes:        c.cfg.EventTypes,
				AfterPersonas:     c.cfg.AfterPersonas,
				BeforeHookTargets: c.cfg.BeforeHookTargets,
			},
		},
	}
	if err := stream.Send(reg); err != nil {
		return fmt.Errorf("send registration: %w", err)
	}

	// Step 2: Wait for RegistrationAck.
	ackMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv ack: %w", err)
	}
	ack := ackMsg.GetAck()
	if ack == nil {
		return fmt.Errorf("expected RegistrationAck as first server message")
	}
	if ack.Status != "ok" {
		return fmt.Errorf("registration rejected: %s", ack.Status)
	}

	c.logger.Info("grpc client: registered",
		slog.String("handler", c.cfg.Name),
		slog.Any("event_types", c.cfg.EventTypes),
		slog.Any("after_personas", c.cfg.AfterPersonas),
		slog.Any("before_hook_targets", c.cfg.BeforeHookTargets),
	)

	// Step 2b: Send WatchRequest if configured.
	if c.cfg.WatchAll || c.cfg.WatchCorrelations != nil {
		watchMsg := &pb.HandlerMessage{
			Msg: &pb.HandlerMessage_Watch{
				Watch: &pb.WatchRequest{CorrelationIds: c.cfg.WatchCorrelations},
			},
		}
		if err := stream.Send(watchMsg); err != nil {
			return fmt.Errorf("send watch: %w", err)
		}
	}

	// Step 3: Store the active stream for InjectEvent calls.
	c.streamMu.Lock()
	c.activeStream = stream
	c.streamMu.Unlock()
	defer func() {
		c.streamMu.Lock()
		c.activeStream = nil
		c.streamMu.Unlock()
	}()

	// Step 4: Process dispatch requests until stream closes.
	return c.recvLoop(ctx, stream)
}

// recvLoop reads DispatchRequests from the stream, calls cfg.Handler, and
// sends HandlerResult back. Returns when the stream closes or an error occurs.
func (c *Client) recvLoop(ctx context.Context, stream pb.PersonaService_HandleStreamClient) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return fmt.Errorf("stream closed by server (EOF)")
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}

		switch {
		case msg.GetDispatch() != nil:
			result := c.handleDispatch(ctx, msg.GetDispatch())
			c.streamMu.Lock()
			sendErr := stream.Send(result)
			c.streamMu.Unlock()
			if sendErr != nil {
				return fmt.Errorf("send result: %w", sendErr)
			}
		case msg.GetInjectResult() != nil:
			ir := msg.GetInjectResult()
			if ch, ok := c.pendingInjects.LoadAndDelete(ir.RequestId); ok {
				ch.(chan *pb.InjectEventResult) <- ir
			}
		case msg.GetNotification() != nil:
			if c.cfg.NotificationHandler != nil {
				notif := msg.GetNotification()
				go c.cfg.NotificationHandler(ctx, notif)
			}
		case msg.GetWorkflowResult() != nil:
			wr := msg.GetWorkflowResult()
			if ch, ok := c.pendingWorkflows.LoadAndDelete(wr.RequestId); ok {
				ch.(chan *pb.RegisterWorkflowResult) <- wr
			}
		case msg.GetDisplaced() != nil:
			d := msg.GetDisplaced()
			c.logger.Warn("grpc client: displaced by another instance — will reconnect",
				slog.String("handler", d.Handler),
				slog.String("reason", d.Reason),
			)
			return fmt.Errorf("displaced: %s", d.Reason)
		default:
			// RegistrationAck arriving mid-stream would be unexpected — skip.
			continue
		}
	}
}

// handleDispatch calls cfg.Handler for a single dispatch and builds the
// HandlerResult. Handler errors are returned as a non-empty error string in the
// result — the stream itself remains intact.
func (c *Client) handleDispatch(ctx context.Context, dispatch *pb.DispatchRequest) *pb.HandlerMessage {
	env := ProtoToEnvelope(dispatch.Event)

	c.logger.Debug("grpc client: dispatched",
		slog.String("handler", c.cfg.Name),
		slog.String("dispatch_id", dispatch.DispatchId),
		slog.String("event_type", string(env.Type)),
		slog.String("correlation_id", env.CorrelationID),
	)

	// Use HintHandler for hint_only dispatches if available.
	handlerFn := c.cfg.Handler
	if dispatch.HintOnly && c.cfg.HintHandler != nil {
		handlerFn = c.cfg.HintHandler
	}
	resultEvents, handlerErr := handlerFn(ctx, env)

	result := &pb.HandlerResult{
		DispatchId: dispatch.DispatchId,
	}
	if errors.Is(handlerErr, handler.ErrIncomplete) {
		// Handler is incomplete — signal to server, don't set error.
		result.Incomplete = true
		result.Events = make([]*pb.EventEnvelope, len(resultEvents))
		for i, e := range resultEvents {
			result.Events[i] = EnvelopeToProto(e)
		}
	} else if handlerErr != nil {
		c.logger.Error("grpc client: handler error",
			slog.String("handler", c.cfg.Name),
			slog.String("dispatch_id", dispatch.DispatchId),
			slog.Any("error", handlerErr),
		)
		result.Error = handlerErr.Error()
	} else {
		result.Events = make([]*pb.EventEnvelope, len(resultEvents))
		for i, e := range resultEvents {
			result.Events[i] = EnvelopeToProto(e)
		}
	}

	return &pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Result{Result: result},
	}
}

// InjectEvent sends an event into a Rick workflow through the existing stream.
// Blocks until the server responds with InjectEventResult or ctx is cancelled.
// Returns the server-assigned event ID on success.
func (c *Client) InjectEvent(ctx context.Context, correlationID string, eventType event.Type, payload json.RawMessage) (string, error) {
	c.streamMu.Lock()
	stream := c.activeStream
	c.streamMu.Unlock()
	if stream == nil {
		return "", fmt.Errorf("grpc client: no active stream")
	}

	requestID := uuid.New().String()
	resultCh := make(chan *pb.InjectEventResult, 1)
	c.pendingInjects.Store(requestID, resultCh)
	defer c.pendingInjects.Delete(requestID)

	msg := &pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Inject{
			Inject: &pb.InjectEventRequest{
				RequestId: requestID,
				Event: &pb.EventEnvelope{
					Type:          string(eventType),
					CorrelationId: correlationID,
					Payload:       payload,
				},
			},
		},
	}

	c.streamMu.Lock()
	err := stream.Send(msg)
	c.streamMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("grpc client: send inject: %w", err)
	}

	select {
	case res := <-resultCh:
		if !res.Success {
			return "", fmt.Errorf("grpc client: inject rejected: %s", res.Error)
		}
		return res.EventId, nil
	case <-ctx.Done():
		return "", fmt.Errorf("grpc client: inject cancelled: %w", ctx.Err())
	}
}

// WatchWorkflow sends a WatchRequest for the given correlation IDs through the
// active stream. Fire-and-forget — no server response expected.
func (c *Client) WatchWorkflow(ctx context.Context, correlationIDs ...string) error {
	c.streamMu.Lock()
	stream := c.activeStream
	c.streamMu.Unlock()
	if stream == nil {
		return fmt.Errorf("grpc client: no active stream")
	}

	msg := &pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Watch{
			Watch: &pb.WatchRequest{CorrelationIds: correlationIDs},
		},
	}
	c.streamMu.Lock()
	err := stream.Send(msg)
	c.streamMu.Unlock()
	if err != nil {
		return fmt.Errorf("grpc client: send watch: %w", err)
	}
	return nil
}

// UnwatchWorkflow sends an UnwatchRequest for the given correlation IDs through
// the active stream. Fire-and-forget — no server response expected.
func (c *Client) UnwatchWorkflow(ctx context.Context, correlationIDs ...string) error {
	c.streamMu.Lock()
	stream := c.activeStream
	c.streamMu.Unlock()
	if stream == nil {
		return fmt.Errorf("grpc client: no active stream")
	}

	msg := &pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Unwatch{
			Unwatch: &pb.UnwatchRequest{CorrelationIds: correlationIDs},
		},
	}
	c.streamMu.Lock()
	err := stream.Send(msg)
	c.streamMu.Unlock()
	if err != nil {
		return fmt.Errorf("grpc client: send unwatch: %w", err)
	}
	return nil
}

// RegisterWorkflow registers a custom workflow definition on the server. The
// def is a completion manifest — it declares which handler names must emit
// PersonaCompleted for the workflow to succeed. Returns which required handlers
// are currently available and which are missing (may connect later).
func (c *Client) RegisterWorkflow(ctx context.Context, workflowID string, required []string, opts ...RegisterWorkflowOption) (*pb.RegisterWorkflowResult, error) {
	c.streamMu.Lock()
	stream := c.activeStream
	c.streamMu.Unlock()
	if stream == nil {
		return nil, fmt.Errorf("grpc client: no active stream")
	}

	cfg := registerWorkflowConfig{maxIterations: 3}
	for _, o := range opts {
		o(&cfg)
	}

	requestID := uuid.New().String()
	resultCh := make(chan *pb.RegisterWorkflowResult, 1)
	c.pendingWorkflows.Store(requestID, resultCh)
	defer c.pendingWorkflows.Delete(requestID)

	msg := &pb.HandlerMessage{
		Msg: &pb.HandlerMessage_RegisterWorkflow{
			RegisterWorkflow: &pb.RegisterWorkflowRequest{
				RequestId:         requestID,
				WorkflowId:        workflowID,
				Required:          required,
				MaxIterations:     int32(cfg.maxIterations),
				EscalateOnMaxIter: cfg.escalateOnMaxIter,
			},
		},
	}

	c.streamMu.Lock()
	err := stream.Send(msg)
	c.streamMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("grpc client: send register workflow: %w", err)
	}

	select {
	case res := <-resultCh:
		if !res.Success {
			return res, fmt.Errorf("grpc client: register workflow rejected: %s", res.Error)
		}
		return res, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("grpc client: register workflow cancelled: %w", ctx.Err())
	}
}

type registerWorkflowConfig struct {
	maxIterations     int
	escalateOnMaxIter bool
}

// RegisterWorkflowOption configures a RegisterWorkflow call.
type RegisterWorkflowOption func(*registerWorkflowConfig)

// WithMaxIterations sets the max feedback loop iterations for the workflow.
func WithMaxIterations(n int) RegisterWorkflowOption {
	return func(c *registerWorkflowConfig) { c.maxIterations = n }
}

// WithEscalateOnMaxIter pauses instead of failing when max iterations is reached.
func WithEscalateOnMaxIter() RegisterWorkflowOption {
	return func(c *registerWorkflowConfig) { c.escalateOnMaxIter = true }
}

// backoff computes the delay for a given attempt using exponential backoff
// capped at MaxDelay: BaseDelay * 2^(attempt-1).
func (c *Client) backoff(attempt int) time.Duration {
	// Use float64 exponentiation to avoid integer overflow on large attempt counts.
	multiplier := math.Pow(2, float64(attempt-1))
	delay := time.Duration(float64(c.cfg.BaseDelay) * multiplier)
	if delay > c.cfg.MaxDelay || delay <= 0 {
		return c.cfg.MaxDelay
	}
	return delay
}
