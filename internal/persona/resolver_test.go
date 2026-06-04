package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest writes a SKILL.md under <root>/<kind>/<name>/SKILL.md.
func writeManifest(t *testing.T, root, kind, name, content string) {
	t.Helper()
	dir := filepath.Join(root, kind, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func skillManifest(name, body string) string {
	return "---\nname: " + name + "\ndescription: the " + name + " skill\n---\n" + body + "\n"
}

func personaManifest(name string, skills []string, body string) string {
	var b strings.Builder
	b.WriteString("---\nschema_version: 1\nname: ")
	b.WriteString(name)
	b.WriteString("\ndescription: a manifest persona\n")
	if len(skills) > 0 {
		b.WriteString("skills:\n")
		for _, s := range skills {
			b.WriteString("  - " + s + "\n")
		}
	}
	b.WriteString("---\n")
	b.WriteString(body)
	b.WriteString("\n")
	return b.String()
}

func TestComposeSystemPrompt_IdentityThenSkillsInOrder(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, manifestSkillsSubdir, "diff-grounding", skillManifest("diff-grounding", "GROUND every finding on a changed line."))
	writeManifest(t, root, manifestSkillsSubdir, "conventional-commit", skillManifest("conventional-commit", "USE conventional commit messages."))
	writeManifest(t, root, manifestPersonasSubdir, "dev",
		personaManifest("dev", []string{"diff-grounding", "conventional-commit"}, "You are the developer. Implement minimally."))

	src, errs := LoadManifestDir(root)
	if len(errs) != 0 {
		t.Fatalf("unexpected load errors: %v", errs)
	}
	got, err := src.ComposeSystemPrompt("dev")
	if err != nil {
		t.Fatalf("ComposeSystemPrompt: %v", err)
	}

	// Identity first, then skills in declared order.
	want := "You are the developer. Implement minimally.\n\n" +
		"GROUND every finding on a changed line.\n\n" +
		"USE conventional commit messages."
	if got != want {
		t.Errorf("composed prompt mismatch:\n got: %q\nwant: %q", got, want)
	}
	// Order assertion is explicit (regression guard if join changes).
	idIdx := strings.Index(got, "developer")
	dgIdx := strings.Index(got, "GROUND")
	ccIdx := strings.Index(got, "conventional commit")
	if idIdx >= dgIdx || dgIdx >= ccIdx {
		t.Errorf("composition order wrong: identity=%d diff=%d commit=%d", idIdx, dgIdx, ccIdx)
	}
}

func TestComposeSystemPrompt_IdentityFileRef(t *testing.T) {
	root := t.TempDir()
	// identity: points at a file relative to the persona's manifest dir.
	personaDir := filepath.Join(root, manifestPersonasSubdir, "dev")
	if err := os.MkdirAll(personaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "identity.md"), []byte("File-based identity."), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nschema_version: 1\nname: dev\ndescription: d\nidentity: identity.md\n---\n"
	if err := os.WriteFile(filepath.Join(personaDir, manifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	src, errs := LoadManifestDir(root)
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	got, err := src.ComposeSystemPrompt("dev")
	if err != nil {
		t.Fatalf("ComposeSystemPrompt: %v", err)
	}
	if got != "File-based identity." {
		t.Errorf("got %q", got)
	}
}

func TestComposeSystemPrompt_MissingSkillRefFailsLoudly(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, manifestPersonasSubdir, "dev",
		personaManifest("dev", []string{"nonexistent-skill"}, "Identity."))

	src, _ := LoadManifestDir(root)
	_, err := src.ComposeSystemPrompt("dev")
	if err == nil {
		t.Fatal("missing skill ref must fail the persona loudly")
	}
	if !strings.Contains(err.Error(), "nonexistent-skill") {
		t.Errorf("error must name the missing skill, got: %v", err)
	}
}

func TestLoadManifestDir_BadManifestFailsOnlyItself(t *testing.T) {
	root := t.TempDir()
	// One valid persona, one invalid (forbidden key).
	writeManifest(t, root, manifestPersonasSubdir, "good",
		personaManifest("good", nil, "Good identity."))
	writeManifest(t, root, manifestPersonasSubdir, "bad",
		"---\nschema_version: 1\nname: bad\ndescription: d\nyolo: true\n---\nbody\n")

	src, errs := LoadManifestDir(root)
	if len(errs) != 1 {
		t.Fatalf("want exactly 1 load error (the bad manifest), got %d: %v", len(errs), errs)
	}
	if !src.Has("good") {
		t.Error("the valid manifest must still load when a sibling is bad")
	}
	if src.Has("bad") {
		t.Error("the invalid manifest must not be registered")
	}
}

func TestLoadManifestDir_MissingSubdirsNotAnError(t *testing.T) {
	root := t.TempDir() // empty: no personas/ or skills/
	src, errs := LoadManifestDir(root)
	if len(errs) != 0 {
		t.Fatalf("empty root should yield no errors, got %v", errs)
	}
	if len(src.PersonaNames()) != 0 {
		t.Errorf("expected no personas, got %v", src.PersonaNames())
	}
}

func TestRegistry_ManifestWinsOnCollision(t *testing.T) {
	root := t.TempDir()
	// Override the embedded "developer" persona with a manifest.
	writeManifest(t, root, manifestPersonasSubdir, "developer",
		personaManifest("developer", nil, "MANIFEST developer identity override."))

	reg := DefaultRegistry()
	if err := reg.LoadManifests(root, nil); err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}

	got, err := reg.LoadSystemPrompt("developer")
	if err != nil {
		t.Fatalf("LoadSystemPrompt: %v", err)
	}
	if !strings.Contains(got, "MANIFEST developer identity override") {
		t.Errorf("manifest must win over embedded prompt, got: %q", got[:min(80, len(got))])
	}
}

func TestRegistry_ManifestOnlyPersonaIsRegistered(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, manifestPersonasSubdir, "custom-reviewer",
		personaManifest("custom-reviewer", nil, "A brand-new persona from a manifest."))

	reg := DefaultRegistry()
	if err := reg.LoadManifests(root, nil); err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}
	if _, err := reg.Get("custom-reviewer"); err != nil {
		t.Errorf("manifest-only persona should be registered: %v", err)
	}
	got, err := reg.LoadSystemPrompt("custom-reviewer")
	if err != nil || !strings.Contains(got, "brand-new persona") {
		t.Errorf("LoadSystemPrompt(custom-reviewer) = %q, %v", got, err)
	}
}

