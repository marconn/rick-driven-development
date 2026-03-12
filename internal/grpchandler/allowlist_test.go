package grpchandler

import (
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

func TestIsInjectable(t *testing.T) {
	t.Helper()

	allowed := []event.Type{
		event.WorkflowRequested,
		event.WorkflowCancelled,
		event.WorkflowPaused,
		event.WorkflowResumed,
		event.OperatorGuidance,
		event.ContextEnrichment,
		event.ContextCodebase,
		event.ContextSchema,
		event.ContextGit,
		event.ChildWorkflowCompleted,
	}

	blocked := []event.Type{
		event.PersonaCompleted,
		event.PersonaFailed,
		event.FeedbackGenerated,
		event.VerdictRendered,
		event.WorkflowStarted,
		event.WorkflowCompleted,
		event.WorkflowFailed,
		event.TokenBudgetExceeded,
		event.AIRequestSent,
		event.AIResponseReceived,
		event.AIStructuredOutput,
		event.FeedbackConsumed,
		event.CompensationStarted,
		event.CompensationCompleted,
		event.WorkspaceReady,
	}

	for _, et := range allowed {
		if !IsInjectable(et) {
			t.Errorf("expected %q to be injectable", et)
		}
	}
	for _, et := range blocked {
		if IsInjectable(et) {
			t.Errorf("expected %q to be blocked", et)
		}
	}
}
