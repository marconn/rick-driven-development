package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/marconn/rick-event-driven-development/internal/github"
)

// newTestGitHubClient returns a gh.Client pointed at an httptest server.
// The test server dispatches based on path — the test provides a handler map
// (path → JSON body). Missing paths return 404 so tests can assert on
// error messages.
func newTestGitHubClient(t *testing.T, routes map[string]string) (*gh.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	client := gh.NewClientWithBase(srv.URL, "test-token")
	return client, srv.Close
}

func TestComputeGithubWavePlan_Spec641(t *testing.T) {
	// Mirror the spec §12 validation case.
	parentBody := `Observability epic — coordinates nine child issues.

| Issue | Title                            | Depends on    |
| ----- | -------------------------------- | ------------- |
| #642  | Temporal OTel interceptor        |               |
| #643  | Delete appmetrics                |               |
| #646  | Collector base                   |               |
| #647  | Dashboards baseline              |               |
| #648  | Tracing wiring                   |               |
| #650  | Log routing                      |               |
| #645  | Wave 2 A                         | #642          |
| #649  | Wave 2 B                         | #646          |
| #644  | Final consolidation              | #642, #646, #645 |
`
	parent := map[string]any{
		"number": 641,
		"title":  "Observability epic",
		"body":   parentBody,
		"state":  "open",
	}
	parentJSON, _ := json.Marshal(parent)

	subs := make([]map[string]any, 0)
	for _, n := range []int{642, 643, 646, 647, 648, 650, 645, 649, 644} {
		subs = append(subs, map[string]any{
			"number": n,
			"title":  fmt.Sprintf("child-%d", n),
			"state":  "open",
			"labels": []map[string]string{},
		})
	}
	subsJSON, _ := json.Marshal(subs)

	routes := map[string]string{
		"/repos/hulilabs/huli/issues/641":            string(parentJSON),
		"/repos/hulilabs/huli/issues/641/sub_issues": string(subsJSON),
	}
	client, closeSrv := newTestGitHubClient(t, routes)
	defer closeSrv()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{
			"type":              "github",
			"parent":            "hulilabs/huli#641",
			"child_discovery":   "sub_issues",
			"dependency_source": "table",
		},
	})
	if err != nil {
		t.Fatalf("rick_wave_plan: %v", err)
	}

	plan, ok := result.(wavePlanResult)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	if plan.Source.Type != "github" || plan.Source.Parent != "hulilabs/huli#641" {
		t.Fatalf("source mismatch: %+v", plan.Source)
	}
	if plan.Diagnostics.DiscoveryPath != "sub_issues" || plan.Diagnostics.DependencyPath != "table" {
		t.Fatalf("diagnostics.discovery/dependency mismatch: %+v", plan.Diagnostics)
	}
	if len(plan.Waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %+v", len(plan.Waves), plan.Waves)
	}
	wantWave1 := map[string]bool{
		"hulilabs/huli#642": true,
		"hulilabs/huli#643": true,
		"hulilabs/huli#646": true,
		"hulilabs/huli#647": true,
		"hulilabs/huli#648": true,
		"hulilabs/huli#650": true,
	}
	got1 := make(map[string]bool)
	for _, tk := range plan.Waves[0].Tickets {
		got1[tk.ID] = true
		if tk.IDKind != "github" {
			t.Errorf("expected id_kind=github, got %q", tk.IDKind)
		}
	}
	if len(got1) != len(wantWave1) {
		t.Fatalf("wave 1 size mismatch: got=%v want=%v", got1, wantWave1)
	}
	for k := range wantWave1 {
		if !got1[k] {
			t.Errorf("wave 1 missing %s", k)
		}
	}

	wantWave2 := map[string]bool{"hulilabs/huli#645": true, "hulilabs/huli#649": true}
	got2 := make(map[string]bool)
	for _, tk := range plan.Waves[1].Tickets {
		got2[tk.ID] = true
	}
	if fmt.Sprintf("%v", got2) != fmt.Sprintf("%v", wantWave2) {
		t.Errorf("wave 2 mismatch: got=%v want=%v", got2, wantWave2)
	}

	if len(plan.Waves[2].Tickets) != 1 || plan.Waves[2].Tickets[0].ID != "hulilabs/huli#644" {
		t.Errorf("wave 3 mismatch: %+v", plan.Waves[2].Tickets)
	}
}

func TestComputeGithubWavePlan_ClosedParentFails(t *testing.T) {
	parent := map[string]any{"number": 99, "title": "closed", "body": "", "state": "closed"}
	parentJSON, _ := json.Marshal(parent)
	client, closeSrv := newTestGitHubClient(t, map[string]string{
		"/repos/o/r/issues/99": string(parentJSON),
	})
	defer closeSrv()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{"type": "github", "parent": "o/r#99"},
	})
	if err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("expected closed-parent error, got %v", err)
	}
}

func TestComputeGithubWavePlan_NoChildrenEmitsDiagnostic(t *testing.T) {
	parentJSON, _ := json.Marshal(map[string]any{
		"number": 10, "title": "empty", "body": "", "state": "open",
	})
	client, closeSrv := newTestGitHubClient(t, map[string]string{
		"/repos/o/r/issues/10":            string(parentJSON),
		"/repos/o/r/issues/10/sub_issues": "[]",
	})
	defer closeSrv()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.GitHub = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_plan", map[string]any{
		"source": map[string]any{"type": "github", "parent": "o/r#10"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	plan := result.(wavePlanResult)
	if plan.Diagnostics.Reason != "no_children_discovered" {
		t.Fatalf("expected diagnostics.reason=no_children_discovered, got %+v", plan.Diagnostics)
	}
	if len(plan.Waves) != 0 {
		t.Fatalf("expected 0 waves, got %d", len(plan.Waves))
	}
}

func TestParseGHParent(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"owner/repo#1", false},
		{"hulilabs/huli#641", false},
		{"owner/repo", true},
		{"#42", true},
		{"owner#42", true},
		{"owner/repo#", true},
		{"owner/repo#abc", true},
		{"owner/repo#-5", true},
	}
	for _, tc := range tests {
		_, _, _, err := parseGHParent(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseGHParent(%q): err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestWavePlan_JiraEpicBackCompat(t *testing.T) {
	// With no source object and no Jira client, the legacy `epic` shorthand
	// should still route to Jira and fail with the standard Jira-not-configured
	// error — proving the shim is in place and routes correctly.
	s, cleanup := testServer(t)
	defer cleanup()

	raw, _ := json.Marshal(map[string]any{"epic": "PROJ-1"})
	_, err := s.tools["rick_wave_plan"].Handler(context.Background(), raw)
	if err == nil {
		t.Fatalf("expected error when Jira is unconfigured")
	}
	if !strings.Contains(err.Error(), "Jira") {
		t.Fatalf("expected Jira-related error, got %v", err)
	}
}