func TestRegistry_NoManifestsUnchangedBehavior(t *testing.T) {
	// Flag unset (LoadManifests never called / empty root) ⇒ embedded prompt.
	reg := DefaultRegistry()
	if err := reg.LoadManifests("", nil); err != nil {
		t.Fatalf("LoadManifests(empty) should be a no-op: %v", err)
	}
	got, err := reg.LoadSystemPrompt("developer")
	if err != nil {
		t.Fatalf("LoadSystemPrompt: %v", err)
	}
	embedded, err := loadEmbeddedPrompt("developer")
	if err != nil {
		t.Fatalf("loadEmbeddedPrompt: %v", err)
	}
	if got != embedded {
		t.Error("with no manifests, LoadSystemPrompt must return the embedded prompt byte-for-byte")
	}
}

func TestRegistry_ComposeErrorSurfacesNotSilentFallback(t *testing.T) {
	root := t.TempDir()
	// Manifest references a missing skill → composition error must propagate,
	// NOT silently fall back to an embedded prompt.
	writeManifest(t, root, manifestPersonasSubdir, "developer",
		personaManifest("developer", []string{"ghost-skill"}, "Identity."))

	reg := DefaultRegistry()
	if err := reg.LoadManifests(root, nil); err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}
	if _, err := reg.LoadSystemPrompt("developer"); err == nil {
		t.Fatal("a manifest composition error must surface, not fall back to the embedded prompt")
	}
}
