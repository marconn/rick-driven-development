package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// tracedEvent captures an event with its wall-clock offset for trace output.
type tracedEvent struct {
	Offset  time.Duration
	Env     event.Envelope
	Summary string
}

// eventTracer subscribes to all bus events and records them in order.
type eventTracer struct {
	mu      sync.Mutex
	events  []tracedEvent
	started time.Time
}

func newEventTracer(t *testing.T, bus eventbus.Bus) *eventTracer {
	t.Helper()
	tr := &eventTracer{started: time.Now()}
	unsub := bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.events = append(tr.events, tracedEvent{
			Offset:  time.Since(tr.started),
			Env:     env,
			Summary: summarize(env),
		})
		return nil
	}, eventbus.WithName("test:tracer"))
	t.Cleanup(unsub)
	return tr
}

func (tr *eventTracer) dump(t *testing.T) []tracedEvent {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()

	// Sort by offset for deterministic output.
	sorted := make([]tracedEvent, len(tr.events))
	copy(sorted, tr.events)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Offset < sorted[j].Offset
	})

	t.Logf("\n=== EVENT TRACE (%d events) ===", len(sorted))
	t.Logf("%-10s %-28s %-35s %s", "Offset", "Type", "Source/Aggregate", "Details")
	t.Logf("%s", strings.Repeat("─", 120))
	for _, e := range sorted {
		aggShort := e.Env.AggregateID
		if len(aggShort) > 30 {
			aggShort = "…" + aggShort[len(aggShort)-29:]
		}
		src := e.Env.Source
		if src == "" {
			src = "(none)"
		}
		t.Logf("%-10s %-28s %-35s %s",
			e.Offset.Truncate(time.Millisecond),
			e.Env.Type,
			fmt.Sprintf("%s [%s]", aggShort, src),
			e.Summary,
		)
	}
	t.Logf("%s", strings.Repeat("─", 120))

	// Also dump unique event types seen.
	types := make(map[event.Type]int)
	for _, e := range sorted {
		types[e.Env.Type]++
	}
	t.Logf("\n=== EVENT TYPE SUMMARY ===")
	for et, count := range types {
		t.Logf("  %-30s ×%d", et, count)
	}

	return sorted
}

func summarize(env event.Envelope) string {
	switch env.Type {
	case event.WorkflowRequested:
		var p event.WorkflowRequestedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("prompt=%q wf=%s", truncate(p.Prompt, 40), p.WorkflowID)
	case event.WorkflowCompleted:
		var p event.WorkflowCompletedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return p.Result
	case event.WorkflowFailed:
		var p event.WorkflowFailedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("reason=%q phase=%s", truncate(p.Reason, 50), p.Phase)
	case event.PersonaCompleted:
		var p event.PersonaCompletedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("persona=%s trigger=%s chain=%d dur=%dms",
			p.Persona, p.TriggerEvent, p.ChainDepth, p.DurationMS)
	case event.PersonaFailed:
		var p event.PersonaFailedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("persona=%s err=%q", p.Persona, truncate(p.Error, 40))
	case event.VerdictRendered:
		var p event.VerdictPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("outcome=%s phase=%s source=%s issues=%d",
			p.Outcome, p.Phase, p.SourcePhase, len(p.Issues))
	case event.FeedbackGenerated:
		var p event.FeedbackGeneratedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("target=%s source=%s iter=%d", p.TargetPhase, p.SourcePhase, p.Iteration)
	case event.WorkflowPaused:
		var p event.WorkflowPausedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return fmt.Sprintf("reason=%q", truncate(p.Reason, 50))
	case event.WorkflowResumed:
		return "resumed"
	default:
		if event.IsWorkflowStarted(env.Type) {
			var p event.WorkflowStartedPayload
			_ = json.Unmarshal(env.Payload, &p)
			return fmt.Sprintf("phases=%v", p.Phases)
		}
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// =============================================================================
// Scenario A: Default workflow — full 6-persona chain (happy path)
// =============================================================================

func TestTraceDefaultWorkflowHappyPath(t *testing.T) {
	def := WorkflowDef{
		ID:            "workspace-dev",
		Required:      []string{"researcher", "architect", "developer", "reviewer", "qa", "committer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"researcher": {},
			"architect":  {"researcher"},
			"developer":  {"architect"},
			"reviewer":   {"developer"},
			"qa":         {"developer"},
			"committer":  {"reviewer", "qa"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)
	tracer := newEventTracer(t, env.bus)

	// Register all 6 personas with stub handlers.
	personas := []struct {
		name    string
		events  []event.Type
		after   []string
	}{
		{"researcher", []event.Type{event.WorkflowStartedFor("workspace-dev")}, nil},
		{"architect", []event.Type{event.PersonaCompleted}, []string{"researcher"}},
		{"developer", []event.Type{event.PersonaCompleted, event.FeedbackGenerated}, []string{"architect"}},
		{"reviewer", []event.Type{event.PersonaCompleted}, []string{"developer"}},
		{"qa", []event.Type{event.PersonaCompleted}, []string{"developer"}},
		{"committer", []event.Type{event.PersonaCompleted}, []string{"reviewer", "qa"}},
	}
	for _, p := range personas {
		p := p
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{
				name:   p.name,
				handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
			},
			trigger: handler.Trigger{Events: p.events, AfterPersonas: p.after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-trace-happy")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-trace-happy", "workspace-dev")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}

	time.Sleep(100 * time.Millisecond)
	tracer.dump(t)
}

// =============================================================================
// Scenario B: Default workflow with feedback loop (reviewer fails once)
// =============================================================================

func TestTraceDefaultWorkflowWithFeedback(t *testing.T) {
	def := WorkflowDef{
		ID:            "workspace-dev",
		Required:      []string{"researcher", "architect", "developer", "reviewer", "qa", "committer"},
		MaxIterations: 3,
		PhaseMap:      corePhaseMap,
		Graph: map[string][]string{
			"researcher": {},
			"architect":  {"researcher"},
			"developer":  {"architect"},
			"reviewer":   {"developer"},
			"qa":         {"developer"},
			"committer":  {"reviewer", "qa"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)
	tracer := newEventTracer(t, env.bus)

	var reviewCount atomic.Int32

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	noop := func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil }

	register("researcher", []event.Type{event.WorkflowStartedFor("workspace-dev")}, nil, noop)
	register("architect", []event.Type{event.PersonaCompleted}, []string{"researcher"}, noop)
	register("developer", []event.Type{event.PersonaCompleted, event.FeedbackGenerated}, []string{"architect"}, noop)
	register("reviewer", []event.Type{event.PersonaCompleted}, []string{"developer"},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			n := reviewCount.Add(1)
			if n == 1 {
				return []event.Envelope{
					event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
						Phase:       "develop",
						SourcePhase: "review",
						Outcome:     event.VerdictFail,
						Summary:     "missing error handling in handler.go:42",
						Issues: []event.Issue{
							{Severity: "major", Category: "correctness", Description: "missing error handling in handler.go:42"},
							{Severity: "minor", Category: "style", Description: "unused import in util.go:3"},
						},
					})),
				}, nil
			}
			return nil, nil
		})
	register("qa", []event.Type{event.PersonaCompleted}, []string{"developer"}, noop)
	register("committer", []event.Type{event.PersonaCompleted}, []string{"reviewer", "qa"}, noop)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-trace-feedback")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-trace-feedback", "workspace-dev")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout")
	}

	time.Sleep(200 * time.Millisecond)
	tracer.dump(t)
}

