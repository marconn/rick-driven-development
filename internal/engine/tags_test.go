package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// TestE2ETagLookupByJiraTicket verifies that an external system can discover
// a workflow's correlation ID by looking up its Jira ticket tag.
func TestE2ETagLookupByJiraTicket(t *testing.T) {
	def := WorkflowDef{ID: "workspace-dev", Required: []string{"alpha"}, MaxIterations: 3}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("workspace-dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-tags")
	env.start(ctx)

	// Fire a workflow with business keys.
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "Fix NULL scan error",
		WorkflowID: "workspace-dev",
		Source:     "jira:PROJ-123",
		Ticket:     "PROJ-123",
		Repo:       "acme/myapp",
		BaseBranch: "main",
	})).
		WithAggregate("wf-tags", 1).
		WithCorrelation("wf-tags").
		WithSource("test")

	if err := env.store.Append(ctx, "wf-tags", 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := env.bus.Publish(ctx, reqEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	// External system looks up by Jira ticket.
	ids, err := env.store.LoadByTag(ctx, "ticket", "PROJ-123")
	if err != nil {
		t.Fatalf("LoadByTag: %v", err)
	}
	if len(ids) != 1 || ids[0] != "wf-tags" {
		t.Errorf("expected [wf-tags], got %v", ids)
	}

	// Look up by source.
	ids, err = env.store.LoadByTag(ctx, "source", "jira:PROJ-123")
	if err != nil {
		t.Fatalf("LoadByTag source: %v", err)
	}
	if len(ids) != 1 || ids[0] != "wf-tags" {
		t.Errorf("expected [wf-tags], got %v", ids)
	}

	// Look up by repo.
	ids, err = env.store.LoadByTag(ctx, "repo", "acme/myapp")
	if err != nil {
		t.Fatalf("LoadByTag repo: %v", err)
	}
	if len(ids) != 1 || ids[0] != "wf-tags" {
		t.Errorf("expected [wf-tags], got %v", ids)
	}

	// Look up by repo+branch composite.
	ids, err = env.store.LoadByTag(ctx, "repo_branch", "acme/myapp:main")
	if err != nil {
		t.Fatalf("LoadByTag repo_branch: %v", err)
	}
	if len(ids) != 1 || ids[0] != "wf-tags" {
		t.Errorf("expected [wf-tags], got %v", ids)
	}

	// Look up by non-existent tag returns empty.
	ids, err = env.store.LoadByTag(ctx, "ticket", "PROJ-999")
	if err != nil {
		t.Fatalf("LoadByTag missing: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

// TestE2ETagMultipleWorkflowsSameTicket verifies that multiple workflow runs
// for the same ticket are all discoverable via the tag.
func TestE2ETagMultipleWorkflowsSameTicket(t *testing.T) {
	def := WorkflowDef{ID: "workspace-dev", Required: []string{"alpha"}, MaxIterations: 3}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("workspace-dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	env.start(ctx)

	// Fire two workflows for the same ticket.
	for _, wfID := range []string{"wf-run-1", "wf-run-2"} {
		result := awaitWorkflowResult(t, env.bus, wfID)
		reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "Fix bug",
			WorkflowID: "workspace-dev",
			Source:     "jira:BUG-42",
			Ticket:     "BUG-42",
		})).
			WithAggregate(wfID, 1).
			WithCorrelation(wfID).
			WithSource("test")

		if err := env.store.Append(ctx, wfID, 0, []event.Envelope{reqEvt}); err != nil {
			t.Fatalf("append %s: %v", wfID, err)
		}
		if err := env.bus.Publish(ctx, reqEvt); err != nil {
			t.Fatalf("publish %s: %v", wfID, err)
		}

		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout %s", wfID)
		}
	}

	// Both runs should be discoverable by ticket.
	ids, err := env.store.LoadByTag(ctx, "ticket", "BUG-42")
	if err != nil {
		t.Fatalf("LoadByTag: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 correlations, got %d: %v", len(ids), ids)
	}
}

// TestTagsStoreUnit tests SaveTags and LoadByTag at the store level.
func TestTagsStoreUnit(t *testing.T) {
	env := newE2EEnv(t, WorkflowDef{ID: "x", Required: []string{"a"}, MaxIterations: 1})
	ctx := context.Background()

	// Save tags for two correlations.
	if err := env.store.SaveTags(ctx, "corr-1", map[string]string{
		"ticket": "PROJ-1",
		"repo":   "org/repo",
	}); err != nil {
		t.Fatalf("SaveTags corr-1: %v", err)
	}
	if err := env.store.SaveTags(ctx, "corr-2", map[string]string{
		"ticket": "PROJ-1", // same ticket, different correlation
		"repo":   "org/other",
	}); err != nil {
		t.Fatalf("SaveTags corr-2: %v", err)
	}

	// Lookup by shared ticket.
	ids, _ := env.store.LoadByTag(ctx, "ticket", "PROJ-1")
	if len(ids) != 2 {
		t.Errorf("ticket PROJ-1: expected 2 correlations, got %d", len(ids))
	}

	// Lookup by unique repo.
	ids, _ = env.store.LoadByTag(ctx, "repo", "org/other")
	if len(ids) != 1 || ids[0] != "corr-2" {
		t.Errorf("repo org/other: expected [corr-2], got %v", ids)
	}

	// SaveTags is idempotent (UNIQUE constraint with OR IGNORE).
	if err := env.store.SaveTags(ctx, "corr-1", map[string]string{
		"ticket": "PROJ-1",
	}); err != nil {
		t.Fatalf("SaveTags idempotent: %v", err)
	}
	ids, _ = env.store.LoadByTag(ctx, "ticket", "PROJ-1")
	if len(ids) != 2 {
		t.Errorf("idempotent: expected still 2, got %d", len(ids))
	}

	// Empty tags is a no-op.
	if err := env.store.SaveTags(ctx, "corr-3", nil); err != nil {
		t.Fatalf("SaveTags empty: %v", err)
	}
}
