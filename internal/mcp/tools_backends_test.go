package mcp

import (
	"encoding/json"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/backend"
)

func TestToolBackends(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.BackendName = "codex"
	t.Setenv("RICK_REVIEW_BACKENDS", "claude,opencode")

	s := NewServer(deps, testLogger())
	defer s.Close()

	// Backends are now exposed through the rick_job_inspect facade's "backends"
	// panel (no job_id), so the result is wrapped under that panel key.
	raw, err := callTool(t, s, "rick_job_inspect", map[string]any{"include": []string{"backends"}})
	if err != nil {
		t.Fatalf("rick_job_inspect backends: %v", err)
	}

	// Round-trip through JSON to assert the serialized shape clients receive.
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wrapper struct {
		Backends backendsResult `json:"backends"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	res := wrapper.Backends

	if res.DefaultBackend != "codex" {
		t.Errorf("default_backend: want codex, got %q", res.DefaultBackend)
	}
	if len(res.ReviewBackends) != 2 || res.ReviewBackends[0] != "claude" || res.ReviewBackends[1] != "opencode" {
		t.Errorf("review_backends: want [claude opencode], got %v", res.ReviewBackends)
	}

	// Every catalog backend must be listed, with opencode present and the
	// default/review flags set consistently.
	if len(res.Backends) != len(backend.Catalog) {
		t.Fatalf("want %d backends, got %d", len(backend.Catalog), len(res.Backends))
	}
	byName := make(map[string]backendInfo, len(res.Backends))
	for _, bi := range res.Backends {
		byName[bi.Name] = bi
	}
	oc, ok := byName["opencode"]
	if !ok {
		t.Fatal("opencode missing from backends list")
	}
	if oc.Binary != "opencode" {
		t.Errorf("opencode binary: want opencode, got %q", oc.Binary)
	}
	if !oc.InReview {
		t.Error("opencode should be flagged in_review_rotation")
	}
	if byName["codex"].Default != true {
		t.Error("codex should be flagged default")
	}
	if byName["gemini"].InReview {
		t.Error("gemini should NOT be in review rotation after env override")
	}
}
