package cli

import (
	"bytes"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

func TestRootCommand(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"--help"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("root --help: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected help output")
	}
}

func TestRunCommandHelp(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"run", "--help"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	output := buf.String()
	if output == "" {
		t.Error("expected run help output")
	}
}

func TestEventsCommandHelp(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"events", "--help"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("events --help: %v", err)
	}
}

func TestStatusCommandHelp(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"status", "--help"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status --help: %v", err)
	}
}

func TestRunCommandRequiresPromptOrSource(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"run", "--db", ":memory:"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no prompt or source provided")
	}
}

func TestRunCommandUnknownWorkflow(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"run", "--dag", "nonexistent", "--db", ":memory:", "test prompt"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for unknown workflow")
	}
}

func TestRunCommandUnknownBackend(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"run", "--backend", "openai", "--db", ":memory:", "test prompt"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for unknown backend")
	}
}

func TestSelectWorkflowDef(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"develop-only", false},
		{"workspace-dev", false},
		{"pr-review", false},
		{"nonexistent", true},
	}
	for _, tc := range tests {
		def, err := selectWorkflowDef(tc.name)
		if tc.wantErr {
			if err == nil {
				t.Errorf("selectWorkflowDef(%q): want error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("selectWorkflowDef(%q): %v", tc.name, err)
			continue
		}
		if def.ID == "" {
			t.Errorf("selectWorkflowDef(%q): want non-empty ID", tc.name)
		}
	}
}

func TestSelectWorkflowDef_DisableQualityGate(t *testing.T) {
	t.Setenv("RICK_DISABLE_QUALITY_GATE", "1")

	// workspace-dev normally includes quality-gate.
	def, err := selectWorkflowDef("workspace-dev")
	if err != nil {
		t.Fatalf("selectWorkflowDef: %v", err)
	}
	for _, r := range def.Required {
		if r == "quality-gate" {
			t.Error("quality-gate still in Required with RICK_DISABLE_QUALITY_GATE set")
		}
	}
	if _, exists := def.Graph["quality-gate"]; exists {
		t.Error("quality-gate still in Graph with RICK_DISABLE_QUALITY_GATE set")
	}

	// committer should now depend on reviewer + qa.
	deps := def.Graph["committer"]
	depSet := make(map[string]bool, len(deps))
	for _, d := range deps {
		depSet[d] = true
	}
	if !depSet["reviewer"] || !depSet["qa"] {
		t.Errorf("committer deps = %v, want reviewer and qa", deps)
	}
}

func TestMCPCommandHelp(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"mcp", "--help"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("mcp --help: %v", err)
	}
	output := buf.String()
	if output == "" {
		t.Error("expected mcp help output")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("want 'short', got %q", got)
	}
	if got := truncate("this is a long string", 10); got != "this is..." {
		t.Errorf("want 'this is...', got %q", got)
	}
}

func TestServeCommandHelp(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"serve", "--help"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve --help: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected serve help output")
	}
}

func TestSelectWorkflowDef_AllBuiltins(t *testing.T) {
	// Exhaustively verify all registered built-in workflow names return a
	// non-empty WorkflowDef without error.
	builtins := []string{
		"develop-only",
		"workspace-dev",
		"pr-review",
		"pr-feedback",
		"jira-dev",
		"ci-fix",
		"plan-btu",
	}
	for _, name := range builtins {
		t.Run(name, func(t *testing.T) {
			def, err := selectWorkflowDef(name)
			if err != nil {
				t.Fatalf("selectWorkflowDef(%q): unexpected error: %v", name, err)
			}
			if def.ID == "" {
				t.Errorf("selectWorkflowDef(%q): expected non-empty ID", name)
			}
			if len(def.Required) == 0 {
				t.Errorf("selectWorkflowDef(%q): expected at least one required handler", name)
			}
		})
	}
}

func TestSelectWorkflowDef_UnknownWorkflow(t *testing.T) {
	unknowns := []string{"openai-pipeline", "my-custom", ""}
	for _, name := range unknowns {
		t.Run(name, func(t *testing.T) {
			_, err := selectWorkflowDef(name)
			if err == nil {
				t.Errorf("selectWorkflowDef(%q): expected error for unknown workflow", name)
			}
		})
	}
}

// TestDepsWiring verifies that initialising the core dependencies with an
// in-memory database and a valid backend does not panic and produces no error.
// This is a lightweight smoke test for the dependency graph — it does NOT
// start the gRPC listener or engine.
func TestDepsWiring(t *testing.T) {
	// backend.New should succeed for "claude".
	be, err := backend.New("claude")
	if err != nil {
		t.Fatalf("backend.New(claude): %v", err)
	}
	if be == nil {
		t.Fatal("expected non-nil backend")
	}

	// eventstore.NewSQLiteStore should succeed for :memory:.
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("eventstore.NewSQLiteStore(:memory:): %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	defer func() { _ = store.Close() }()
}

func TestDepsWiring_InvalidBackend(t *testing.T) {
	_, err := backend.New("unsupported-llm")
	if err == nil {
		t.Fatal("expected error for unsupported backend, got nil")
	}
}
