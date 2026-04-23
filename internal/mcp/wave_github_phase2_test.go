package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	gh "github.com/marconn/rick-event-driven-development/internal/github"
)

// routableServer dispatches by path and optionally by method. Test handlers
// can register path → responder.
type routableServer struct {
	srv *httptest.Server
	mux map[string]http.HandlerFunc
}

func newRoutable(t *testing.T) *routableServer {
	t.Helper()
	r := &routableServer{mux: make(map[string]http.HandlerFunc)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h, ok := r.mux[req.URL.Path]
		if !ok {
			http.Error(w, "not mocked: "+req.URL.Path, http.StatusNotFound)
			return
		}
		h(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *routableServer) handleJSON(path, body string) {
	r.mux[path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (r *routableServer) client() *gh.Client {
	return gh.NewClientWithBase(r.srv.URL, "test-token")
}

// --- body_refs dependency mode ---

func TestComputeGithubWavePlan_BodyRefsDeps(t *testing.T) {
	r := newRoutable(t)
	parentBody := "" // irrelevant when dependency_source=body_refs
	parent := map[string]any{"number": 1, "title": "parent", "body": parentBody, "state": "open"}
	parentJSON, _ := json.Marshal(parent)
	r.handleJSON("/repos/o/r/issues/1", string(parentJSON))

	subs := []map[string]any{
		{"number": 10, "title": "root", "state": "open", "body": "no deps", "labels": []map[string]string{}},
		{"number": 11, "title": "mid", "state": "open", "body": "Depends on #10.", "labels": []map[string]string{}},
		{"number": 12, "title": "leaf", "state": "open", "body": "Depends on #10 and #11.", "labels": []map[string]string{}},
	}
	subsJSON, _ := json.Marshal(subs)
	r.handleJSON("/repos/o/r/issues/1/sub_issues", string(subsJSON))
	// Each sub needs a timeline response (empty) so PR-detection returns no PR.
	for _, n := range []int{10, 11, 12} {
		r.handleJSON(fmt.Sprintf("/repos/o/r/issues/%d/timeline", n), "[]")
		// body_refs mode fetches each child body via GetIssue. The sub_issues
		// payload already includes `body`, so we populate it and it stays on
		// the node — but the implementation also calls GetIssue if body is
		// empty. Sub-issues from the fixture include body, so no extra mock
		// is strictly needed for #10; still register for safety.
	}

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "sub_issues",
			"dependency_source": "body_refs",
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)
	if plan.Diagnostics.DependencyPath != "body_refs" {
		t.Fatalf("dependency_path: %q", plan.Diagnostics.DependencyPath)
	}
	if len(plan.Waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %+v", len(plan.Waves), plan.Waves)
	}
	if plan.Waves[0].Tickets[0].ID != "o/r#10" {
		t.Errorf("wave 1: %+v", plan.Waves[0].Tickets)
	}
}

// --- labels dependency mode ---

func TestComputeGithubWavePlan_LabelsDeps(t *testing.T) {
	r := newRoutable(t)
	parent, _ := json.Marshal(map[string]any{"number": 1, "title": "p", "body": "", "state": "open"})
	r.handleJSON("/repos/o/r/issues/1", string(parent))

	subs := []map[string]any{
		{"number": 10, "title": "root", "state": "open", "labels": []map[string]string{}},
		{"number": 11, "title": "leaf", "state": "open", "labels": []map[string]string{{"name": "depends:10"}}},
	}
	subsJSON, _ := json.Marshal(subs)
	r.handleJSON("/repos/o/r/issues/1/sub_issues", string(subsJSON))
	for _, n := range []int{10, 11} {
		r.handleJSON(fmt.Sprintf("/repos/o/r/issues/%d/timeline", n), "[]")
	}

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "sub_issues",
			"dependency_source": "labels",
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)
	if plan.Diagnostics.DependencyPath != "labels" {
		t.Fatalf("dependency_path: %q", plan.Diagnostics.DependencyPath)
	}
	if len(plan.Waves) != 2 {
		t.Fatalf("expected 2 waves, got %d", len(plan.Waves))
	}
	if plan.Waves[1].Tickets[0].ID != "o/r#11" {
		t.Errorf("wave 2 mismatch: %+v", plan.Waves[1].Tickets)
	}
}

// --- task_list discovery ---

func TestComputeGithubWavePlan_TaskListDiscovery(t *testing.T) {
	r := newRoutable(t)
	parentBody := `
- [ ] #21 alpha
- [x] #22 done
- [ ] #23 gamma
`
	parent, _ := json.Marshal(map[string]any{"number": 1, "title": "p", "body": parentBody, "state": "open"})
	r.handleJSON("/repos/o/r/issues/1", string(parent))

	for _, n := range []int{21, 23} {
		issue, _ := json.Marshal(map[string]any{
			"number": n, "title": fmt.Sprintf("child-%d", n), "body": "",
			"state": "open", "labels": []map[string]string{},
		})
		r.handleJSON(fmt.Sprintf("/repos/o/r/issues/%d", n), string(issue))
		r.handleJSON(fmt.Sprintf("/repos/o/r/issues/%d/timeline", n), "[]")
	}

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "task_list",
			"dependency_source": "none",
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)
	if plan.Diagnostics.DiscoveryPath != "task_list" {
		t.Fatalf("discovery_path: %q", plan.Diagnostics.DiscoveryPath)
	}
	if len(plan.Waves) != 1 || len(plan.Waves[0].Tickets) != 2 {
		t.Fatalf("expected 1 wave with 2 tickets, got %+v", plan.Waves)
	}
}

// --- dag_map + rick:* label routing ---

func TestComputeGithubWavePlan_DAGMapRouting(t *testing.T) {
	r := newRoutable(t)
	parent, _ := json.Marshal(map[string]any{"number": 1, "title": "p", "body": "", "state": "open"})
	r.handleJSON("/repos/o/r/issues/1", string(parent))

	subs := []map[string]any{
		{"number": 30, "title": "generic", "state": "open", "labels": []map[string]string{}},
		{"number": 31, "title": "develop-only", "state": "open", "labels": []map[string]string{{"name": "rick:develop-only"}}},
		{"number": 32, "title": "mapped", "state": "open", "labels": []map[string]string{{"name": "rick:custom"}}},
		{"number": 33, "title": "typo", "state": "open", "labels": []map[string]string{{"name": "rick:typo-that-doesnt-exist"}}},
	}
	subsJSON, _ := json.Marshal(subs)
	r.handleJSON("/repos/o/r/issues/1/sub_issues", string(subsJSON))
	for _, n := range []int{30, 31, 32, 33} {
		r.handleJSON(fmt.Sprintf("/repos/o/r/issues/%d/timeline", n), "[]")
	}

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "sub_issues",
			"dependency_source": "none",
			"dag_options": map[string]any{
				"dag_map": map[string]string{
					"rick:custom": "github-dev",
					"default":     "workspace-dev",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)

	byID := map[string]wavePlanTicket{}
	for _, w := range plan.Waves {
		for _, tk := range w.Tickets {
			byID[tk.ID] = tk
		}
	}
	checks := []struct {
		id     string
		wantDA string
	}{
		{"o/r#30", "workspace-dev"}, // no rick:* label → default
		{"o/r#31", "develop-only"},  // rick:develop-only known convention
		{"o/r#32", "github-dev"},    // rick:custom mapped
		{"o/r#33", "workspace-dev"}, // rick:typo → default + diagnostic
	}
	for _, c := range checks {
		tk, ok := byID[c.id]
		if !ok {
			t.Fatalf("missing %s in waves: %+v", c.id, plan.Waves)
		}
		got, _ := tk.DAGParams["dag"].(string)
		if got != c.wantDA {
			t.Errorf("%s: dag=%q, want %q (DAGParams=%v)", c.id, got, c.wantDA, tk.DAGParams)
		}
	}
	foundTypoDiag := false
	for _, sk := range plan.Diagnostics.Skipped {
		if sk.ID == "o/r#33" && strings.Contains(sk.Reason, "unknown rick label") {
			foundTypoDiag = true
		}
	}
	if !foundTypoDiag {
		t.Errorf("expected unknown_rick_label diagnostic for o/r#33, got: %+v", plan.Diagnostics.Skipped)
	}
}

// TestComputeGithubWavePlan_DefaultDAGForGithubSource locks in the built-in
// default for GitHub wave sources: when no dag_map is supplied, every
// unlabeled child must default to github-dev (not workspace-dev). This is
// what routes the workflow through github-context so the workspace branch
// comes out as "issue-<N>" instead of "rick/<corr>".
func TestComputeGithubWavePlan_DefaultDAGForGithubSource(t *testing.T) {
	r := newRoutable(t)
	parent, _ := json.Marshal(map[string]any{"number": 1, "title": "p", "body": "", "state": "open"})
	r.handleJSON("/repos/o/r/issues/1", string(parent))

	subs := []map[string]any{
		{"number": 40, "title": "unlabeled", "state": "open", "labels": []map[string]string{}},
	}
	subsJSON, _ := json.Marshal(subs)
	r.handleJSON("/repos/o/r/issues/1/sub_issues", string(subsJSON))
	r.handleJSON("/repos/o/r/issues/40/timeline", "[]")

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "sub_issues",
			"dependency_source": "none",
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)

	var target wavePlanTicket
	for _, w := range plan.Waves {
		for _, tk := range w.Tickets {
			if tk.ID == "o/r#40" {
				target = tk
			}
		}
	}
	got, _ := target.DAGParams["dag"].(string)
	if got != "github-dev" {
		t.Errorf("default dag for unlabeled GitHub child: got %q, want github-dev", got)
	}
}

// --- PR-feedback routing via timeline cross-reference ---

func TestComputeGithubWavePlan_PRFeedbackTimelineRouting(t *testing.T) {
	r := newRoutable(t)
	parent, _ := json.Marshal(map[string]any{"number": 1, "title": "p", "body": "", "state": "open"})
	r.handleJSON("/repos/o/r/issues/1", string(parent))

	subs := []map[string]any{
		{"number": 50, "title": "has-pr", "state": "open", "labels": []map[string]string{}},
	}
	subsJSON, _ := json.Marshal(subs)
	r.handleJSON("/repos/o/r/issues/1/sub_issues", string(subsJSON))

	timeline := []map[string]any{
		{
			"event": "cross-referenced",
			"source": map[string]any{
				"type": "issue",
				"issue": map[string]any{
					"number":       123,
					"state":        "open",
					"pull_request": map[string]any{},
				},
			},
		},
	}
	timelineJSON, _ := json.Marshal(timeline)
	r.handleJSON("/repos/o/r/issues/50/timeline", string(timelineJSON))

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "sub_issues",
			"dependency_source": "none",
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)
	tk := plan.Waves[0].Tickets[0]
	if tk.DAGParams["dag"] != "pr-feedback" {
		t.Errorf("expected dag=pr-feedback, got %v", tk.DAGParams)
	}
	if tk.DAGParams["pr_source"] != "gh:o/r#123" {
		t.Errorf("expected pr_source=gh:o/r#123, got %v", tk.DAGParams)
	}
}

