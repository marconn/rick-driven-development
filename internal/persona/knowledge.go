package persona

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Knowledge packs are operator-local, per-repo reference material a persona can
// consult, mirroring the per-repo quality-manifest model. Phase 1 delivers them
// only on tool-capable (MCP) backends via progressive disclosure; on
// non-capable backends, the criticality field governs behavior (required pins
// or fails; optional degrades + signals). Eager inlining is deferred.
//
// Resolution root precedence: RICK_KNOWLEDGE_DIR → $XDG_CONFIG_HOME/rick/knowledge
// → $HOME/.config/rick/knowledge. A pack lives at <root>/<owner>/<repo>/<pack>/
// with a SKILL.md. Empty root ⇒ the knowledge layer is off.

// KnowledgeEnabled reports whether the knowledge layer is turned on. It is
// opt-in via an EXPLICIT RICK_KNOWLEDGE_DIR — unset ⇒ no knowledge layer (the
// default), regardless of the XDG/HOME fallback dirs KnowledgeDir() resolves
// for pack lookup. Gating activation on the explicit flag (not the fallbacks)
// is what makes "unset ⇒ no behavior change" hold on any machine.
func KnowledgeEnabled() bool { return os.Getenv("RICK_KNOWLEDGE_DIR") != "" }

// KnowledgeDir resolves the operator-local knowledge root, or "" when none is
// configured (knowledge layer off — the default).
func KnowledgeDir() string {
	if d := os.Getenv("RICK_KNOWLEDGE_DIR"); d != "" {
		return d
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rick", "knowledge")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "rick", "knowledge")
	}
	return ""
}

// ResolvePackDir returns the on-disk directory for a pack scoped to repo
// (<owner>/<repo>), and whether it exists. repo may be "owner/name" or just
// "name"; both <root>/<repo>/<pack> are tried. A pack directory must contain a
// SKILL.md to count as present.
func ResolvePackDir(root, repo, pack string) (string, bool) {
	if root == "" || pack == "" {
		return "", false
	}
	candidates := []string{filepath.Join(root, repo, pack)}
	// Bare-name fallback: when repo is "owner/name", also try "<root>/name/pack".
	if base := filepath.Base(repo); base != repo {
		candidates = append(candidates, filepath.Join(root, base, pack))
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(filepath.Join(dir, manifestFileName)); err == nil && !fi.IsDir() {
			return dir, true
		}
	}
	return "", false
}

// KnowledgePlan is the negotiated outcome of a persona's declared knowledge
// against a chosen backend's capability. It is a pure function of (refs,
// mcpCapable) — see NegotiateKnowledge — so the criticality contract is fully
// testable without a live backend.
type KnowledgePlan struct {
	// DeliverPacks are the pack names to expose via tool retrieval. Populated
	// only when the backend is MCP-capable.
	DeliverPacks []string
	// UnavailableOptional are optional packs that could not be delivered on a
	// non-capable backend — the persona runs degraded and a knowledge_unavailable
	// signal is emitted.
	UnavailableOptional []string
	// FailRequired is non-empty when a REQUIRED pack cannot be delivered by the
	// chosen backend (it should have been pinned to a capable one). The caller
	// MUST fail dispatch with this list named — never run a required pack blind.
	FailRequired []string
}

// HasRequiredKnowledge reports whether any ref is criticality=required. The
// dispatch path uses this to decide whether to pin to an MCP-capable backend
// BEFORE selecting/running.
func HasRequiredKnowledge(refs []KnowledgeRef) bool {
	for _, r := range refs {
		if r.Criticality == CriticalityRequired {
			return true
		}
	}
	return false
}

// NegotiateKnowledge applies the criticality contract (Spec §3.4.1):
//
//	             | MCP-capable backend | non-MCP backend
//	  required   | deliver via tool    | FAIL (must pin) — never run degraded
//	  optional   | deliver via tool    | degrade + knowledge_unavailable
//
// It is pure: callers handle the FailRequired list (fail dispatch) and the
// UnavailableOptional list (emit the signal) themselves.
func NegotiateKnowledge(refs []KnowledgeRef, mcpCapable bool) KnowledgePlan {
	var plan KnowledgePlan
	for _, r := range refs {
		if mcpCapable {
			plan.DeliverPacks = append(plan.DeliverPacks, r.Pack)
			continue
		}
		switch r.Criticality {
		case CriticalityRequired:
			plan.FailRequired = append(plan.FailRequired, r.Pack)
		default: // optional
			plan.UnavailableOptional = append(plan.UnavailableOptional, r.Pack)
		}
	}
	return plan
}

// KnowledgeRefs returns the knowledge references a persona's manifest declares,
// or nil when the persona has no manifest (code-registered personas carry no
// knowledge). This is how the dispatch path discovers what to negotiate.
func (r *Registry) KnowledgeRefs(name string) []KnowledgeRef {
	r.mu.RLock()
	manifests := r.manifests
	r.mu.RUnlock()
	if manifests == nil {
		return nil
	}
	lp, ok := manifests.personas[name]
	if !ok {
		return nil
	}
	return lp.manifest.Knowledge
}

// BuildRetrievalMCPConfig produces the --mcp-config JSON that exposes the given
// resolved pack directories to an MCP-capable backend as a read-only knowledge
// source (the filesystem MCP server convention). Returns "" when there are no
// dirs. The handler threads this into backend.Request.MCPConfig.
//
// The server command is the standard MCP filesystem server; operators running
// required/optional knowledge on Claude must have it available. Delivery is
// progressive disclosure — the model retrieves pack files on demand rather than
// the packs being eagerly inlined.
func BuildRetrievalMCPConfig(packDirs []string) (string, error) {
	if len(packDirs) == 0 {
		return "", nil
	}
	type mcpServer struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	args := []string{"-y", "@modelcontextprotocol/server-filesystem"}
	args = append(args, packDirs...)
	cfg := map[string]any{
		"mcpServers": map[string]mcpServer{
			"knowledge": {Command: "npx", Args: args},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal knowledge mcp config: %w", err)
	}
	return string(b), nil
}
