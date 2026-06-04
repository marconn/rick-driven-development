package persona

import (
	"strings"
	"testing"
)

// prReviewerManifests is the authoritative list of category reviewers migrated
// to manifests (mirrors the prCategoryReviewerLabels in the handler package).
var prReviewerManifests = []string{
	"pr-security", "pr-concurrency", "pr-error-handling", "pr-observability",
	"pr-api-contract", "pr-idempotency", "pr-testing", "pr-integration",
	"pr-performance", "pr-data", "pr-hygiene", "pr-vendor-resilience",
	"pr-docs-concordance",
}

// extractSection returns the lines of the named "## <header>" section of an
// embedded prompt, up to the next "## " header. Used to pull a reviewer's
// domain rules for the equivalence check.
func extractSection(prompt, header string) []string {
	lines := strings.Split(prompt, "\n")
	var out []string
	in := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			in = strings.TrimSpace(strings.TrimPrefix(ln, "##")) == header
			continue
		}
		if in {
			if s := strings.TrimSpace(ln); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// TestPRReviewerManifestsEquivalent is the migration safety net: each migrated
// reviewer's COMPOSED prompt (identity + shared skills) must be a SUPERSET of
// its prior embedded prompt's domain rules — the "## Your Domain (ONLY these)"
// and "## Severity Guide" sections, which carry the reviewer's actual scope.
// A regression here would silently narrow a reviewer's coverage.
func TestPRReviewerManifestsEquivalent(t *testing.T) {
	// Manifests live in-repo under ./personas and ./skills; go test runs with
	// the package dir as CWD.
	src, errs := LoadManifestDir(".")
	if len(errs) != 0 {
		t.Fatalf("manifest load errors: %v", errs)
	}

	for _, name := range prReviewerManifests {
		t.Run(name, func(t *testing.T) {
			if !src.Has(name) {
				t.Fatalf("manifest %q not found under ./personas", name)
			}
			composed, err := src.ComposeSystemPrompt(name)
			if err != nil {
				t.Fatalf("compose %q: %v", name, err)
			}
			embedded, err := loadEmbeddedPrompt(name)
			if err != nil {
				t.Fatalf("load embedded %q: %v", name, err)
			}

			// Every domain-rule line from the embedded prompt must survive into
			// the composed prompt.
			var domainLines []string
			domainLines = append(domainLines, extractSection(embedded, "Your Domain (ONLY these)")...)
			domainLines = append(domainLines, extractSection(embedded, "Severity Guide")...)
			if len(domainLines) == 0 {
				// vendor-resilience / docs-concordance use a different structure;
				// fall back to requiring the identity headline survives.
				if !strings.Contains(composed, "RickAI Persona") {
					t.Errorf("%s: composed prompt lost its persona identity", name)
				}
				return
			}
			for _, line := range domainLines {
				if !strings.Contains(composed, line) {
					t.Errorf("%s: composed prompt dropped a domain rule:\n  %q", name, line)
				}
			}

			// The shared skills must be composed in (the dedup target).
			if !strings.Contains(composed, "cite the exact file and line") {
				t.Errorf("%s: diff-grounding skill not composed in", name)
			}
			if !strings.Contains(composed, "PASS") {
				t.Errorf("%s: domain-boundary skill (PASS contract) not composed in", name)
			}
		})
	}
}

// suppressionMarkers are phrases that instruct the model to WITHHOLD a finding
// (as opposed to scope rules like "do not flag outside your domain", which are
// legitimate and present in the embedded prompts). A skill must never introduce
// one of these — grounding/scoping is enforced by the code-side filter, not by
// telling the LLM to self-censor. This guards the regression where the
// diff-grounding skill added "if you cannot point at a changed line, do not
// raise it", which dropped reviewer candidate-finding rate ~8x in production.
var suppressionMarkers = []string{
	"do not raise", "don't raise", "do not report", "do not surface",
	"withhold", "suppress", "stay silent", "do not mention",
}

// TestPRReviewerManifestsNoNewSuppression is the additions-catching counterpart
// to the superset equivalence test: a migrated reviewer's COMPOSED prompt must
// not contain a finding-suppression instruction that its embedded prompt did
// not already have. Superset tests catch removed domain rules; this catches
// harmful added instructions — the class that silently narrowed engagement and
// that only production telemetry caught the first time.
func TestPRReviewerManifestsNoNewSuppression(t *testing.T) {
	src, errs := LoadManifestDir(".")
	if len(errs) != 0 {
		t.Fatalf("manifest load errors: %v", errs)
	}
	for _, name := range prReviewerManifests {
		t.Run(name, func(t *testing.T) {
			composed, err := src.ComposeSystemPrompt(name)
			if err != nil {
				t.Fatalf("compose %q: %v", name, err)
			}
			embedded, err := loadEmbeddedPrompt(name)
			if err != nil {
				t.Fatalf("load embedded %q: %v", name, err)
			}
			lc, le := strings.ToLower(composed), strings.ToLower(embedded)
			for _, m := range suppressionMarkers {
				if strings.Contains(lc, m) && !strings.Contains(le, m) {
					t.Errorf("%s: composed prompt introduces a finding-suppression instruction %q "+
						"absent from the embedded prompt — skills describe HOW to review, they must not "+
						"tell the model to withhold findings (the grounding filter does that in code)", name, m)
				}
			}
		})
	}
}

// TestPRReviewerBoilerplateDedupedOnce asserts the shared boilerplate exists
// ONCE (in the skills), not copy-pasted across the migrated identities: the
// bare generic citation line must not remain in any migrated persona identity.
func TestPRReviewerBoilerplateDedupedOnce(t *testing.T) {
	src, errs := LoadManifestDir(".")
	if len(errs) != 0 {
		t.Fatalf("manifest load errors: %v", errs)
	}
	const generic = "- Every finding must cite the exact file and line"
	for _, name := range prReviewerManifests {
		lp := src.personas[name]
		// The identity body (not the composed prompt) must not carry the bare
		// generic line — it now lives only in the diff-grounding skill.
		for _, bodyLine := range strings.Split(lp.manifest.Body, "\n") {
			if strings.TrimSpace(bodyLine) == strings.TrimSpace(generic) {
				t.Errorf("%s identity still contains the bare generic citation line; "+
					"it should be deduped into the diff-grounding skill", name)
			}
		}
	}
	// And the skill DOES carry it (exists once).
	skill := src.skills["diff-grounding"]
	if skill == nil || !strings.Contains(skill.Body, "cite the exact file and line") {
		t.Fatal("diff-grounding skill must hold the shared citation rule")
	}
}
