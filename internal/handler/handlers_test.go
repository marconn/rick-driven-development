package handler

import (
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

func testDeps() Deps {
	return Deps{
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "ok", Duration: time.Second}},
		Store:    newMockStore(),
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	}
}

func TestAIHandlerResearcher(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "researcher", Phase: "research", Persona: "researcher",
		Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
	})
	if h.Name() != "researcher" {
		t.Errorf("want name 'researcher', got %q", h.Name())
	}
}

func TestAIHandlerArchitect(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "architect", Phase: "architect", Persona: "architect",
		Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
	})
	if h.Name() != "architect" {
		t.Errorf("want name 'architect', got %q", h.Name())
	}
}

func TestAIHandlerDeveloper(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "developer", Phase: "develop", Persona: "developer",
		Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
	})
	if h.Name() != "developer" {
		t.Errorf("want name 'developer', got %q", h.Name())
	}
}

func TestReviewHandlerReviewer(t *testing.T) {
	d := testDeps()
	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name: "reviewer", Phase: "review", Persona: "reviewer",
			Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
		},
		TargetPhase: "develop",
	})
	if h.Name() != "reviewer" {
		t.Errorf("want name 'reviewer', got %q", h.Name())
	}
}

func TestReviewHandlerQA(t *testing.T) {
	d := testDeps()
	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name: "qa", Phase: "qa", Persona: "qa",
			Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
		},
		TargetPhase: "develop",
	})
	if h.Name() != "qa" {
		t.Errorf("want name 'qa', got %q", h.Name())
	}
}

func TestAIHandlerCommitter(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "committer", Phase: "commit", Persona: "committer",
		Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
	})
	if h.Name() != "committer" {
		t.Errorf("want name 'committer', got %q", h.Name())
	}
}

func TestRegisterAll(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterAll(reg, testDeps()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	expected := []string{"researcher", "architect", "developer", "reviewer", "qa", "committer"}
	for _, name := range expected {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("handler %q not registered", name)
		}
	}
}

// TestRegisterAllNoGithubFetcher verifies that when Deps has no GitHub client configured
// (which is the current state — Deps does not have a GitHub field), the "github-fetcher"
// handler is NOT registered. This documents the conditional registration contract:
// github-fetcher should only appear when a GitHub integration is wired in.
func TestRegisterAllNoGithubFetcher(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterAll(reg, testDeps()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if _, ok := reg.Get("github-fetcher"); ok {
		t.Error("github-fetcher should NOT be registered when no GitHub client is configured")
	}
}

func TestRegisterAllIncludesQAStepsHandlers(t *testing.T) {
	// qa-context and qa-jira-writer are registered via RegisterAll as part of the
	// jira-qa-steps workflow. Verify they are present and no github-fetcher is added
	// when no GitHub client is configured.
	reg := NewRegistry()
	if err := RegisterAll(reg, testDeps()); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	for _, name := range []string{"qa-context", "qa-jira-writer", "qa-analyzer"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("handler %q not registered by RegisterAll", name)
		}
	}
	if _, ok := reg.Get("github-fetcher"); ok {
		t.Error("github-fetcher should NOT be registered when no GitHub client is configured")
	}
}

func TestRegisterAllDuplicate(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterAll(reg, testDeps()); err != nil {
		t.Fatalf("first RegisterAll: %v", err)
	}
	if err := RegisterAll(reg, testDeps()); err == nil {
		t.Error("expected error on duplicate RegisterAll")
	}
}
