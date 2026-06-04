package persona

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest on-disk layout under a manifests root (operator-local
// RICK_PERSONA_MANIFESTS_DIR, mirroring the in-repo defaults layout):
//
//	<root>/personas/<name>/SKILL.md   — persona manifests
//	<root>/skills/<name>/SKILL.md     — reusable skill fragments
//
// A persona's system prompt is composed as: identity + each referenced skill
// fragment, in the persona's declared skill order. This is the L1 capability —
// prompt composition for existing handlers, no recompile. Handler-binding /
// safety fields stay in Go construction (the manifest cannot set them; the
// validator rejects them — see manifest.go forbiddenKeys).
const (
	manifestPersonasSubdir = "personas"
	manifestSkillsSubdir   = "skills"
	manifestFileName       = "SKILL.md"
)

// loadedPersona pairs a parsed persona manifest with the directory of its
// SKILL.md, so an `identity:` file ref can be resolved relative to it.
type loadedPersona struct {
	manifest *PersonaManifest
	dir      string
}

// ManifestSource holds the persona and skill manifests discovered under a
// manifests root. It is the data-driven half of the dual-source registry; the
// Registry consults it first (manifest wins on name collision) and falls back
// to the embedded/code prompts.
type ManifestSource struct {
	personas map[string]loadedPersona
	skills   map[string]*SkillManifest
}

// LoadManifestDir discovers and parses every persona and skill manifest under
// root. It is resilient: a malformed or invalid manifest fails ONLY itself —
// its error is collected and returned, and loading continues. Returns the
// source (always non-nil) plus the per-manifest errors. A missing personas/ or
// skills/ subdir is not an error (an operator may provide only one).
func LoadManifestDir(root string) (*ManifestSource, []error) {
	src := &ManifestSource{
		personas: make(map[string]loadedPersona),
		skills:   make(map[string]*SkillManifest),
	}
	var errs []error

	// Skills first so persona composition can reference them.
	for name, path := range discoverManifests(filepath.Join(root, manifestSkillsSubdir), &errs) {
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read skill %q: %w", path, err))
			continue
		}
		sm, err := ParseSkillManifest(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("skill %q (%s): %w", name, path, err))
			continue
		}
		src.skills[sm.Name] = sm
	}

	for name, path := range discoverManifests(filepath.Join(root, manifestPersonasSubdir), &errs) {
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read persona %q: %w", path, err))
			continue
		}
		pm, err := ParsePersonaManifest(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("persona %q (%s): %w", name, path, err))
			continue
		}
		src.personas[pm.Name] = loadedPersona{manifest: pm, dir: filepath.Dir(path)}
	}

	return src, errs
}

// discoverManifests returns name→SKILL.md path for each immediate subdirectory
// of parent that contains a SKILL.md. A non-existent parent yields nothing
// (not an error). Read errors on parent itself are appended to errs.
func discoverManifests(parent string, errs *[]error) map[string]string {
	out := make(map[string]string)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if !os.IsNotExist(err) {
			*errs = append(*errs, fmt.Errorf("read manifests dir %q: %w", parent, err))
		}
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(parent, e.Name(), manifestFileName)
		if _, err := os.Stat(path); err == nil {
			out[e.Name()] = path
		}
	}
	return out
}

// Has reports whether a persona manifest with this name was loaded.
func (s *ManifestSource) Has(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.personas[name]
	return ok
}

// PersonaNames returns the names of all loaded persona manifests.
func (s *ManifestSource) PersonaNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.personas))
	for n := range s.personas {
		names = append(names, n)
	}
	return names
}

// ComposeSystemPrompt builds a persona's system prompt from its manifest:
// the identity (an `identity:` file ref resolved relative to the manifest dir,
// or the inline markdown body) followed by each referenced skill fragment in
// declared order. A missing skill reference is a LOAD ERROR for that persona —
// never a silent skip — so a typo can't quietly drop a procedure the reviewer
// depends on.
func (s *ManifestSource) ComposeSystemPrompt(name string) (string, error) {
	lp, ok := s.personas[name]
	if !ok {
		return "", fmt.Errorf("persona manifest %q not found", name)
	}

	identity, err := s.resolveIdentity(lp)
	if err != nil {
		return "", err
	}

	parts := make([]string, 0, 1+len(lp.manifest.Skills))
	parts = append(parts, strings.TrimSpace(identity))
	for _, ref := range lp.manifest.Skills {
		sm, ok := s.skills[ref]
		if !ok {
			return "", fmt.Errorf("persona %q references unknown skill %q", name, ref)
		}
		parts = append(parts, strings.TrimSpace(sm.Body))
	}
	return strings.Join(parts, "\n\n"), nil
}

// resolveIdentity returns the persona's identity text: the file referenced by
// `identity:` (read relative to the manifest dir) or, when no ref is set, the
// inline markdown body. The manifest validator guarantees at least one is
// present.
func (s *ManifestSource) resolveIdentity(lp loadedPersona) (string, error) {
	ref := strings.TrimSpace(lp.manifest.Identity)
	if ref == "" {
		return lp.manifest.Body, nil
	}
	path := filepath.Join(lp.dir, ref)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("persona %q: read identity %q: %w", lp.manifest.Name, path, err)
	}
	return string(data), nil
}
