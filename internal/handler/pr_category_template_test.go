package handler

import (
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// TestPRCategoryReviewersResolveLoadablePhaseTemplate guards the class of bug
// that left pr-docs-concordance inert: a reviewer wired into the authoritative
// consolidator join list (prCategoryReviewerLabels) but missing from the
// pr-category-review case in defaultTemplate. Such a handler's template falls
// through to its own name, the builder looks for phases/<name>.md (which does
// not exist), and Handle fails at prompt-build on every dispatch — before the
// LLM is ever called — so it contributes zero reviews and zero grounding
// summaries while looking "wired" everywhere else.
//
// pr-docs-concordance shipped in 5d58f57 added to reviewAiCfg, the DAG, the
// consolidator list, and isVerdictBearingReviewer — but not defaultTemplate. It
// errored with "open phases/pr-docs-concordance.md: file does not exist" on
// every run. Binding the authoritative reviewer list to a real template load
// turns that omission into a test failure.
func TestPRCategoryReviewersResolveLoadablePhaseTemplate(t *testing.T) {
	builder := persona.NewPromptBuilder()
	for _, r := range prCategoryReviewerLabels {
		t.Run(r.key, func(t *testing.T) {
			tmpl := defaultTemplate(r.key)
			// Empty context is sufficient: the shared pr-category-review
			// template references only optional fields ({{.Source}},
			// {{if .Enrichments}}), so a successful load+parse+execute proves
			// the phase file exists and is valid.
			if _, err := builder.Build(tmpl, persona.PromptContext{}); err != nil {
				t.Fatalf("reviewer %q maps to phase template %q which fails to load: %v\n"+
					"fix: add %q to the pr-category-review case in defaultTemplate (ai.go), "+
					"or add internal/persona/phases/%s.md", r.key, tmpl, err, r.key, tmpl)
			}
		})
	}
}
