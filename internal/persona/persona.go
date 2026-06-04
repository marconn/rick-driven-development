package persona

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

//go:embed prompts/*.md
var promptFS embed.FS

// Persona names for the built-in AI agents and non-AI provisioners.
const (
	Researcher       = "researcher"
	Architect        = "architect"
	Developer        = "developer"
	Reviewer         = "reviewer"
	QA               = "qa"
	QAAnalyzer       = "qa-analyzer"
	Committer        = "committer"
	Workspace        = "workspace"
	ContextSnapshot  = "context-snapshot"
	FeedbackAnalyzer = "feedback-analyzer"
	PRConsolidator   = "pr-consolidator"

	// PR category review personas — dedicated single-concern reviewers for pr-review workflow.
	PRSecurity         = "pr-security"
	PRConcurrency      = "pr-concurrency"
	PRErrorHandling    = "pr-error-handling"
	PRObservability    = "pr-observability"
	PRAPIContract      = "pr-api-contract"
	PRIdempotency      = "pr-idempotency"
	PRTesting          = "pr-testing"
	PRIntegration      = "pr-integration"
	PRPerformance      = "pr-performance"
	PRData             = "pr-data"
	PRHygiene          = "pr-hygiene"
	PRVendorResilience = "pr-vendor-resilience"
	PRDocsConcordance  = "pr-docs-concordance"

	// PR reply composer — text-only persona whose output Rick posts on its
	// behalf. Runs with Yolo=false (no tool access) to eliminate the
	// double-post failure mode where the LLM would run `gh pr comment` itself.
	PRReplier = "pr-replier"
)

// Persona defines an AI agent's identity.
type Persona struct {
	Name        string // unique identifier (e.g., "researcher")
	Description string // brief description for logging
}

// Registry holds available personas and loads their system prompts.
// Thread-safe for concurrent access.
type Registry struct {
	mu        sync.RWMutex
	personas  map[string]*Persona
	customDir string // optional override directory for system prompts
	// manifests is the data-driven persona source (RICK_PERSONA_MANIFESTS_DIR),
	// nil unless LoadManifests was called. When a persona name exists here, its
	// composed prompt (identity + skills) WINS over the embedded/code prompt —
	// this is what lets an operator override or recompose a persona without a
	// recompile. nil ⇒ byte-for-byte the prior code-only behavior.
	manifests *ManifestSource
}

// NewRegistry creates an empty persona registry.
func NewRegistry() *Registry {
	return &Registry{
		personas: make(map[string]*Persona),
	}
}

// DefaultRegistry returns a registry pre-loaded with all built-in personas.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	for _, p := range []Persona{
		{Name: Researcher, Description: "Requirements and codebase researcher"},
		{Name: Architect, Description: "Multi-Dimensional Architect"},
		{Name: Developer, Description: "Staff Engineer Implementor"},
		{Name: Reviewer, Description: "Implementation reviewer"},
		{Name: QA, Description: "QA reviewer"},
		{Name: Committer, Description: "Release Engineer"},
		{Name: Workspace, Description: "Git Workspace Provisioner"},
		{Name: ContextSnapshot, Description: "Codebase Context Snapshotter"},
		{Name: QAAnalyzer, Description: "QA scenario generator"},
		{Name: FeedbackAnalyzer, Description: "PR Feedback Triage Analyst"},
		{Name: PRConsolidator, Description: "PR Review Consolidator"},
		// PR category review personas.
		{Name: PRSecurity, Description: "Security Reviewer"},
		{Name: PRConcurrency, Description: "Concurrency Reviewer"},
		{Name: PRErrorHandling, Description: "Error Handling Reviewer"},
		{Name: PRObservability, Description: "Observability Reviewer"},
		{Name: PRAPIContract, Description: "API Contract Reviewer"},
		{Name: PRIdempotency, Description: "Idempotency Reviewer"},
		{Name: PRTesting, Description: "Testing Reviewer"},
		{Name: PRIntegration, Description: "Integration Reviewer"},
		{Name: PRPerformance, Description: "Performance Reviewer"},
		{Name: PRData, Description: "Data Integrity Reviewer"},
		{Name: PRHygiene, Description: "Code Hygiene Reviewer"},
		{Name: PRVendorResilience, Description: "Vendor Resilience Reviewer"},
		{Name: PRDocsConcordance, Description: "Docs/Code Concordance Reviewer"},
		{Name: PRReplier, Description: "PR Reply Composer"},
	} {
		_ = r.Register(&p)
	}
	return r
}

