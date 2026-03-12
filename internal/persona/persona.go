package persona

import (
	"embed"
	"fmt"
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
)

// PhasePersona maps a workflow phase name to its default persona.
var PhasePersona = map[string]string{
	"research":  Researcher,
	"architect": Architect,
	"develop":   Developer,
	"review":    Reviewer,
	"qa":        QA,
	"commit":    Committer,
	"workspace":        Workspace,
	"feedback-analyze": FeedbackAnalyzer,
	"feedback-verify":  Reviewer,
}

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
		{Name: Researcher, Description: "Interdimensional Research Scout"},
		{Name: Architect, Description: "Multi-Dimensional Architect"},
		{Name: Developer, Description: "Staff Engineer Implementor"},
		{Name: Reviewer, Description: "PR Executioner"},
		{Name: QA, Description: "Quality Enforcement Officer"},
		{Name: Committer, Description: "Release Engineer"},
		{Name: Workspace, Description: "Git Workspace Provisioner"},
		{Name: ContextSnapshot, Description: "Codebase Context Snapshotter"},
		{Name: QAAnalyzer, Description: "QA Steps Generator"},
		{Name: FeedbackAnalyzer, Description: "PR Feedback Triage Analyst"},
		{Name: PRConsolidator, Description: "PR Review Consolidator"},
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

// LoadSystemPrompt reads the system prompt for the named persona.
// If a custom directory is set and contains <name>.md, that file is used.
// Otherwise, the embedded default is returned.
func (r *Registry) LoadSystemPrompt(name string) (string, error) {
	r.mu.RLock()
	_, ok := r.personas[name]
	customDir := r.customDir
	r.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("unknown persona: %s", name)
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
