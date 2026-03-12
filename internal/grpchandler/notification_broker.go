package grpchandler

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// NotificationBroker subscribes to terminal workflow events on the bus and
// routes WorkflowNotification messages to watching gRPC streams. Clients
// register watches via Watch/Unwatch; the broker pushes notifications through
// the same sendCh used for dispatch messages.
type NotificationBroker struct {
	mu       sync.RWMutex
	watchers map[string]map[string]chan<- *pb.DispatchMessage // correlationID → handlerName → sendCh
	wildcard map[string]chan<- *pb.DispatchMessage            // handlerName → sendCh (watch all)

	workflows *projection.WorkflowStatusProjection
	tokens    *projection.TokenUsageProjection
	timelines *projection.PhaseTimelineProjection
	verdicts  *projection.VerdictProjection

	bus    eventbus.Bus
	unsubs []func()
	logger *slog.Logger
}

// NewNotificationBroker creates a broker that watches for terminal workflow
// events and pushes notifications to registered watchers.
func NewNotificationBroker(
	bus eventbus.Bus,
	workflows *projection.WorkflowStatusProjection,
	tokens *projection.TokenUsageProjection,
	timelines *projection.PhaseTimelineProjection,
	verdicts *projection.VerdictProjection,
	logger *slog.Logger,
) *NotificationBroker {
	return &NotificationBroker{
		watchers:  make(map[string]map[string]chan<- *pb.DispatchMessage),
		wildcard:  make(map[string]chan<- *pb.DispatchMessage),
		workflows: workflows,
		tokens:    tokens,
		timelines: timelines,
		verdicts:  verdicts,
		bus:       bus,
		logger:    logger,
	}
}

// Start subscribes to terminal workflow events on the bus.
func (b *NotificationBroker) Start() {
	for _, eventType := range []event.Type{
		event.WorkflowCompleted,
		event.WorkflowFailed,
		event.WorkflowCancelled,
	} {
		unsub := b.bus.Subscribe(eventType, b.handleTerminalEvent,
			eventbus.WithName("notification-broker:"+string(eventType)),
		)
		b.unsubs = append(b.unsubs, unsub)
	}
	b.logger.Info("notification broker: started")
}

// Stop unsubscribes from all bus events.
func (b *NotificationBroker) Stop() {
	for _, unsub := range b.unsubs {
		unsub()
	}
	b.unsubs = nil
	b.logger.Info("notification broker: stopped")
}

// Watch registers the given sendCh as a watcher for the specified correlation
// IDs. If correlationIDs is empty, the watcher receives notifications for all
// workflows (wildcard). On registration, immediately checks for already-terminal
// workflows and pushes catch-up notifications.
func (b *NotificationBroker) Watch(handlerName string, correlationIDs []string, sendCh chan<- *pb.DispatchMessage) {
	b.mu.Lock()
	if len(correlationIDs) == 0 {
		b.wildcard[handlerName] = sendCh
	} else {
		for _, cid := range correlationIDs {
			watchers, ok := b.watchers[cid]
			if !ok {
				watchers = make(map[string]chan<- *pb.DispatchMessage)
				b.watchers[cid] = watchers
			}
			watchers[handlerName] = sendCh
		}
	}
	b.mu.Unlock()

	b.logger.Info("notification broker: watch registered",
		slog.String("handler", handlerName),
		slog.Int("correlation_ids", len(correlationIDs)),
	)

	// Catch-up: check if any watched workflows are already terminal.
	b.sendCatchUp(handlerName, correlationIDs, sendCh)
}

// Unwatch removes the watcher for the specified correlation IDs. If
// correlationIDs is empty, removes the wildcard watcher.
func (b *NotificationBroker) Unwatch(handlerName string, correlationIDs []string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(correlationIDs) == 0 {
		delete(b.wildcard, handlerName)
	} else {
		for _, cid := range correlationIDs {
			if watchers, ok := b.watchers[cid]; ok {
				delete(watchers, handlerName)
				if len(watchers) == 0 {
					delete(b.watchers, cid)
				}
			}
		}
	}
}

