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
		Name: "researcher", Persona: "researcher",
		Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
	})
	if h.Name() != "researcher" {
		t.Errorf("want name 'researcher', got %q", h.Name())
	}
}

func TestAIHandlerArchitect(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "architect", Persona: "architect",
		Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
	})
	if h.Name() != "architect" {
		t.Errorf("want name 'architect', got %q", h.Name())
	}
}

func TestAIHandlerDeveloper(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "developer", Persona: "developer",
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
			Name: "reviewer", Persona: "reviewer",
			Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
		},
		TargetPersona: "developer",
	})
	if h.Name() != "reviewer" {
		t.Errorf("want name 'reviewer', got %q", h.Name())
	}
}

func TestReviewHandlerQA(t *testing.T) {
	d := testDeps()
	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name: "qa", Persona: "qa",
			Backend: d.Backend, Store: d.Store, Personas: d.Personas, Builder: d.Builder,
		},
		TargetPersona: "developer",
	})
	if h.Name() != "qa" {
		t.Errorf("want name 'qa', got %q", h.Name())
	}
}

func TestAIHandlerCommitter(t *testing.T) {
	d := testDeps()
	h := NewAIHandler(AIHandlerConfig{
		Name: "committer", Persona: "committer",
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

// TestPRReviewerSafetyConfigUnchanged is the migration safety net for task
// 0009: moving the 13 category reviewers to manifests must NOT change their
// safety configuration. Verdict-bearing reviewers stay PlainText=true (so
// ExtractJSON cannot steal an in-prose JSON snippet and drop the VERDICT tail —
// the default-optimistic-pass class). Persona manifests own prompt composition
// ONLY; this safety knob is code-owned and independent of the migration.
func TestPRReviewerSafetyConfigUnchanged(t *testing.T) {
	prReviewers := []string{
		"pr-security", "pr-concurrency", "pr-error-handling", "pr-observability",
		"pr-api-contract", "pr-idempotency", "pr-testing", "pr-integration",
		"pr-performance", "pr-data", "pr-hygiene", "pr-vendor-resilience",
		"pr-docs-concordance", "pr-correctness",
	}
	for _, name := range prReviewers {
		if !isVerdictBearingReviewer(name) {
			t.Errorf("%s must remain verdict-bearing (PlainText=true) after the manifest migration", name)
		}
	}
	// reviewer/qa are also verdict-bearing; pr-consolidator/pr-replier are not.
	if !isVerdictBearingReviewer("reviewer") || !isVerdictBearingReviewer("qa") {
		t.Error("reviewer/qa must stay verdict-bearing")
	}
	if isVerdictBearingReviewer("pr-consolidator") || isVerdictBearingReviewer("pr-replier") {
		t.Error("pr-consolidator/pr-replier must NOT be verdict-bearing")
	}
}