// --- GraphQL fast path ---

func TestComputeGithubWavePlan_GraphQLFastPath(t *testing.T) {
	r := newRoutable(t)
	gqlResponse := `{
	  "data": {
	    "repository": {
	      "issue": {
	        "number": 1,
	        "title": "parent",
	        "body": "",
	        "state": "OPEN",
	        "subIssues": {
	          "nodes": [
	            {"number": 100, "title": "alpha", "state": "OPEN", "body": "", "repository": {"nameWithOwner": "o/r"}, "labels": {"nodes": []}, "timelineItems": {"nodes": []}},
	            {"number": 101, "title": "beta",  "state": "OPEN", "body": "", "repository": {"nameWithOwner": "o/r"}, "labels": {"nodes": []},
	              "timelineItems": {"nodes": [
	                {"__typename": "CrossReferencedEvent", "source": {"__typename": "PullRequest", "number": 9001, "state": "OPEN", "repository": {"nameWithOwner": "o/r"}}}
	              ]}}
	          ]
	        }
	      }
	    }
	  }
	}`
	r.mux["/graphql"] = func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gqlResponse))
	}

	t.Setenv("RICK_GITHUB_GRAPHQL", "1")

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = r.client()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "o/r#1",
			"child_discovery":   "sub_issues",
			"dependency_source": "none",
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := result.(wavePlanResult)
	if plan.Diagnostics.DiscoveryPath != "sub_issues_graphql" {
		t.Fatalf("expected sub_issues_graphql, got %q", plan.Diagnostics.DiscoveryPath)
	}
	if len(plan.Waves) != 1 || len(plan.Waves[0].Tickets) != 2 {
		t.Fatalf("expected 1 wave with 2 tickets, got %+v", plan.Waves)
	}
	// beta (#101) had an open PR cross-referenced → routes to pr-feedback
	// without needing a timeline REST call (REST endpoint isn't mocked — the
	// test would fail if we hit it).
	for _, tk := range plan.Waves[0].Tickets {
		if tk.ID == "o/r#101" {
			if tk.DAGParams["dag"] != "pr-feedback" {
				t.Errorf("expected o/r#101 dag=pr-feedback, got %+v", tk.DAGParams)
			}
		}
	}
}

// Sanity: the GraphQL env var must not leak between tests.
func TestGraphQLFastPath_OptOut(t *testing.T) {
	if v := os.Getenv("RICK_GITHUB_GRAPHQL"); v != "" {
		t.Fatalf("RICK_GITHUB_GRAPHQL=%q leaked from another test", v)
	}
}
