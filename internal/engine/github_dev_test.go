package engine

import (
	"slices"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

func TestGithubDevWorkflowDef_Shape(t *testing.T) {
	def := GithubDevWorkflowDef()

	if def.ID != "github-dev" {
		t.Errorf("ID=%q, want github-dev", def.ID)
	}

	// github-context must be the sole root (no predecessors).
	deps, ok := def.Graph["github-context"]
	if !ok {
		t.Fatal("graph missing github-context")
	}
	if len(deps) != 0 {
		t.Errorf("github-context should be root, got predecessors %v", deps)
	}

	// workspace depends on github-context — proves issue context flows into
	// workspace provisioning (the whole point of the workflow).
	if got := def.Graph["workspace"]; !slices.Equal(got, []string{"github-context"}) {
		t.Errorf("workspace deps=%v, want [github-context]", got)
	}

	// github-context must be in Required so the workflow actually waits on it.
	if !slices.Contains(def.Required, "github-context") {
		t.Errorf("Required missing github-context: %v", def.Required)
	}

	// Feedback loop: developer must be re-triggerable on FeedbackGenerated.
	retriggers, ok := def.RetriggeredBy["developer"]
	if !ok {
		t.Fatal("RetriggeredBy missing developer")
	}
	if !slices.Contains(retriggers, event.FeedbackGenerated) {
		t.Errorf("developer should retrigger on FeedbackGenerated, got %v", retriggers)
	}
}

func TestGithubDevWorkflowDef_MirrorsJiraDevStructure(t *testing.T) {
	// github-dev is a sibling of jira-dev — same topology, different context
	// source. Catches drift if one workflow evolves without the other.
	gh := GithubDevWorkflowDef()
	jira := JiraDevWorkflowDef()

	if len(gh.Required) != len(jira.Required) {
		t.Errorf("Required length mismatch: github-dev=%d, jira-dev=%d", len(gh.Required), len(jira.Required))
	}
	if len(gh.Graph) != len(jira.Graph) {
		t.Errorf("Graph size mismatch: github-dev=%d, jira-dev=%d", len(gh.Graph), len(jira.Graph))
	}
	if gh.MaxIterations != jira.MaxIterations {
		t.Errorf("MaxIterations mismatch: github-dev=%d, jira-dev=%d", gh.MaxIterations, jira.MaxIterations)
	}
	if gh.EscalateOnMaxIter != jira.EscalateOnMaxIter {
		t.Errorf("EscalateOnMaxIter mismatch")
	}
}