// UnwatchAll removes all watches (specific + wildcard) for the given handler.
// Called on stream disconnect to prevent leaks.
func (b *NotificationBroker) UnwatchAll(handlerName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.wildcard, handlerName)

	for cid, watchers := range b.watchers {
		delete(watchers, handlerName)
		if len(watchers) == 0 {
			delete(b.watchers, cid)
		}
	}
}

// handleTerminalEvent is the bus handler for WorkflowCompleted/Failed/Cancelled.
func (b *NotificationBroker) handleTerminalEvent(_ context.Context, env event.Envelope) error {
	correlationID := env.CorrelationID
	notif := b.buildNotification(correlationID, env)

	msg := &pb.DispatchMessage{
		Msg: &pb.DispatchMessage_Notification{Notification: notif},
	}

	b.mu.RLock()
	targets := b.collectTargets(correlationID)
	b.mu.RUnlock()

	for _, sendCh := range targets {
		select {
		case sendCh <- msg:
		default:
			b.logger.Warn("notification broker: sendCh full, dropping notification",
				slog.String("correlation_id", correlationID),
			)
		}
	}

	return nil
}

// collectTargets returns all sendCh channels that should receive a notification
// for the given correlationID. Must be called under b.mu.RLock.
func (b *NotificationBroker) collectTargets(correlationID string) []chan<- *pb.DispatchMessage {
	seen := make(map[string]bool)
	var targets []chan<- *pb.DispatchMessage

	// Specific watchers for this correlation.
	if watchers, ok := b.watchers[correlationID]; ok {
		for name, ch := range watchers {
			if !seen[name] {
				seen[name] = true
				targets = append(targets, ch)
			}
		}
	}

	// Wildcard watchers.
	for name, ch := range b.wildcard {
		if !seen[name] {
			seen[name] = true
			targets = append(targets, ch)
		}
	}

	return targets
}

// sendCatchUp checks if any of the watched correlationIDs are already in a
// terminal state and pushes immediate notifications. If correlationIDs is empty
// (wildcard), checks all known workflows.
func (b *NotificationBroker) sendCatchUp(handlerName string, correlationIDs []string, sendCh chan<- *pb.DispatchMessage) {
	var toCheck []string
	if len(correlationIDs) == 0 {
		// Wildcard: check all known workflows.
		for _, ws := range b.workflows.All() {
			if isTerminal(ws.Status) {
				toCheck = append(toCheck, ws.AggregateID)
			}
		}
	} else {
		toCheck = correlationIDs
	}

	for _, cid := range toCheck {
		ws, ok := b.workflows.Get(cid)
		if !ok || !isTerminal(ws.Status) {
			continue
		}
		notif := b.buildNotificationFromProjections(cid, ws)
		msg := &pb.DispatchMessage{
			Msg: &pb.DispatchMessage_Notification{Notification: notif},
		}
		select {
		case sendCh <- msg:
			b.logger.Debug("notification broker: catch-up notification sent",
				slog.String("handler", handlerName),
				slog.String("correlation_id", cid),
			)
		default:
			b.logger.Warn("notification broker: sendCh full, dropping catch-up",
				slog.String("handler", handlerName),
				slog.String("correlation_id", cid),
			)
		}
	}
}

// buildNotification builds a WorkflowNotification from the terminal event
// payload (authoritative for status/reason) enriched with projection data.
func (b *NotificationBroker) buildNotification(correlationID string, env event.Envelope) *pb.WorkflowNotification {
	notif := &pb.WorkflowNotification{
		CorrelationId: correlationID,
	}

	// Status and result from the event payload — always available.
	switch env.Type {
	case event.WorkflowCompleted:
		notif.Status = "completed"
		var p event.WorkflowCompletedPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil {
			notif.Result = p.Result
		}
	case event.WorkflowFailed:
		notif.Status = "failed"
		var p event.WorkflowFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil {
			notif.Result = p.Reason
			notif.FailedPhase = p.Phase
		}
	case event.WorkflowCancelled:
		notif.Status = "cancelled"
		var p event.WorkflowCancelledPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil {
			notif.Result = p.Reason
		}
	}

	// Enrichment from projections (graceful fallback if not yet populated).
	if ws, ok := b.workflows.Get(correlationID); ok {
		if !ws.StartedAt.IsZero() {
			notif.StartedAtMs = ws.StartedAt.UnixMilli()
		}
		if !ws.CompletedAt.IsZero() {
			notif.CompletedAtMs = ws.CompletedAt.UnixMilli()
		}
		if !ws.StartedAt.IsZero() && !ws.CompletedAt.IsZero() {
			notif.DurationMs = ws.CompletedAt.Sub(ws.StartedAt).Milliseconds()
		}
	}

	b.enrichTokens(notif, correlationID)
	b.enrichPhases(notif, correlationID)
	b.enrichVerdicts(notif, correlationID)

	return notif
}

