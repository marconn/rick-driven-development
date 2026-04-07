package engine

import (
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestDevelopOnlyWorkflowDefRetriggeredBy verifies that DevelopOnlyWorkflowDef
// includes RetriggeredBy["developer"] = [FeedbackGenerated]. Without this,
// the aggregate emits FeedbackGenerated after a committer VerdictRendered{fail}
// but the PersonaRunner finds no retrigger entry and the developer never re-fires,
// deadlocking the workflow (correlation 8af108dd-70eb-44c1-a7ae-44c1c6350911).
func TestDevelopOnlyWorkflowDefRetriggeredBy(t *testing.T) {
	def := DevelopOnlyWorkflowDef()

	retriggers, ok := def.RetriggeredBy["developer"]
	if !ok {
		t.Fatal("DevelopOnlyWorkflowDef.RetriggeredBy must contain an entry for 'developer'")
	}

	found := false
	for _, et := range retriggers {
		if et == event.FeedbackGenerated {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DevelopOnlyWorkflowDef.RetriggeredBy[\"developer\"] must contain event.FeedbackGenerated; got %v", retriggers)
	}
}

// TestDevelopOnlyWorkflowDefRequired verifies the Required list is unchanged.
func TestDevelopOnlyWorkflowDefRequired(t *testing.T) {
	def := DevelopOnlyWorkflowDef()
	want := []string{"workspace", "developer", "reviewer", "committer"}

	if len(def.Required) != len(want) {
		t.Fatalf("Required length: want %d, got %d", len(want), len(def.Required))
	}
	for i, r := range want {
		if def.Required[i] != r {
			t.Errorf("Required[%d]: want %q, got %q", i, r, def.Required[i])
		}
	}
}

// TestDevelopOnlyWorkflowDefGraph verifies the DAG edges are unchanged.
func TestDevelopOnlyWorkflowDefGraph(t *testing.T) {
	def := DevelopOnlyWorkflowDef()

	cases := []struct {
		handler string
		preds   []string
	}{
		{"workspace", []string{}},
		{"developer", []string{"workspace"}},
		{"reviewer", []string{"developer"}},
		{"committer", []string{"reviewer"}},
	}

	for _, tc := range cases {
		preds, ok := def.Graph[tc.handler]
		if !ok {
			t.Errorf("Graph missing entry for %q", tc.handler)
			continue
		}
		if len(preds) != len(tc.preds) {
			t.Errorf("Graph[%q] predecessor count: want %d, got %d (preds=%v)", tc.handler, len(tc.preds), len(preds), preds)
			continue
		}
		for i, p := range tc.preds {
			if preds[i] != p {
				t.Errorf("Graph[%q][%d]: want %q, got %q", tc.handler, i, p, preds[i])
			}
		}
	}
}

// TestDevelopOnlyWorkflowDefPreservesMaxIterations verifies MaxIterations stays at 3
// and EscalateOnMaxIter remains false (develop-only silently fails at limit).
func TestDevelopOnlyWorkflowDefPreservesMaxIterations(t *testing.T) {
	def := DevelopOnlyWorkflowDef()

	if def.MaxIterations != 3 {
		t.Errorf("MaxIterations: want 3, got %d", def.MaxIterations)
	}
	if def.EscalateOnMaxIter {
		t.Error("EscalateOnMaxIter must be false for develop-only (silent fail at max iterations)")
	}
}
