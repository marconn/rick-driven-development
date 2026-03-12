package grpchandler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// Server implements the PersonaService gRPC server. It manages the lifecycle
// of external handler streams: registration, dispatch routing, and cleanup.
type Server struct {
	pb.UnimplementedPersonaServiceServer

	stream   *StreamDispatcher
	runner   *engine.PersonaRunner
	injector *EventInjector
	broker   *NotificationBroker
	eng      *engine.Engine
	reg      *handler.Registry
	logger   *slog.Logger
}

// NewServer creates a PersonaService gRPC server.
func NewServer(stream *StreamDispatcher, runner *engine.PersonaRunner, injector *EventInjector, broker *NotificationBroker, eng *engine.Engine, reg *handler.Registry, logger *slog.Logger) *Server {
	return &Server{
		stream:   stream,
		runner:   runner,
		injector: injector,
		broker:   broker,
		eng:      eng,
		reg:      reg,
		logger:   logger,
	}
}

// HandleStream implements the bidirectional streaming RPC. Each connected
// client is an external handler. The first message must be HandlerRegistration.
func (s *Server) HandleStream(stream pb.PersonaService_HandleStreamServer) error {
	// 1. Wait for registration message.
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("grpc server: recv registration: %w", err)
	}
	reg := first.GetRegistration()
	if reg == nil {
		return fmt.Errorf("grpc server: first message must be HandlerRegistration")
	}

	name := reg.Name
	s.logger.Info("grpc server: handler connecting",
		slog.String("handler", name),
		slog.Any("events", reg.EventTypes),
		slog.Any("after", reg.AfterPersonas),
		slog.Any("hooks", reg.BeforeHookTargets),
	)

	// 2. Set up send channel for dispatch requests.
	sendCh := make(chan *pb.DispatchMessage, 16)
	regToken := s.stream.Register(name, sendCh)
	defer s.stream.Unregister(name, regToken)
	if s.broker != nil {
		defer s.broker.UnwatchAll(name)
	}

	// 3. Register a proxy handler with the PersonaRunner so it subscribes
	// to the declared events and evaluates join conditions. The proxy is NOT
	// added to the handler.Registry — when PersonaRunner dispatches via the
	// CompositeDispatcher, LocalDispatcher returns ErrHandlerNotFound, and
	// the call falls through to StreamDispatcher which routes to this stream.
	proxy := newProxyHandler(name, reg)
	unsubHandler := s.runner.RegisterHandler(proxy)
	defer unsubHandler()

	// 4. Register before-hooks if declared. Clean up on disconnect.
	for _, target := range reg.BeforeHookTargets {
		s.runner.RegisterHook(target, name)
	}
	defer func() {
		for _, target := range reg.BeforeHookTargets {
			s.runner.UnregisterHook(target, name)
		}
	}()

	// 5. Register a gRPCHinter proxy if the handler declared supports_hints.
	// This enables two-phase hint/execute dispatch for this external handler.
	if reg.SupportsHints {
		hinter := newGRPCHinter(name, s.stream)
		s.runner.RegisterExternalHinter(name, hinter)
		defer s.runner.UnregisterExternalHinter(name)
		s.logger.Info("grpc server: hint support enabled", slog.String("handler", name))
	}

	// 6. Ack the registration.
	if err := stream.Send(&pb.DispatchMessage{
		Msg: &pb.DispatchMessage_Ack{
			Ack: &pb.RegistrationAck{Name: name, Status: "ok"},
		},
	}); err != nil {
		return fmt.Errorf("grpc server: send ack: %w", err)
	}

	s.logger.Info("grpc server: handler registered", slog.String("handler", name))

	// 7. Run send/recv loops until stream closes.
	errCh := make(chan error, 2)

	// Send loop: forward dispatch requests to the client.
	go func() {
		for msg := range sendCh {
			if err := stream.Send(msg); err != nil {
				errCh <- fmt.Errorf("grpc server: send: %w", err)
				return
			}
		}
	}()

	// Recv loop: receive results and heartbeats from the client.
	go func() {
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- fmt.Errorf("grpc server: recv: %w", err)
				return
			}
			switch m := msg.Msg.(type) {
			case *pb.HandlerMessage_Result:
				s.stream.DeliverResult(name, m.Result)
			case *pb.HandlerMessage_Heartbeat:
				// Keep-alive — no action needed.
			case *pb.HandlerMessage_Inject:
				go s.handleInject(stream.Context(), name, m.Inject, sendCh)
			case *pb.HandlerMessage_Watch:
				if s.broker != nil {
					s.broker.Watch(name, m.Watch.CorrelationIds, sendCh)
				}
			case *pb.HandlerMessage_Unwatch:
				if s.broker != nil {
					s.broker.Unwatch(name, m.Unwatch.CorrelationIds)
				}
			case *pb.HandlerMessage_RegisterWorkflow:
				go s.handleRegisterWorkflow(name, m.RegisterWorkflow, sendCh)
			}
		}
	}()

	// Wait for either loop to finish (stream closed or error).
	streamErr := <-errCh
	close(sendCh) // signal send loop to exit
	s.logger.Info("grpc server: handler disconnected",
		slog.String("handler", name),
	)
	return streamErr
}

