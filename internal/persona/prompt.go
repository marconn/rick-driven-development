package persona

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

//go:embed phases/*.md
var phaseFS embed.FS

// PromptContext holds the accumulated state for building phase prompts.
// AI handlers populate this from the event store before calling Build.
type PromptContext struct {
	Task          string            // original user request (from WorkflowRequestedPayload.Prompt)
	Source        string            // source reference (e.g., "jira:PROJ-123", "gh:owner/repo#1")
	Outputs       map[string]string // previous phase outputs (phase name → output text)
	Feedback      string            // accumulated review/QA feedback for retry iterations
	Iteration     int               // current iteration (0-based)
	Ticket        string            // jira ticket ID (for commit phase)
	BaseBranch    string            // git base branch (for commit phase)
	WorkspacePath string                          // set by workspace persona, used as dynamic WorkDir
	Codebase      string                          // formatted codebase context (file tree, key files)
	Schema        string                          // formatted schema context (proto, SQL, GraphQL)
	GitContext    string                          // formatted git context (log, diff, modified files)
	Enrichments   []event.ContextEnrichmentPayload // library/component suggestions from enrichment hooks
}

// promptData is the template data structure matching v1's field names.
// This ensures backward compatibility with existing phase templates.
type promptData struct {
	Source           string // requirements / task description
	Research         string // output from research phase
	Architecture     string // output from architect phase
	Develop          string // output from develop phase
	PreviousDevelop  string // previous iteration's develop output (for feedback loops)
	Feedback         string // accumulated feedback text
	FeedbackAnalysis string // output from feedback-analyze phase (PR feedback triage)
	Ticket           string // jira ticket ID
	BaseBranch       string // git base branch
	WorkspacePath    string // isolated workspace path (set when running inside a Rick workspace)
	Codebase         string // codebase context (file tree, key files)
	Schema           string // schema context (proto, SQL, GraphQL)
	GitContext       string // git context (log, diff, modified files)
	Enrichments      string // library/component suggestions from enrichment hooks
}

// PromptBuilder constructs user prompts from phase templates and workflow context.
type PromptBuilder struct {
	customDir string // optional override directory for phase templates
}

// NewPromptBuilder creates a prompt builder.
func NewPromptBuilder() *PromptBuilder {
	return &PromptBuilder{}
}

// SetCustomDir sets the directory to check for custom phase template overrides.
// Templates in <dir>/phases/<phase>.md override the embedded defaults.
func (b *PromptBuilder) SetCustomDir(dir string) {
	b.customDir = dir
}

// Build assembles the user prompt for a given phase using the accumulated context.
func (b *PromptBuilder) Build(phase string, ctx PromptContext) (string, error) {
	tmplBytes, err := b.loadTemplate(phase)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New(phase).Parse(string(tmplBytes))
	if err != nil {
		return "", fmt.Errorf("parsing phase template for %s: %w", phase, err)
	}

	data := promptData{
		Source:           ctx.Task,
		Research:         ctx.Outputs["research"],
		Architecture:     ctx.Outputs["architect"],
		Develop:          ctx.Outputs["develop"],
		FeedbackAnalysis: ctx.Outputs["feedback-analyze"],
		Ticket:           ctx.Ticket,
		BaseBranch:       ctx.BaseBranch,
		WorkspacePath:    ctx.WorkspacePath,
		Codebase:         ctx.Codebase,
		Schema:           ctx.Schema,
		GitContext:       ctx.GitContext,
		Enrichments:      formatEnrichments(ctx.Enrichments),
	}

	// For develop iterations after the first, include feedback and previous output.
	if ctx.Feedback != "" {
		data.Feedback = ctx.Feedback
		data.PreviousDevelop = ctx.Outputs["develop"]
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing phase template for %s: %w", phase, err)
	}

	return strings.TrimSpace(buf.String()), nil
}

// loadTemplate returns the template bytes for a phase, checking custom dir first.
func (b *PromptBuilder) loadTemplate(phase string) ([]byte, error) {
	if b.customDir != "" {
		path := filepath.Join(b.customDir, "phases", phase+".md")
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return loadEmbeddedPhaseTemplate(phase)
}

// loadEmbeddedPhaseTemplate returns the built-in embedded phase template.
func loadEmbeddedPhaseTemplate(phase string) ([]byte, error) {
	data, err := phaseFS.ReadFile(fmt.Sprintf("phases/%s.md", phase))
	if err != nil {
		return nil, fmt.Errorf("loading embedded phase template for %s: %w", phase, err)
	}
	return data, nil
}

// formatEnrichments renders enrichment payloads as human-readable text
// for inclusion in prompt templates. Each enrichment source gets a section
// with its suggested libraries/components.
func formatEnrichments(enrichments []event.ContextEnrichmentPayload) string {
	if len(enrichments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range enrichments {
		if e.Summary != "" {
			fmt.Fprintf(&b, "## %s\n%s\n\n", e.Source, e.Summary)
		}
		for _, item := range e.Items {
			fmt.Fprintf(&b, "- **%s**", item.Name)
			if item.Version != "" {
				fmt.Fprintf(&b, " %s", item.Version)
			}
			if item.ImportPath != "" {
				fmt.Fprintf(&b, " (`%s`)", item.ImportPath)
			}
			fmt.Fprintf(&b, ": %s", item.Reason)
			if item.DocURL != "" {
				fmt.Fprintf(&b, " — %s", item.DocURL)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