// =============================================================================
// Scenario C: Workspace-dev workflow (happy path)
// =============================================================================

func TestTraceWorkspaceDevHappyPath(t *testing.T) {
	def := WorkflowDef{
		ID:                "workspace-dev",
		Required:          []string{"workspace", "context-snapshot", "developer", "reviewer", "committer"},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		Graph: map[string][]string{
			"workspace":        {},
			"context-snapshot": {"workspace"},
			"developer":        {"context-snapshot"},
			"reviewer":         {"developer"},
			"committer":        {"reviewer"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)
	tracer := newEventTracer(t, env.bus)

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	noop := func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil }

	register("workspace", []event.Type{event.WorkflowStartedFor("workspace-dev")}, nil, noop)
	register("context-snapshot", []event.Type{event.PersonaCompleted}, []string{"workspace"},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				event.New(event.ContextCodebase, 1, event.MustMarshal(event.ContextCodebasePayload{
					Language: "go", Framework: "grpc",
					Tree: []event.FileEntry{
						{Path: "cmd/server/main.go", Size: 1200, Language: "go"},
						{Path: "internal/api/handler.go", Size: 3400, Language: "go"},
						{Path: "go.mod", Size: 250},
					},
				})),
			}, nil
		})
	register("developer", []event.Type{event.PersonaCompleted, event.FeedbackGenerated}, []string{"context-snapshot"}, noop)
	register("reviewer", []event.Type{event.PersonaCompleted}, []string{"developer"}, noop)
	register("committer", []event.Type{event.PersonaCompleted}, []string{"reviewer"}, noop)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-trace-wsdev")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-trace-wsdev", "workspace-dev")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}

	time.Sleep(100 * time.Millisecond)
	tracer.dump(t)
}

// =============================================================================
// Scenario D: Max iterations → escalation (auto-pause)
// =============================================================================

func TestTraceEscalation(t *testing.T) {
	def := WorkflowDef{
		ID:                "escalate-test",
		Required:          []string{"developer", "reviewer"},
		MaxIterations:     1,
		EscalateOnMaxIter: true,
		PhaseMap:          corePhaseMap,
		Graph:             map[string][]string{"developer": {}, "reviewer": {"developer"}},
		RetriggeredBy:     map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)
	tracer := newEventTracer(t, env.bus)

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	register("developer", []event.Type{event.WorkflowStartedFor("escalate-test"), event.FeedbackGenerated}, nil,
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil })
	register("reviewer", []event.Type{event.PersonaCompleted}, []string{"developer"},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Phase: "develop", SourcePhase: "review",
					Outcome: event.VerdictFail, Summary: "code quality insufficient",
				})),
			}, nil
		})

	ctx := context.Background()

	// Wait for pause instead of completion.
	paused := make(chan event.Envelope, 1)
	unsubPause := env.bus.Subscribe(event.WorkflowPaused, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID == "wf-trace-escalate" {
			select {
			case paused <- e:
			default:
			}
		}
		return nil
	}, eventbus.WithName("test:pause-detect"))
	defer unsubPause()

	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-trace-escalate", "escalate-test")

	select {
	case <-paused:
		// Expected — workflow escalated to operator.
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: escalation never triggered")
	}

	time.Sleep(200 * time.Millisecond)
	tracer.dump(t)
}
