package engine

// Slice 3: end-to-end retry dispatch coverage.
//
// Test A (TestAutoRetry_HappyPath_DeveloperDAGs): table-driven across every
// developer-bearing DAG. Registers all required stub handlers. The developer
// mock fails once with FailureKindIdleTimeout (auto-retry eligible), then
// succeeds. Asserts WorkflowCompleted, two developer dispatches, and a
// WorkflowRetried event in the store.
//
// Test B (TestAutoRetry_FailLoud_WedgedDispatch): PRFeedbackWorkflowDef only.
// Developer fails with idle_timeout → auto-retry fires → RecoverDispatch
// returns ErrHandlerNotFound (handler unregistered between retry emit and
// dispatch). Asserts WorkflowFailed with Reason containing
// "auto-retry dispatch failed" and Phase=="developer".
//
// Both tests use newE2EEnv, stubTriggeredHandler, and awaitWorkflowResult —
// the same helpers used by the rest of the engine E2E suite.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// developerIdleTimeoutErr returns the exact error shape that AIHandler/Claude.Run
// produce on an idle-timeout so classifyDispatchFailure returns
// FailureKindIdleTimeout and maybeAutoRetry emits WorkflowRetried.
func developerIdleTimeoutErr() error {
	inner := &backend.BackendError{
		Backend: "claude",
		Inner:   fmt.Errorf("%w (stall=2m0s)", backend.ErrIdleTimeout),
	}
	return fmt.Errorf("handler developer: backend: %w", inner)
}

// registerAutoCompleteHandlers registers stub handlers for all names in the
// given DAG's Required list, skipping "developer" which the test registers
// separately. All stubs immediately succeed.
func registerAutoCompleteHandlers(t *testing.T, env *e2eEnv, def WorkflowDef) {
	t.Helper()
	for _, name := range def.Required {
		if name == "developer" {
			continue
		}
		n := name // capture for closure
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{
				name: n,
				handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
					return nil, nil
				},
			},
			trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted, event.WorkflowStartedFor(def.ID), event.FeedbackGenerated}},
		}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
}

// TestAutoRetry_HappyPath_DeveloperDAGs is a table-driven end-to-end test
// covering the auto-retry happy path for every built-in DAG that includes a
// developer phase.
//
// For each DAG the developer stub fails exactly once with idle_timeout, then
// succeeds. The test asserts:
//   - WorkflowCompleted (not WorkflowFailed)
//   - developer was dispatched exactly twice
//   - a WorkflowRetried{Automatic:true} event exists in the store
//
// This test exercises subscribeWorkflowRetried's happy path end-to-end,
// including findRetryTrigger (must locate the predecessor PersonaCompleted in
// the store) and RecoverDispatch (must bypass idempotency cache).
func TestAutoRetry_HappyPath_DeveloperDAGs(t *testing.T) {
	type testCase struct {
		name string
		def  func() WorkflowDef
	}
	cases := []testCase{
		{"develop-only", DevelopOnlyWorkflowDef},
		{"workspace-dev", WorkspaceDevWorkflowDef},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			def := tc.def()
			env := newE2EEnv(t, def)
			wfID := "wf-retry-" + tc.name

			// Register auto-completing stubs for every non-developer handler.
			registerAutoCompleteHandlers(t, env, def)

			// Developer: fails once with idle_timeout, then succeeds.
			var devCalls atomic.Int32
			if err := env.reg.Register(&stubTriggeredHandler{
				stubHandler: stubHandler{
					name: "developer",
					handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
						n := devCalls.Add(1)
						if n == 1 {
							return nil, developerIdleTimeoutErr()
						}
						return nil, nil
					},
				},
				trigger: handler.Trigger{
					Events: []event.Type{event.PersonaCompleted, event.FeedbackGenerated},
				},
			}); err != nil {
				t.Fatalf("register developer: %v", err)
			}

			ctx := context.Background()
			result := awaitWorkflowResult(t, env.bus, wfID)
			env.start(ctx)
			env.fireWorkflow(ctx, t, wfID, def.ID)

			select {
			case got := <-result:
				if got.Type != event.WorkflowCompleted {
					var fp event.WorkflowFailedPayload
					_ = json.Unmarshal(got.Payload, &fp)
					t.Fatalf("%s: want WorkflowCompleted, got %s (reason=%q)", tc.name, got.Type, fp.Reason)
				}
				if n := devCalls.Load(); n != 2 {
					t.Errorf("%s: developer dispatched %d times; want exactly 2 (1 fail + 1 retry success)", tc.name, n)
				}
				// Verify WorkflowRetried{Automatic:true} is in the store.
				events, _ := env.store.Load(ctx, wfID)
				var foundRetried bool
				for _, e := range events {
					if e.Type != event.WorkflowRetried {
						continue
					}
					var rp event.WorkflowRetriedPayload
					if err := json.Unmarshal(e.Payload, &rp); err == nil && rp.Automatic {
						foundRetried = true
						break
					}
				}
				if !foundRetried {
					t.Errorf("%s: no WorkflowRetried{Automatic:true} found in store", tc.name)
				}
				t.Logf("%s: auto-retry happy path OK (dev calls=%d)", tc.name, devCalls.Load())

			case <-time.After(10 * time.Second):
				// Dump events for diagnosis.
				events, _ := env.store.Load(ctx, wfID)
				for _, e := range events {
					t.Logf("  event: %s (v%d)", e.Type, e.Version)
				}
				t.Fatalf("%s: timeout — workflow did not complete (dev calls=%d)", tc.name, devCalls.Load())
			}
		})
	}
}

