package persona

import (
	"strings"
	"testing"
)

// TestBuild_DevelopPhase_WorkspaceSection verifies that the develop phase
// prompt injects an explicit Workspace Constraints block whenever the
// PromptContext carries a WorkspacePath, and omits it for prompt-only flows.
//
// Regression guard for HULI-33546: without these constraints the LLM was free
// to interpret a repo name in the task text as the operator's canonical
// checkout and write commits to the wrong tree.
func TestBuild_DevelopPhase_WorkspaceSection(t *testing.T) {
	pb := NewPromptBuilder()

	t.Run("with workspace path renders constraints section", func(t *testing.T) {
		out, err := pb.Build("develop", PromptContext{
			Task:          "Implement feature X on hulihealth-web",
			WorkspacePath: "/tmp/repos/hulihealth-web-HULI-X-abc123",
			Ticket:        "HULI-X",
			BaseBranch:    "main",
		})
		if err != nil {
			t.Fatalf("Build failed: %v", err)
		}
		for _, want := range []string{
			"Workspace Constraints",
			"/tmp/repos/hulihealth-web-HULI-X-abc123",
			"HULI-X",
			"from `main`",
			"Do NOT `cd` out",
			"STOP IMMEDIATELY",
			".rick/workspace.yaml",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("expected develop prompt to contain %q\nfull prompt:\n%s", want, out)
			}
		}
	})

	t.Run("without workspace path omits constraints section", func(t *testing.T) {
		out, err := pb.Build("develop", PromptContext{Task: "do a thing"})
		if err != nil {
			t.Fatalf("Build failed: %v", err)
		}
		if strings.Contains(out, "Workspace Constraints") {
			t.Errorf("prompt-only flow should not render workspace section\nfull prompt:\n%s", out)
		}
	})
}

// TestBuild_CommitPhase_WorkspaceSection verifies the commit phase carries the
// same workspace pin. The commit phase runs `git push` — another cd-out vector
// — so it must be guarded with the identical block.
func TestBuild_CommitPhase_WorkspaceSection(t *testing.T) {
	pb := NewPromptBuilder()

	t.Run("with workspace path renders constraints section", func(t *testing.T) {
		out, err := pb.Build("commit", PromptContext{
			Task:          "ship it",
			WorkspacePath: "/tmp/repos/myapp-PROJ-1-deadbeef",
			Ticket:        "PROJ-1",
			BaseBranch:    "main",
			Outputs:       map[string]string{"develop": "stub"},
		})
		if err != nil {
			t.Fatalf("Build failed: %v", err)
		}
		for _, want := range []string{
			"Workspace Constraints",
			"/tmp/repos/myapp-PROJ-1-deadbeef",
			"PROJ-1",
			"Do NOT `cd` out",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("expected commit prompt to contain %q\nfull prompt:\n%s", want, out)
			}
		}
	})

	t.Run("without workspace path omits constraints section", func(t *testing.T) {
		out, err := pb.Build("commit", PromptContext{
			Task:    "ship it",
			Ticket:  "PROJ-1",
			Outputs: map[string]string{"develop": "stub"},
		})
		if err != nil {
			t.Fatalf("Build failed: %v", err)
		}
		if strings.Contains(out, "Workspace Constraints") {
			t.Errorf("prompt-only commit should not render workspace section\nfull prompt:\n%s", out)
		}
	})
}
