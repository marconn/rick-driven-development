package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

func TestDefaultRegistry(t *testing.T) {
	r := DefaultRegistry()
	names := r.Names()
	if len(names) != 11 {
		t.Fatalf("want 11 personas, got %d: %v", len(names), names)
	}
	// Names are returned sorted — verify all expected personas are present.
	want := []string{Architect, Committer, ContextSnapshot, Developer, FeedbackAnalyzer, PRConsolidator, QA, QAAnalyzer, Researcher, Reviewer, Workspace}
	for i, name := range want {
		if names[i] != name {
			t.Errorf("names[%d]: want %q, got %q", i, name, names[i])
		}
	}
}

func TestRegistryGet(t *testing.T) {
	r := DefaultRegistry()

	t.Run("existing", func(t *testing.T) {
		p, err := r.Get(Researcher)
		if err != nil {
			t.Fatalf("Get(researcher): %v", err)
		}
		if p.Name != Researcher {
			t.Errorf("want name %q, got %q", Researcher, p.Name)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		_, err := r.Get("nonexistent")
		if err == nil {
			t.Fatal("want error for unknown persona")
		}
	})
}

func TestRegistryRegister(t *testing.T) {
	r := NewRegistry()

	t.Run("success", func(t *testing.T) {
		err := r.Register(&Persona{Name: "custom", Description: "test"})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		p, err := r.Get("custom")
		if err != nil {
			t.Fatalf("Get(custom): %v", err)
		}
		if p.Description != "test" {
			t.Errorf("want description %q, got %q", "test", p.Description)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		err := r.Register(&Persona{Name: "custom", Description: "dup"})
		if err == nil {
			t.Fatal("want error for duplicate registration")
		}
	})
}

// ---------------------------------------------------------------------------
// System prompt loading
// ---------------------------------------------------------------------------

func TestLoadSystemPrompt(t *testing.T) {
	r := DefaultRegistry()

	for _, name := range []string{Researcher, Architect, Developer, Reviewer, QA, Committer, FeedbackAnalyzer} {
		t.Run(name, func(t *testing.T) {
			prompt, err := r.LoadSystemPrompt(name)
			if err != nil {
				t.Fatalf("LoadSystemPrompt(%s): %v", name, err)
			}
			if len(prompt) < 100 {
				t.Errorf("prompt too short (%d bytes)", len(prompt))
			}
			// Each prompt should mention "Rick" somewhere.
			if !strings.Contains(prompt, "Rick") {
				t.Error("prompt should contain 'Rick'")
			}
		})
	}
}

func TestLoadSystemPromptUnknown(t *testing.T) {
	r := DefaultRegistry()
	_, err := r.LoadSystemPrompt("nonexistent")
	if err == nil {
		t.Fatal("want error for unknown persona")
	}
}

func TestLoadSystemPromptCustomDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "researcher.md"), []byte("Custom researcher."), 0o644); err != nil {
		t.Fatal(err)
	}

	r := DefaultRegistry()
	r.SetCustomDir(dir)

	t.Run("overridden", func(t *testing.T) {
		prompt, err := r.LoadSystemPrompt(Researcher)
		if err != nil {
			t.Fatalf("LoadSystemPrompt: %v", err)
		}
		if prompt != "Custom researcher." {
			t.Errorf("want custom prompt, got %q", prompt)
		}
	})

	t.Run("fallback_to_embedded", func(t *testing.T) {
		// Architect has no custom override, should fall back to embedded.
		prompt, err := r.LoadSystemPrompt(Architect)
		if err != nil {
			t.Fatalf("LoadSystemPrompt: %v", err)
		}
		if !strings.Contains(prompt, "Multi-Dimensional Architect") {
			t.Error("expected embedded architect prompt")
		}
	})
}

// ---------------------------------------------------------------------------
// Phase persona mapping
// ---------------------------------------------------------------------------

func TestPhasePersonaMapping(t *testing.T) {
	expected := map[string]string{
		"research":  Researcher,
		"architect": Architect,
		"develop":   Developer,
		"review":    Reviewer,
		"qa":        QA,
		"commit":    Committer,
	}
	for phase, persona := range expected {
		if got := PhasePersona[phase]; got != persona {
			t.Errorf("PhasePersona[%s]: want %q, got %q", phase, persona, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Prompt building
// ---------------------------------------------------------------------------

func TestPromptBuilderResearch(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a REST API for user management",
	}
	prompt, err := b.Build("research", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "REST API for user management") {
		t.Error("prompt should contain the task")
	}
	if !strings.Contains(prompt, "Research") {
		t.Error("prompt should reference the Research phase")
	}
}

func TestPromptBuilderArchitect(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a REST API",
		Outputs: map[string]string{
			"research": "Domain analysis: User entity with CRUD operations.",
		},
	}
	prompt, err := b.Build("architect", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "REST API") {
		t.Error("prompt should contain the task")
	}
	if !strings.Contains(prompt, "Domain analysis") {
		t.Error("prompt should contain research findings")
	}
}

func TestPromptBuilderDevelop(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a REST API",
		Outputs: map[string]string{
			"architect": "Use Go with chi router.",
		},
	}
	prompt, err := b.Build("develop", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "chi router") {
		t.Error("prompt should contain architecture")
	}
}

func TestPromptBuilderDevelopWithFeedback(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a REST API",
		Outputs: map[string]string{
			"architect": "Use Go with chi router.",
			"develop":   "Previous implementation...",
		},
		Feedback:  "1. Missing error handling in handler\n2. No input validation",
		Iteration: 1,
	}
	prompt, err := b.Build("develop", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "Missing error handling") {
		t.Error("prompt should contain feedback")
	}
	if !strings.Contains(prompt, "Previous implementation") {
		t.Error("prompt should contain previous develop output")
	}
}