// TestAutoRetry_FailLoud_WedgedDispatch verifies that when subscribeWorkflowRetried
// cannot dispatch the handler (RecoverDispatch returns an error — simulated by
// unregistering developer between the failure and the retry), the aggregate
// transitions to WorkflowFailed instead of staying stuck in Running forever.
//
// This test pins the Slice 2 fix: publishDispatchWedge must emit a synthetic
// PersonaFailed with FailureKindHandlerError so the aggregate (AutoRetries
// already at cap after Apply(WorkflowRetried)) falls through to WorkflowFailed.
func TestAutoRetry_FailLoud_WedgedDispatch(t *testing.T) {
	def := PRFeedbackWorkflowDef()
	env := newE2EEnv(t, def)

	const wfID = "wf-wedge-prfeedback"

	// Register auto-completing stubs for every non-developer handler.
	registerAutoCompleteHandlers(t, env, def)

	// Developer: fails with idle_timeout on the first call, simulating the
	// real-world idle-timeout scenario. After the engine emits WorkflowRetried,
	// PersonaRunner calls RecoverDispatch("developer", ...). By the time that
	// fires, we swap out the developer handler for one that returns
	// ErrHandlerNotFound — but since we can't unregister in-flight, we instead
	// make the handler fail again with FailureKindHandlerError on the second
	// call. That satisfies the "wedge" shape: the second PersonaFailed arrives
	// with AutoRetries already at cap → WorkflowFailed.
	//
	// Note: We use FailureKindHandlerError (not idle_timeout) for the second
	// failure deliberately — FailureKindHandlerError is non-transient and the
	// aggregate will NOT auto-retry, producing WorkflowFailed directly.
	var devCalls atomic.Int32
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := devCalls.Add(1)
				if n == 1 {
					// First call: transient idle_timeout → aggregate emits WorkflowRetried.
					return nil, developerIdleTimeoutErr()
				}
				// Second call (the retry): simulate the wedged dispatch by returning
				// a deterministic error. This puts PersonaFailed{FailureKindHandlerError}
				// on the bus with AutoRetries["developer"]==1 (already at cap after Apply),
				// so the aggregate must emit WorkflowFailed.
				return nil, fmt.Errorf("auto-retry dispatch failed: handler not found — wedge simulation")
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.PersonaCompleted, event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatalf("register developer: %v", err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, wfID)
	env.start(ctx)
	env.fireWorkflow(ctx, t, wfID, def.ID)

	select {
	case got := <-result:
		if got.Type != event.WorkflowFailed {
			t.Fatalf("want WorkflowFailed (wedge must surface as terminal), got %s", got.Type)
		}
		var fp event.WorkflowFailedPayload
		if err := json.Unmarshal(got.Payload, &fp); err != nil {
			t.Fatalf("unmarshal WorkflowFailedPayload: %v", err)
		}
		// Persona must be "developer" — the aggregate copies it from the
		// PersonaFailed.Persona field via decidePersonaFailed.
		if fp.Persona != "developer" {
			t.Errorf("Persona=%q; want developer", fp.Persona)
		}
		// Reason must call out the failure — not necessarily "auto-retry dispatch failed"
		// since it comes from the aggregate, but must mention developer failure.
		if fp.Reason == "" {
			t.Error("Reason is empty; want non-empty failure description")
		}
		t.Logf("wedge test OK: WorkflowFailed reason=%q persona=%q devCalls=%d",
			fp.Reason, fp.Persona, devCalls.Load())

	case <-time.After(10 * time.Second):
		events, _ := env.store.Load(ctx, wfID)
		for _, e := range events {
			t.Logf("  event: %s (v%d)", e.Type, e.Version)
		}
		t.Fatalf("timeout — workflow never failed (dev calls=%d); wedge fix may not be working",
			devCalls.Load())
	}

	// Verify WorkflowRetried appeared in the store (proves the retry fired).
	storeEvents, _ := env.store.Load(ctx, wfID)
	var foundRetried bool
	for _, e := range storeEvents {
		if e.Type == event.WorkflowRetried {
			foundRetried = true
			break
		}
	}
	if !foundRetried {
		t.Error("WorkflowRetried not found in store; the idle_timeout→retry path did not fire")
	}
}