// SetCustomDir sets the directory to check for custom system prompt overrides.
// Files in <dir>/<persona>.md override the embedded defaults.
func (r *Registry) SetCustomDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.customDir = dir
}

// LoadManifests loads data-driven persona/skill manifests from root (the
// operator-local RICK_PERSONA_MANIFESTS_DIR) and merges them into the registry.
// Manifest personas WIN on name collision: a manifest named "developer"
// overrides the embedded developer prompt, with no recompile. Any persona named
// only by a manifest is registered so Get/Names see it too.
//
// Resilient by design: a malformed or invalid single manifest fails only
// itself — its error is logged and loading continues (the validator's
// "fail-one-not-the-process" contract). Returns an error only for a fatal
// problem reading the root itself. Calling with an empty root is a no-op, so
// callers can pass an unset env var unconditionally.
func (r *Registry) LoadManifests(root string, logger *slog.Logger) error {
	if root == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	src, errs := LoadManifestDir(root)
	for _, e := range errs {
		// Each is a single bad manifest — surface it loudly but never abort the
		// process; the rest of the registry stays usable.
		logger.Error("persona manifest rejected", slog.String("error", e.Error()))
	}

	r.mu.Lock()
	r.manifests = src
	r.mu.Unlock()

	// Register a Persona record for any manifest-only persona so Get/Names work.
	// Existing code-registered personas are left in place; manifest composition
	// still wins in LoadSystemPrompt regardless of which record is present.
	for _, name := range src.PersonaNames() {
		if _, err := r.Get(name); err != nil {
			_ = r.Register(&Persona{Name: name, Description: "manifest persona"})
		}
	}
	if n := len(src.PersonaNames()); n > 0 {
		logger.Info("loaded persona manifests",
			slog.String("dir", root), slog.Int("count", n))
	}
	return nil
}

// Register adds a persona to the registry.
func (r *Registry) Register(p *Persona) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.personas[p.Name]; exists {
		return fmt.Errorf("persona %q already registered", p.Name)
	}
	r.personas[p.Name] = p
	return nil
}

// Get returns a persona by name.
func (r *Registry) Get(name string) (*Persona, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.personas[name]
	if !ok {
		return nil, fmt.Errorf("unknown persona: %s", name)
	}
	return p, nil
}

// Names returns all registered persona names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.personas))
	for name := range r.personas {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// LoadSystemPrompt reads the system prompt for the named persona. Resolution
// precedence:
//
//  1. Manifest composition (identity + ordered skill fragments) when a persona
//     manifest of this name was loaded — the data-driven path. WINS over the
//     embedded/code prompt so an operator can override/recompose without a
//     recompile.
//  2. Custom-dir override file <customDir>/<name>.md, if present.
//  3. The embedded default prompt.
//
// This is the single shared persona-resolution path: every consumer
// (AIHandler, PRConsolidator, the rick_consult/rick_run MCP jobs) goes through
// it, so manifest composition applies uniformly without per-call-site wiring
// (F8). With no manifests loaded and no custom dir, behavior is byte-for-byte
// the prior embedded-only path.
func (r *Registry) LoadSystemPrompt(name string) (string, error) {
	r.mu.RLock()
	_, ok := r.personas[name]
	customDir := r.customDir
	manifests := r.manifests
	r.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("unknown persona: %s", name)
	}

	if manifests.Has(name) {
		// A composition error (e.g. a missing skill ref) is loud, not a silent
		// fallback to the embedded prompt — the operator authored a manifest and
		// must see why it failed rather than get surprising stale behavior.
		return manifests.ComposeSystemPrompt(name)
	}

	if customDir != "" {
		path := filepath.Join(customDir, name+".md")
		if data, err := os.ReadFile(path); err == nil {
			return string(data), nil
		}
	}

	return loadEmbeddedPrompt(name)
}

// loadEmbeddedPrompt returns the built-in embedded system prompt.
func loadEmbeddedPrompt(name string) (string, error) {
	data, err := promptFS.ReadFile(fmt.Sprintf("prompts/%s.md", name))
	if err != nil {
		return "", fmt.Errorf("loading embedded prompt for %s: %w", name, err)
	}
	return string(data), nil
}
