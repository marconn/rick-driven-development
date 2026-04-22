package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/persona"
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

func TestPRFeedbackWorkflowDef_IncludesQualityGate(t *testing.T) {
	def, err := selectWorkflowDef("pr-feedback")
	if err != nil {
		t.Fatalf("selectWorkflowDef: %v", err)
	}

	// qa and quality-gate must be in Required.
	reqSet := make(map[string]bool, len(def.Required))
	for _, r := range def.Required {
		reqSet[r] = true
	}
	if !reqSet["qa"] {
		t.Error("qa missing from Required")
	}
	if !reqSet["quality-gate"] {
		t.Error("quality-gate missing from Required")
	}

	// quality-gate must depend on reviewer + qa.
	qgDeps := def.Graph["quality-gate"]
	depSet := make(map[string]bool, len(qgDeps))
	for _, d := range qgDeps {
		depSet[d] = true
	}
	if !depSet["reviewer"] || !depSet["qa"] {
		t.Errorf("quality-gate deps = %v, want [reviewer, qa]", qgDeps)
	}

	// committer must depend on quality-gate.
	committerDeps := def.Graph["committer"]
	if len(committerDeps) != 1 || committerDeps[0] != "quality-gate" {
		t.Errorf("committer deps = %v, want [quality-gate]", committerDeps)
	}
}

func TestPRFeedbackWorkflowDef_DisableQualityGate(t *testing.T) {
	t.Setenv("RICK_DISABLE_QUALITY_GATE", "1")

	def, err := selectWorkflowDef("pr-feedback")
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

	// committer should now depend on reviewer + qa directly.
	deps := def.Graph["committer"]
	depSet := make(map[string]bool, len(deps))
	for _, d := range deps {
		depSet[d] = true
	}
	if !depSet["reviewer"] || !depSet["qa"] {
		t.Errorf("committer deps = %v, want reviewer and qa", deps)
	}
}

// Regression test for the hulilabs/huli#689 duplicate-comment incident (2026-04-17).
// Rick must own PR comment posting after the committer — the pr-feedback DAG
// therefore terminates in a single composer→poster pair, and both must be in
// Required so WorkflowCompleted cannot fire until the post is done.
//
// The pr-summarizer/pr-summary-poster pair was removed because it produced a
// noise-only "Rick pr-feedback run complete" comment after the substantive
// pr-replier reply. GitHub already shows the pushed commits. If this test ever
// re-introduces summary checks, re-read the rationale before adding handlers.
func TestPRFeedbackWorkflowDef_RickOwnedCommentPosters(t *testing.T) {
	def, err := selectWorkflowDef("pr-feedback")
	if err != nil {
		t.Fatalf("selectWorkflowDef: %v", err)
	}

	required := map[string]bool{}
	for _, r := range def.Required {
		required[r] = true
	}
	for _, name := range []string{"pr-replier", "pr-reply-poster"} {
		if !required[name] {
			t.Errorf("%q missing from Required — WorkflowCompleted would fire before the comment is posted", name)
		}
	}
	// Guard against accidental re-introduction of the summary pair.
	for _, name := range []string{"pr-summarizer", "pr-summary-poster"} {
		if required[name] {
			t.Errorf("%q is back in Required — the noise-only summary comment was intentionally removed", name)
		}
		if _, ok := def.Graph[name]; ok {
			t.Errorf("%q is back in Graph — see TestPRFeedbackWorkflowDef_RickOwnedCommentPosters comment", name)
		}
	}

	// Strict ordering: committer → pr-replier → pr-reply-poster.
	// Each must have exactly the one predecessor; any drift lets posts race or fire early.
	checks := []struct {
		handler string
		want    string
	}{
		{"pr-replier", "committer"},
		{"pr-reply-poster", "pr-replier"},
	}
	for _, c := range checks {
		deps := def.Graph[c.handler]
		if len(deps) != 1 || deps[0] != c.want {
			t.Errorf("%s deps = %v, want [%s]", c.handler, deps, c.want)
		}
	}

	// PhaseMap must resolve the reply phase so verdict-to-handler lookups work
	// if a reviewer ever targets pr-reply. Absent entry would silently fall
	// through to handler==phase which doesn't match.
	if got := def.PhaseMap["pr-reply"]; got != "pr-replier" {
		t.Errorf("PhaseMap[pr-reply] = %q, want pr-replier", got)
	}
}

// Regression test for the hulilabs/huli#689 duplicate-comment incident.
// The committer LLM must never be instructed to post PR comments. Two
// properties must hold:
//  1. The actionable "run gh pr comment" instruction is gone (it was present
//     as `gh pr comment {{.Ticket}} --body "<summary>"` before the fix).
//  2. An explicit prohibition is present ("Never run `gh pr comment`...").
//
// If (1) regresses, the LLM gets a documented path to post comments itself,
// reintroducing the duplicate-post failure mode.
// If (2) regresses, the prohibition has been weakened and the LLM may
// fall back to skill-driven posting (pr-feedback skill Phase 4).
func TestCommitterPromptsProhibitGhPrComment(t *testing.T) {
	reg := persona.DefaultRegistry()
	sys, err := reg.LoadSystemPrompt(persona.Committer)
	if err != nil {
		t.Fatalf("load committer system prompt: %v", err)
	}

	builder := persona.NewPromptBuilder()
	userPrompt, err := builder.Build("commit", persona.PromptContext{
		Task: "test", Ticket: "TEST-1", BaseBranch: "main",
		WorkspacePath: "/tmp/wsp",
	})
	if err != nil {
		t.Fatalf("build commit phase prompt: %v", err)
	}

	// Actionable invocation patterns that used to live in the prompt. These
	// are the exact strings that told the LLM to post a comment.
	badPatterns := []string{
		"gh pr comment <branch>",
		"gh pr comment {{.Ticket}}",
		"post a comment summarizing what was addressed",
		"post a comment summarizing",
	}
	for _, pat := range badPatterns {
		if strings.Contains(sys, pat) {
			t.Errorf("committer system prompt still contains actionable instruction %q", pat)
		}
		if strings.Contains(userPrompt, pat) {
			t.Errorf("commit phase prompt still contains actionable instruction %q", pat)
		}
	}

	// Explicit prohibitions must be present — otherwise the LLM can fall
	// back to skill-driven posting (e.g., ~/.claude/skills/pr-feedback).
	if !strings.Contains(sys, "Never run `gh pr comment`") {
		t.Error("committer system prompt must contain 'Never run `gh pr comment`' prohibition")
	}
	if !strings.Contains(userPrompt, "do NOT run `gh pr comment`") {
		t.Error("commit phase prompt must contain 'do NOT run `gh pr comment`' prohibition")
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