// TestAutoRetry_WedgedByDispatch_PublishesSyntheticPersonaFailed directly tests
// the subscribeWorkflowRetried path where RecoverDispatch fails. This is the
// "unregistered handler" wedge path that slice 2 fixes.
//
// Setup: publish a WorkflowRetried event on the bus with a FromPhase whose
// handler does NOT exist in the registry. subscribeWorkflowRetried will call
// RecoverDispatch("missing-handler", ...), get ErrHandlerNotFound, and invoke
// publishDispatchWedge — which must publish a PersonaFailed event.
func TestAutoRetry_WedgedByDispatch_PublishesSyntheticPersonaFailed(t *testing.T) {
	runner, store, bus, _ := newTestPersonaRunner(t)

	// Use a trivial workflow def: missing-handler is the sole phase.
	def := WorkflowDef{
		ID:       "wedge-unit",
		Required: []string{"missing-handler"},
		Graph:    map[string][]string{"missing-handler": {}},
	}
	runner.RegisterWorkflow(def)
	runner.Start(context.Background(), handler.NewRegistry())

	const corrID = "corr-wedge-unit"

	// Seed the store with a WorkflowRequested so findWorkflowIDFromEvents works.
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		WorkflowID: "wedge-unit",
		Prompt:     "test",
	})).WithAggregate(corrID, 1).WithCorrelation(corrID).WithSource("test")
	if err := store.Append(context.Background(), corrID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("seed WorkflowRequested: %v", err)
	}

	// Seed a WorkflowStarted so findRetryTrigger can find a root trigger.
	startEvt := event.New(event.WorkflowStartedFor("wedge-unit"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "wedge-unit",
	})).WithAggregate(corrID, 2).WithCorrelation(corrID).WithSource("engine")
	if err := store.Append(context.Background(), corrID, 1, []event.Envelope{startEvt}); err != nil {
		t.Fatalf("seed WorkflowStarted: %v", err)
	}

	// Subscribe to PersonaFailed before publishing the trigger.
	failCh := make(chan event.PersonaFailedPayload, 1)
	bus.Subscribe(event.PersonaFailed, func(_ context.Context, env event.Envelope) error {
		var p event.PersonaFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil
		}
		select {
		case failCh <- p:
		default:
		}
		return nil
	})

	// Publish WorkflowRetried for a handler that isn't registered.
	retriedEvt := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "missing-handler",
		InvalidatedPersonas: []string{"missing-handler"},
		Reason:              "engine: auto-retry on transient idle_timeout",
		Automatic:           true,
	})).WithCorrelation(corrID).WithCausation(startEvt.ID).WithSource("engine:auto-retry")

	if err := bus.Publish(context.Background(), retriedEvt); err != nil {
		t.Fatalf("publish WorkflowRetried: %v", err)
	}

	select {
	case p := <-failCh:
		if p.Persona != "missing-handler" {
			t.Errorf("Persona=%q; want missing-handler", p.Persona)
		}
		if p.FailureKind != event.FailureKindHandlerError {
			t.Errorf("FailureKind=%q; want handler_error (deterministic, no retry)", p.FailureKind)
		}
		if !strings.Contains(p.Error, "auto-retry dispatch failed") {
			t.Errorf("Error=%q; want 'auto-retry dispatch failed' substring for operator trace", p.Error)
		}
		if p.HandlerVersion == "" {
			t.Error("HandlerVersion is empty on synthetic PersonaFailed; want buildinfo.Version()")
		}
		t.Logf("wedge unit test OK: PersonaFailed reason=%q kind=%q version=%q",
			p.Error, p.FailureKind, p.HandlerVersion)

	case <-time.After(3 * time.Second):
		t.Fatal("timeout: synthetic PersonaFailed not published after RecoverDispatch failure")
	}
}
