package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNegotiateKnowledge_CriticalityMatrix(t *testing.T) {
	refs := []KnowledgeRef{
		{Pack: "go-conv", Load: LoadOnDemand, Criticality: CriticalityRequired},
		{Pack: "domain", Load: LoadOnDemand, Criticality: CriticalityOptional},
	}

	t.Run("mcp-capable delivers all", func(t *testing.T) {
		plan := NegotiateKnowledge(refs, true)
		if len(plan.DeliverPacks) != 2 {
			t.Errorf("capable backend should deliver both packs, got %v", plan.DeliverPacks)
		}
		if len(plan.FailRequired) != 0 || len(plan.UnavailableOptional) != 0 {
			t.Errorf("capable backend must not fail/degrade: %+v", plan)
		}
	})

	t.Run("non-capable: required fails, optional degrades", func(t *testing.T) {
		plan := NegotiateKnowledge(refs, false)
		if len(plan.DeliverPacks) != 0 {
			t.Errorf("non-capable backend delivers nothing, got %v", plan.DeliverPacks)
		}
		if len(plan.FailRequired) != 1 || plan.FailRequired[0] != "go-conv" {
			t.Errorf("required pack must fail dispatch, got %v", plan.FailRequired)
		}
		if len(plan.UnavailableOptional) != 1 || plan.UnavailableOptional[0] != "domain" {
			t.Errorf("optional pack must degrade + signal, got %v", plan.UnavailableOptional)
		}
	})
}

func TestHasRequiredKnowledge(t *testing.T) {
	if HasRequiredKnowledge([]KnowledgeRef{{Pack: "a", Criticality: CriticalityOptional}}) {
		t.Error("optional-only must not require pinning")
	}
	if !HasRequiredKnowledge([]KnowledgeRef{
		{Pack: "a", Criticality: CriticalityOptional},
		{Pack: "b", Criticality: CriticalityRequired},
	}) {
		t.Error("any required ref must require pinning")
	}
}

func TestResolvePackDir(t *testing.T) {
	root := t.TempDir()
	// <root>/acme/widget/go-conv/SKILL.md
	packDir := filepath.Join(root, "acme", "widget", "go-conv")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, manifestFileName), []byte("# pack"), 0o644); err != nil {
		t.Fatal(err)
	}

	if dir, ok := ResolvePackDir(root, "acme/widget", "go-conv"); !ok || dir != packDir {
		t.Errorf("owner/repo lookup: got %q ok=%v, want %q", dir, ok, packDir)
	}
	// Bare-name fallback: repo "other/widget" should still find <root>/widget? No
	// — only the basename of the GIVEN repo is tried, not arbitrary owners.
	if _, ok := ResolvePackDir(root, "acme/widget", "missing"); ok {
		t.Error("missing pack must report ok=false")
	}
	if _, ok := ResolvePackDir("", "acme/widget", "go-conv"); ok {
		t.Error("empty root ⇒ knowledge off ⇒ ok=false")
	}
}

func TestResolvePackDir_BareNameFallback(t *testing.T) {
	root := t.TempDir()
	// Pack stored under bare name <root>/widget/go-conv (no owner dir).
	packDir := filepath.Join(root, "widget", "go-conv")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, manifestFileName), []byte("# pack"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Lookup with owner-qualified repo should fall back to the bare basename.
	if dir, ok := ResolvePackDir(root, "acme/widget", "go-conv"); !ok || dir != packDir {
		t.Errorf("bare-name fallback: got %q ok=%v, want %q", dir, ok, packDir)
	}
}

func TestKnowledgeDir_Precedence(t *testing.T) {
	t.Setenv("RICK_KNOWLEDGE_DIR", "/explicit/knowledge")
	if got := KnowledgeDir(); got != "/explicit/knowledge" {
		t.Errorf("RICK_KNOWLEDGE_DIR must win, got %q", got)
	}
	t.Setenv("RICK_KNOWLEDGE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := KnowledgeDir(); got != filepath.Join("/xdg", "rick", "knowledge") {
		t.Errorf("XDG fallback, got %q", got)
	}
}

func TestBuildRetrievalMCPConfig(t *testing.T) {
	if got, _ := BuildRetrievalMCPConfig(nil); got != "" {
		t.Errorf("no packs ⇒ empty config, got %q", got)
	}
	cfg, err := BuildRetrievalMCPConfig([]string{"/k/acme/widget/go-conv"})
	if err != nil {
		t.Fatalf("BuildRetrievalMCPConfig: %v", err)
	}
	for _, want := range []string{"mcpServers", "knowledge", "/k/acme/widget/go-conv"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q: %s", want, cfg)
		}
	}
}

func TestRegistry_KnowledgeRefs(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, manifestPersonasSubdir, "dev",
		"---\nschema_version: 1\nname: dev\ndescription: d\nidentity: id.md\n"+
			"knowledge:\n  - pack: go-conv\n    load: on-demand\n    criticality: required\n---\nbody\n")
	// identity file so the persona is valid for any later compose (not needed here).

	reg := DefaultRegistry()
	if err := reg.LoadManifests(root, nil); err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}
	refs := reg.KnowledgeRefs("dev")
	if len(refs) != 1 || refs[0].Pack != "go-conv" || refs[0].Criticality != CriticalityRequired {
		t.Errorf("KnowledgeRefs = %+v", refs)
	}
	// A code-only persona carries no knowledge.
	if got := reg.KnowledgeRefs("developer"); got != nil {
		t.Errorf("code persona should have no knowledge refs, got %v", got)
	}
}