// buildNotificationFromProjections builds a notification purely from projection
// data — used for catch-up notifications where there's no triggering event.
func (b *NotificationBroker) buildNotificationFromProjections(correlationID string, ws projection.WorkflowStatus) *pb.WorkflowNotification {
	notif := &pb.WorkflowNotification{
		CorrelationId: correlationID,
		Status:        ws.Status,
		Result:        ws.FailReason, // FailReason doubles as result for failed/cancelled
	}

	if !ws.StartedAt.IsZero() {
		notif.StartedAtMs = ws.StartedAt.UnixMilli()
	}
	if !ws.CompletedAt.IsZero() {
		notif.CompletedAtMs = ws.CompletedAt.UnixMilli()
	}
	if !ws.StartedAt.IsZero() && !ws.CompletedAt.IsZero() {
		notif.DurationMs = ws.CompletedAt.Sub(ws.StartedAt).Milliseconds()
	}

	b.enrichTokens(notif, correlationID)
	b.enrichPhases(notif, correlationID)
	b.enrichVerdicts(notif, correlationID)

	return notif
}

func (b *NotificationBroker) enrichTokens(notif *pb.WorkflowNotification, correlationID string) {
	tu, ok := b.tokens.ForWorkflow(correlationID)
	if !ok {
		return
	}
	notif.TotalTokens = int32(tu.Total)
	if len(tu.ByPhase) > 0 {
		notif.TokensByPhase = make(map[string]int32, len(tu.ByPhase))
		for k, v := range tu.ByPhase {
			notif.TokensByPhase[k] = int32(v)
		}
	}
	if len(tu.ByBackend) > 0 {
		notif.TokensByBackend = make(map[string]int32, len(tu.ByBackend))
		for k, v := range tu.ByBackend {
			notif.TokensByBackend[k] = int32(v)
		}
	}
}

func (b *NotificationBroker) enrichPhases(notif *pb.WorkflowNotification, correlationID string) {
	phases := b.timelines.ForWorkflow(correlationID)
	if len(phases) == 0 {
		return
	}
	notif.Phases = make([]*pb.PhaseSummary, len(phases))
	for i, pt := range phases {
		notif.Phases[i] = &pb.PhaseSummary{
			Phase:      pt.Phase,
			Status:     pt.Status,
			Iterations: int32(pt.Iterations),
			DurationMs: pt.Duration.Milliseconds(),
		}
	}
}

func (b *NotificationBroker) enrichVerdicts(notif *pb.WorkflowNotification, correlationID string) {
	records := b.verdicts.ForWorkflow(correlationID)
	if len(records) == 0 {
		return
	}
	notif.Verdicts = make([]*pb.VerdictDetail, len(records))
	for i, r := range records {
		detail := &pb.VerdictDetail{
			Phase:       r.Phase,
			SourcePhase: r.SourcePhase,
			Outcome:     r.Outcome,
			Summary:     r.Summary,
		}
		if len(r.Issues) > 0 {
			detail.Issues = make([]*pb.IssueSummary, len(r.Issues))
			for j, iss := range r.Issues {
				detail.Issues[j] = &pb.IssueSummary{
					Severity:    iss.Severity,
					Category:    iss.Category,
					Description: iss.Description,
					File:        iss.File,
					Line:        int32(iss.Line),
				}
			}
		}
		notif.Verdicts[i] = detail
	}
}

func isTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}