func TestPromptBuilderReview(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a REST API",
		Outputs: map[string]string{
			"architect": "Architecture plan.",
			"develop":   "Implementation code.",
		},
	}
	prompt, err := b.Build("review", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "VERDICT: PASS") {
		t.Error("review prompt should mention VERDICT format")
	}
	if !strings.Contains(prompt, "Implementation code") {
		t.Error("prompt should contain develop output")
	}
}

func TestPromptBuilderCommit(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a REST API",
		Outputs: map[string]string{
			"develop": "Implementation changes.",
		},
		Ticket:     "PROJ-123",
		BaseBranch: "main",
	}
	prompt, err := b.Build("commit", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "PROJ-123") {
		t.Error("prompt should contain ticket")
	}
	if !strings.Contains(prompt, "main") {
		t.Error("prompt should contain base branch")
	}
}

func TestPromptBuilderUnknownPhase(t *testing.T) {
	b := NewPromptBuilder()
	_, err := b.Build("nonexistent", PromptContext{})
	if err == nil {
		t.Fatal("want error for unknown phase")
	}
}

func TestPromptBuilderCustomDir(t *testing.T) {
	dir := t.TempDir()
	phasesDir := filepath.Join(dir, "phases")
	if err := os.MkdirAll(phasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(phasesDir, "research.md"), []byte("Custom: {{.Source}}"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewPromptBuilder()
	b.SetCustomDir(dir)

	t.Run("overridden", func(t *testing.T) {
		prompt, err := b.Build("research", PromptContext{Task: "test task"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if prompt != "Custom: test task" {
			t.Errorf("want custom prompt, got %q", prompt)
		}
	})

	t.Run("fallback_to_embedded", func(t *testing.T) {
		// review has no custom override.
		prompt, err := b.Build("review", PromptContext{
			Task:    "task",
			Outputs: map[string]string{"architect": "arch", "develop": "dev"},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !strings.Contains(prompt, "VERDICT") {
			t.Error("expected embedded review template")
		}
	})
}

func TestPromptBuilderWithEnrichments(t *testing.T) {
	b := NewPromptBuilder()
	ctx := PromptContext{
		Task: "Build a dashboard",
		Outputs: map[string]string{
			"architect": "Use React + TypeScript for the dashboard.",
		},
		Enrichments: []event.ContextEnrichmentPayload{
			{
				Source:  "frontend-enricher",
				Kind:    "libraries",
				Summary: "Recommended libraries for React dashboard",
				Items: []event.EnrichmentItem{
					{Name: "tanstack-table", Version: "^8.0.0", Reason: "data grid support", ImportPath: "@tanstack/react-table"},
					{Name: "recharts", Version: "^2.0.0", Reason: "chart rendering"},
					{Name: "shadcn/ui", Reason: "accessible UI primitives", DocURL: "https://ui.shadcn.com"},
				},
			},
		},
	}
	prompt, err := b.Build("develop", ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "tanstack-table") {
		t.Error("prompt should contain enrichment library name")
	}
	if !strings.Contains(prompt, "@tanstack/react-table") {
		t.Error("prompt should contain import path")
	}
	if !strings.Contains(prompt, "data grid support") {
		t.Error("prompt should contain enrichment reason")
	}
	if !strings.Contains(prompt, "shadcn/ui") {
		t.Error("prompt should contain all enrichment items")
	}
	if !strings.Contains(prompt, "https://ui.shadcn.com") {
		t.Error("prompt should contain doc URL")
	}
}

func TestFormatEnrichmentsEmpty(t *testing.T) {
	result := formatEnrichments(nil)
	if result != "" {
		t.Errorf("expected empty string for nil enrichments, got %q", result)
	}
}

func TestPromptBuilderNilOutputs(t *testing.T) {
	b := NewPromptBuilder()
	// Should not panic with nil Outputs map.
	prompt, err := b.Build("research", PromptContext{Task: "test"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prompt, "test") {
		t.Error("prompt should contain task")
	}
}
