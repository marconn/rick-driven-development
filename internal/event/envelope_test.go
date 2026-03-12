package event

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	id1 := NewID()
	id2 := NewID()
	if id1 == id2 {
		t.Error("NewID should generate unique IDs")
	}
	if id1 == "" {
		t.Error("NewID should not be empty")
	}
}

func TestNew(t *testing.T) {
	payload := MustMarshal(WorkflowRequestedPayload{
		Prompt:     "build an API",
		WorkflowID: "workspace-dev",
		Source:     "raw",
	})

	env := New(WorkflowRequested, 1, payload)

	if env.ID == "" {
		t.Error("envelope should have an ID")
	}
	if env.Type != WorkflowRequested {
		t.Errorf("expected type %s, got %s", WorkflowRequested, env.Type)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("expected schema version 1, got %d", env.SchemaVersion)
	}
	if env.Timestamp.IsZero() {
		t.Error("timestamp should not be zero")
	}
	if time.Since(env.Timestamp) > time.Second {
		t.Error("timestamp should be recent")
	}
}

func TestEnvelopeBuilders(t *testing.T) {
	env := New(PersonaCompleted, 1, json.RawMessage(`{}`)).
		WithAggregate("wf-123", 5).
		WithCausation("cause-456").
		WithCorrelation("corr-789").
		WithSource("handler:developer")

	if env.AggregateID != "wf-123" {
		t.Errorf("expected aggregate wf-123, got %s", env.AggregateID)
	}
	if env.Version != 5 {
		t.Errorf("expected version 5, got %d", env.Version)
	}
	if env.CausationID != "cause-456" {
		t.Errorf("expected causation cause-456, got %s", env.CausationID)
	}
	if env.CorrelationID != "corr-789" {
		t.Errorf("expected correlation corr-789, got %s", env.CorrelationID)
	}
	if env.Source != "handler:developer" {
		t.Errorf("expected source handler:developer, got %s", env.Source)
	}
}

func TestMustMarshal(t *testing.T) {
	payload := MustMarshal(map[string]string{"key": "value"})
	var result map[string]string
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["key"] != "value" {
		t.Errorf("expected value, got %s", result["key"])
	}
}

func TestMustMarshalPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for unmarshalable value")
		}
	}()
	MustMarshal(make(chan int)) // channels can't be marshaled
}

// --- IsWorkflowStarted ---

func TestIsWorkflowStarted(t *testing.T) {
	tests := []struct {
		input Type
		want  bool
	}{
		{WorkflowStarted, true},
		{"workflow.started.default", true},
		{"workflow.started.jira-dev", true},
		{"workflow.started.some-other-workflow", true},
		{"persona.completed", false},
		{"", false},
		// No false positive on a type that starts with the same chars but isn't the prefix.
		{"workflow.startedx", false},
		{"workflow.started", true}, // exact base type is true
	}

	for _, tc := range tests {
		t.Run(string(tc.input), func(t *testing.T) {
			got := IsWorkflowStarted(tc.input)
			if got != tc.want {
				t.Errorf("IsWorkflowStarted(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// --- WorkflowStartedFor ---

func TestWorkflowStartedFor(t *testing.T) {
	tests := []struct {
		workflowID string
		want       Type
	}{
		{"workspace-dev", "workflow.started.workspace-dev"},
		{"jira-dev", "workflow.started.jira-dev"},
		{"", "workflow.started."},
	}

	for _, tc := range tests {
		t.Run(tc.workflowID, func(t *testing.T) {
			got := WorkflowStartedFor(tc.workflowID)
			if got != tc.want {
				t.Errorf("WorkflowStartedFor(%q) = %q, want %q", tc.workflowID, got, tc.want)
			}
		})
	}
}

// WorkflowStartedFor results must satisfy IsWorkflowStarted.
func TestWorkflowStartedFor_SatisfiesIsWorkflowStarted(t *testing.T) {
	for _, id := range []string{"workspace-dev", "jira-dev", "pr-review", ""} {
		typ := WorkflowStartedFor(id)
		if !IsWorkflowStarted(typ) {
			t.Errorf("IsWorkflowStarted(WorkflowStartedFor(%q)) = false, want true", id)
		}
	}
}