// handleInject processes an InjectEventRequest from the stream. It runs in a
// goroutine so store I/O does not block the recv loop.
func (s *Server) handleInject(ctx context.Context, handlerName string, req *pb.InjectEventRequest, sendCh chan<- *pb.DispatchMessage) {
	eventID, err := s.injector.Inject(ctx, InjectRequest{
		CorrelationID: req.Event.GetCorrelationId(),
		EventType:     event.Type(req.Event.GetType()),
		Payload:       json.RawMessage(req.Event.GetPayload()),
		Source:        fmt.Sprintf("grpc:%s", handlerName),
	})

	result := &pb.InjectEventResult{RequestId: req.RequestId}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Success = true
		result.EventId = string(eventID)
	}

	sendCh <- &pb.DispatchMessage{
		Msg: &pb.DispatchMessage_InjectResult{InjectResult: result},
	}
}

// handleRegisterWorkflow processes a RegisterWorkflowRequest: validates the def,
// registers it with the Engine, and returns which required handlers are available.
func (s *Server) handleRegisterWorkflow(handlerName string, req *pb.RegisterWorkflowRequest, sendCh chan<- *pb.DispatchMessage) {
	result := &pb.RegisterWorkflowResult{RequestId: req.RequestId}

	if req.WorkflowId == "" {
		result.Error = "workflow_id is required"
		sendCh <- &pb.DispatchMessage{Msg: &pb.DispatchMessage_WorkflowResult{WorkflowResult: result}}
		return
	}
	if len(req.Required) == 0 {
		result.Error = "at least one required handler must be specified"
		sendCh <- &pb.DispatchMessage{Msg: &pb.DispatchMessage_WorkflowResult{WorkflowResult: result}}
		return
	}

	maxIter := int(req.MaxIterations)
	if maxIter <= 0 {
		maxIter = 3
	}

	def := engine.WorkflowDef{
		ID:                req.WorkflowId,
		Required:          req.Required,
		MaxIterations:     maxIter,
		EscalateOnMaxIter: req.EscalateOnMaxIter,
	}

	// Merge with existing definition: if the workflow is already registered
	// with a Graph (from built-in definitions), preserve it. gRPC registrations
	// only provide Required/MaxIterations — they must not wipe the DAG.
	if existing, ok := s.eng.GetWorkflowDef(req.WorkflowId); ok && len(existing.Graph) > 0 {
		def.Graph = existing.Graph
		def.RetriggeredBy = existing.RetriggeredBy
		def.HintThreshold = existing.HintThreshold
		def.PhaseMap = existing.PhaseMap
		s.logger.Info("grpc server: merging workflow with existing Graph",
			slog.String("workflow_id", def.ID),
			slog.Int("graph_size", len(def.Graph)),
		)
	}

	s.eng.RegisterWorkflow(def)
	s.runner.RegisterWorkflow(def)

	// Resolve which required handlers are currently available.
	localNames := make(map[string]bool)
	if s.reg != nil {
		for _, n := range s.reg.Names() {
			localNames[n] = true
		}
	}
	streamNames := make(map[string]bool)
	for _, n := range s.stream.Names() {
		streamNames[n] = true
	}

	for _, req := range def.Required {
		if localNames[req] || streamNames[req] {
			result.AvailableHandlers = append(result.AvailableHandlers, req)
		} else {
			result.MissingHandlers = append(result.MissingHandlers, req)
		}
	}

	result.Success = true
	s.logger.Info("grpc server: workflow registered",
		slog.String("handler", handlerName),
		slog.String("workflow_id", def.ID),
		slog.Int("required", len(def.Required)),
		slog.Int("available", len(result.AvailableHandlers)),
		slog.Int("missing", len(result.MissingHandlers)),
	)

	sendCh <- &pb.DispatchMessage{Msg: &pb.DispatchMessage_WorkflowResult{WorkflowResult: result}}
}

// proxyHandler is a local handler.Handler that serves as a placeholder in the
// registry. PersonaRunner subscribes it to the bus and evaluates its trigger
// conditions. When PersonaRunner dispatches to it, the CompositeDispatcher
// falls through to the StreamDispatcher (since proxyHandler returns
// ErrHandlerNotFound from the local dispatcher via the proxy's Handle method).
//
// The proxy never actually handles events — dispatch goes to StreamDispatcher.
// It exists only so PersonaRunner can resolve events and check join conditions.
type proxyHandler struct {
	name    string
	trigger handler.Trigger
}

func newProxyHandler(name string, reg *pb.HandlerRegistration) *proxyHandler {
	eventTypes := make([]event.Type, len(reg.EventTypes))
	for i, et := range reg.EventTypes {
		eventTypes[i] = event.Type(et)
	}
	return &proxyHandler{
		name: name,
		trigger: handler.Trigger{
			Events:        eventTypes,
			AfterPersonas: reg.AfterPersonas,
		},
	}
}

func (p *proxyHandler) Name() string             { return p.name }
func (p *proxyHandler) Subscribes() []event.Type { return p.trigger.Events }
func (p *proxyHandler) Trigger() handler.Trigger { return p.trigger }
func (p *proxyHandler) Handle(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
	// Never called — CompositeDispatcher routes to StreamDispatcher.
	panic("proxyHandler.Handle should never be called directly")
}

// gRPCHinter implements handler.Hinter for an externally-connected gRPC handler.
// When PersonaRunner calls Hint(), it dispatches a hint_only=true DispatchRequest
// over the stream and returns the result events. This enables two-phase
// hint/execute dispatch for gRPC handlers that declare supports_hints=true.
type gRPCHinter struct {
	name   string
	stream *StreamDispatcher
}

func newGRPCHinter(name string, stream *StreamDispatcher) *gRPCHinter {
	return &gRPCHinter{name: name, stream: stream}
}

func (h *gRPCHinter) Hint(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	result, err := h.stream.DispatchHint(ctx, h.name, env)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.Events, nil
}
