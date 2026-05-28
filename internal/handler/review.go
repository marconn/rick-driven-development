package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ReviewHandler wraps an AIHandler to add verdict parsing for review/QA phases.
// After calling the AI backend, it parses VERDICT: PASS/FAIL from the response
// and emits a VerdictRendered event alongside the standard AI events.
type ReviewHandler struct {
	ai            *AIHandler
	targetPersona string // persona to send feedback to on failure (e.g., "developer")
}

// ReviewHandlerConfig configures a review handler.
type ReviewHandlerConfig struct {
	AIConfig      AIHandlerConfig
	TargetPersona string // handler that should be rescheduled on fail (e.g., "developer")
}

// NewReviewHandler creates a handler that parses verdicts from AI responses.
func NewReviewHandler(cfg ReviewHandlerConfig) *ReviewHandler {
	return &ReviewHandler{
		ai:            NewAIHandler(cfg.AIConfig),
		targetPersona: cfg.TargetPersona,
	}
}

func (h *ReviewHandler) Name() string             { return h.ai.Name() }
func (h *ReviewHandler) Subscribes() []event.Type { return nil }

// isPRCategoryReviewer reports whether this reviewer participates in the
// pr-review fan-out and therefore needs the diff-grounding pass over its
// findings before a verdict is emitted.
func (h *ReviewHandler) isPRCategoryReviewer() bool {
	switch h.ai.name {
	case "pr-security", "pr-concurrency", "pr-error-handling",
		"pr-observability", "pr-api-contract", "pr-idempotency",
		"pr-testing", "pr-integration", "pr-performance",
		"pr-data", "pr-hygiene", "pr-vendor-resilience",
		"pr-docs-concordance":
		return true
	}
	return false
}

// Handle calls the AI backend, parses the verdict, and returns AI events
// plus a VerdictRendered event. For pr-category-review handlers, also captures
// the original LLM output before grounding rewrites it (forensics) and emits
// a VerdictGroundingSummary event recording how many findings survived the
// diff-anchoring filter.
func (h *ReviewHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	aiEvents, err := h.ai.Handle(ctx, env)
	if err != nil {
		return nil, err
	}

	// Extract AI response text from the AIResponseReceived event. rawText is
	// captured here, before grounding may rewrite responseText to the canned
	// "no grounded issues" string — preserved for post-mortem via OutputRaw.
	responseText := h.extractResponseText(aiEvents)
	rawText := responseText

	verdict := ParseVerdict(responseText)
	issues := ParseIssues(responseText, verdict.Outcome)
	var summaryEvt *event.Envelope
	if h.isPRCategoryReviewer() {
		responseText, verdict, issues, summaryEvt = h.groundPRCategoryReview(ctx, env, responseText, verdict, issues)
		aiEvents = rewriteAIResponseText(aiEvents, responseText, rawText)
	}

	verdictEvt := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Persona:       h.targetPersona,
		SourcePersona: h.ai.name,
		Outcome:       verdict.Outcome,
		Issues:        issues,
		Summary:       verdict.Summary,
		Source:        verdict.Source,
		// DevTriggerID is the developer PersonaCompleted ID that triggered
		// this review. The review-consolidator joins reviewer+qa verdicts by
		// this key so it never merges across developer iterations.
		DevTriggerID: string(env.ID),
	})).WithSource("handler:" + h.ai.name)

	out := append(aiEvents, verdictEvt)
	if summaryEvt != nil {
		out = append(out, *summaryEvt)
	}
	return out, nil
}

// extractResponseText gets the plain text from the AIResponseReceived event.
func (h *ReviewHandler) extractResponseText(events []event.Envelope) string {
	for _, e := range events {
		if e.Type != event.AIResponseReceived {
			continue
		}
		var p event.AIResponsePayload
		if err := unmarshalPayload(e.Payload, &p); err != nil {
			continue
		}
		return unmarshalOutput(p.Output, p.Structured)
	}
	return ""
}

// Verdict holds the parsed result from AI review output. Source classifies
// the parser path that produced this verdict — populated by ParseVerdict and,
// for pr-category-review handlers, possibly overridden by groundPRCategoryReview
// when an explicit FAIL is demoted to PASS by the grounding filter.
type Verdict struct {
	Outcome event.VerdictOutcome
	Summary string
	Source  event.VerdictSource
}

type prDiffGroundingScope struct {
	changedFiles map[string]struct{}
	changedLines map[string]map[int]string
}

// ParseVerdict extracts VERDICT: PASS or VERDICT: FAIL from AI output.
// Defaults to VerdictPass if no verdict line is found, and stamps Source with
// VerdictSourceDefaultOptimistic so operators can detect the malformed-output
// path post-mortem.
func ParseVerdict(text string) Verdict {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		upper := strings.ToUpper(line)

		if strings.Contains(upper, "VERDICT:") {
			if strings.Contains(upper, "FAIL") {
				summary := extractSummary(lines, i)
				return Verdict{Outcome: event.VerdictFail, Summary: summary, Source: event.VerdictSourceExplicitFail}
			}
			if strings.Contains(upper, "PASS") {
				return Verdict{Outcome: event.VerdictPass, Summary: "passed review", Source: event.VerdictSourceExplicitPass}
			}
		}
	}
	// No explicit verdict — default to pass (optimistic). The Source field is
	// the single most actionable forensics signal: any reviewer producing
	// default_optimistic verdicts is bailing without a verdict line.
	return Verdict{
		Outcome: event.VerdictPass,
		Summary: "no explicit verdict found; defaulting to pass",
		Source:  event.VerdictSourceDefaultOptimistic,
	}
}

// extractSummary collects text around the verdict line for a brief summary.
func extractSummary(lines []string, verdictIdx int) string {
	// Look for a summary in the lines before the verdict
	for i := verdictIdx - 1; i >= 0 && i >= verdictIdx-5; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "```") && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return "review failed"
}

// numberedIssueRe matches lines like "1. Missing error handling" or "- Missing error handling"
var numberedIssueRe = regexp.MustCompile(`^\s*(?:\d+[\.\)]\s*|-\s+)(.+)`)

// ParseIssues extracts structured issues from AI output following a FAIL verdict.
// It looks for numbered/bulleted lists after the VERDICT: FAIL line.
func ParseIssues(text string, outcome event.VerdictOutcome) []event.Issue {
	if outcome != event.VerdictFail {
		return nil
	}

	lines := strings.Split(text, "\n")

	// Find the verdict line
	verdictIdx := -1
	for i, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if strings.Contains(upper, "VERDICT:") && strings.Contains(upper, "FAIL") {
			verdictIdx = i
			break
		}
	}
	if verdictIdx < 0 {
		return nil
	}

	var issues []event.Issue
	for i := verdictIdx + 1; i < len(lines); i++ {
		match := numberedIssueRe.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}
		description := strings.TrimSpace(match[1])
		if description == "" {
			continue
		}

		issue := event.Issue{
			Severity:    classifySeverity(description),
			Category:    classifyCategory(description),
			Description: description,
		}

		// Try to extract file:line references
		file, line := extractFileRef(description)
		issue.File = file
		issue.Line = line

		issues = append(issues, issue)
	}
	return issues
}

// classifySeverity assigns severity based on keywords in the description.
func classifySeverity(desc string) string {
	lower := strings.ToLower(desc)
	switch {
	case strings.Contains(lower, "critical") || strings.Contains(lower, "security") ||
		strings.Contains(lower, "injection") || strings.Contains(lower, "vulnerability") ||
		strings.Contains(lower, "credential") || strings.Contains(lower, "xss") ||
		strings.Contains(lower, "deadlock") || strings.Contains(lower, "data loss"):
		return "critical"
	case strings.Contains(lower, "missing") || strings.Contains(lower, "error handling") ||
		strings.Contains(lower, "race condition") || strings.Contains(lower, "breaking change") ||
		strings.Contains(lower, "goroutine leak") || strings.Contains(lower, "silent fail") ||
		strings.Contains(lower, "partial write"):
		return "major"
	default:
		return "minor"
	}
}

// classifyCategory assigns category based on keywords in the description.
func classifyCategory(desc string) string {
	lower := strings.ToLower(desc)
	switch {
	case strings.Contains(lower, "security") || strings.Contains(lower, "injection") ||
		strings.Contains(lower, "auth") || strings.Contains(lower, "vulnerability") ||
		strings.Contains(lower, "credential") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "xss") || strings.Contains(lower, "csrf"):
		return "security"
	case strings.Contains(lower, "race condition") || strings.Contains(lower, "deadlock") ||
		strings.Contains(lower, "mutex") || strings.Contains(lower, "goroutine leak") ||
		strings.Contains(lower, "channel") || strings.Contains(lower, "concurrent") ||
		strings.Contains(lower, "synchronization") || strings.Contains(lower, "toctou") ||
		strings.Contains(lower, "concurrent map"):
		return "concurrency"
	case strings.Contains(lower, "error handling") || strings.Contains(lower, "swallowed error") ||
		strings.Contains(lower, "unwrapped") || strings.Contains(lower, "naked return") ||
		strings.Contains(lower, "missing context") || strings.Contains(lower, "err != nil") ||
		strings.Contains(lower, "error ignored") || strings.Contains(lower, "bare log"):
		return "error_handling"
	case strings.Contains(lower, "observability") || strings.Contains(lower, "logging") ||
		strings.Contains(lower, "tracing") || strings.Contains(lower, "metric") ||
		strings.Contains(lower, "silent fail") || strings.Contains(lower, "correlation") ||
		strings.Contains(lower, "debug") || strings.Contains(lower, "monitor"):
		return "observability"
	case strings.Contains(lower, "breaking change") || strings.Contains(lower, "api contract") ||
		strings.Contains(lower, "backward compat") || strings.Contains(lower, "removed field") ||
		strings.Contains(lower, "response shape") || strings.Contains(lower, "status code") ||
		strings.Contains(lower, "proto") || strings.Contains(lower, "schema break"):
		return "api_contract"
	case strings.Contains(lower, "idempoten") || strings.Contains(lower, "dedup") ||
		strings.Contains(lower, "retry-unsafe") || strings.Contains(lower, "replay"):
		return "idempotency"
	case strings.Contains(lower, "integration") || strings.Contains(lower, "contract test") ||
		strings.Contains(lower, "end-to-end") || strings.Contains(lower, "e2e"):
		return "integration"
	case strings.Contains(lower, "data integrity") || strings.Contains(lower, "migration") ||
		strings.Contains(lower, "partial write") || strings.Contains(lower, "data loss") ||
		strings.Contains(lower, "rollback") || strings.Contains(lower, "schema migration") ||
		strings.Contains(lower, "orphan"):
		return "data"
	case strings.Contains(lower, "test") || strings.Contains(lower, "coverage"):
		return "testing"
	case strings.Contains(lower, "performance") || strings.Contains(lower, "n+1") ||
		strings.Contains(lower, "index") || strings.Contains(lower, "latency") ||
		strings.Contains(lower, "unbounded") || strings.Contains(lower, "slow query"):
		return "performance"
	case strings.Contains(lower, "naming") || strings.Contains(lower, "style") ||
		strings.Contains(lower, "format") || strings.Contains(lower, "code smell") ||
		strings.Contains(lower, "dead code") || strings.Contains(lower, "magic number") ||
		strings.Contains(lower, "complexity") || strings.Contains(lower, "anti-pattern"):
		return "good_hygiene"
	default:
		return "correctness"
	}
}

// fileRefRe matches patterns like "handler.go:42" or "in handler.go line 42"
var (
	fileColonLineRefRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_./-]+(?:\.[A-Za-z0-9_-]+)?|Makefile):(\d+)\b`)
	fileLineWordRefRe  = regexp.MustCompile("(?i)(`?([A-Za-z0-9_./-]+(?:\\.[A-Za-z0-9_-]+)?|Makefile)`?)\\s+line[s]?\\s+(\\d+)")
	lineWordRefRe      = regexp.MustCompile(`(?i)\bline[s]?\s+(\d+)\b`)
	codeSpanRefRe      = regexp.MustCompile("`([^`]+)`")
	hunkHeaderRe       = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// extractFileRef tries to find a file:line reference in the description.
func extractFileRef(desc string) (string, int) {
	if match := fileColonLineRefRe.FindStringSubmatch(desc); match != nil {
		return match[1], parseLineNumber(match[2])
	}
	if match := fileLineWordRefRe.FindStringSubmatch(desc); match != nil {
		return strings.Trim(match[2], "`"), parseLineNumber(match[3])
	}

	line := 0
	if match := lineWordRefRe.FindStringSubmatch(desc); match != nil {
		line = parseLineNumber(match[1])
	}

	for _, match := range codeSpanRefRe.FindAllStringSubmatch(desc, -1) {
		candidate := strings.TrimSpace(match[1])
		if looksLikeSourceRef(candidate) {
			return candidate, line
		}
	}

	// Fall back to unquoted file-like references such as "Makefile".
	for _, token := range strings.Fields(desc) {
		candidate := strings.Trim(token, "`*()[]{}:,.")
		if looksLikeSourceRef(candidate) {
			return candidate, line
		}
	}

	return "", line
}

func parseLineNumber(raw string) int {
	line := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			break
		}
		line = line*10 + int(c-'0')
	}
	return line
}

func looksLikeSourceRef(candidate string) bool {
	if candidate == "" {
		return false
	}
	if candidate == "Makefile" {
		return true
	}
	switch strings.ToLower(filepath.Ext(candidate)) {
	case ".go", ".sh", ".md", ".yaml", ".yml", ".toml", ".json", ".ts", ".tsx", ".js", ".jsx", ".sql", ".proto":
		return true
	default:
		return false
	}
}

func (h *ReviewHandler) groundPRCategoryReview(
	ctx context.Context,
	env event.Envelope,
	responseText string,
	verdict Verdict,
	issues []event.Issue,
) (string, Verdict, []event.Issue, *event.Envelope) {
	scope, ok := h.loadPRDiffGroundingScope(ctx, env.CorrelationID)
	if !ok {
		// No diff scope available — emit a summary so the absence of grounding
		// is itself recorded, then return inputs unchanged.
		return responseText, verdict, issues, h.buildGroundingSummary(verdict, verdict, len(issues), len(issues), 0, nil)
	}

	originalOutcome := verdict.Outcome
	originalCount := len(issues)
	dropReasons := make(map[event.GroundingDropReason]int)

	rescued := 0
	grounded := make([]event.Issue, 0, len(issues))
	for _, issue := range issues {
		gi, ok, reason := scope.groundIssue(issue)
		if ok {
			grounded = append(grounded, gi)
			if reason == event.GroundingRescuedFileScope {
				rescued++
				dropReasons[reason]++
			}
			continue
		}
		dropReasons[reason]++
	}

	if verdict.Outcome == event.VerdictFail && len(grounded) == 0 {
		// Demotion: every FAIL finding was rejected by grounding. Stamp a
		// dedicated Source so operators can spot the demoted path without
		// reading OutputRaw.
		verdict = Verdict{
			Outcome: event.VerdictPass,
			Summary: "no grounded issues found in the changed lines for this review category",
			Source:  event.VerdictSourceDowngradedNoGrounded,
		}
	}
	if verdict.Outcome != event.VerdictFail {
		grounded = nil
	}

	summaryEvt := h.buildGroundingSummary(Verdict{Outcome: originalOutcome}, verdict, originalCount, len(grounded), rescued, dropReasons)
	return buildCompactPRCategoryReviewOutput(verdict, grounded), verdict, grounded, summaryEvt
}

// buildGroundingSummary constructs the VerdictGroundingSummary envelope.
// Always returns a non-nil envelope — caller appends unconditionally so the
// presence/absence of summary events is itself a code-path bug indicator.
// rescued is the count of issues accepted via the file-scope rescue path
// (GroundingRescuedFileScope); included in RescuedCount for operator visibility.
func (h *ReviewHandler) buildGroundingSummary(
	original Verdict,
	final Verdict,
	originalCount int,
	groundedCount int,
	rescued int,
	dropReasons map[event.GroundingDropReason]int,
) *event.Envelope {
	if len(dropReasons) == 0 {
		dropReasons = nil // honor omitempty — empty map is noise
	}
	evt := event.New(event.VerdictGroundingSummary, 1, event.MustMarshal(event.VerdictGroundingSummaryPayload{
		Reviewer:        h.ai.name,
		OriginalCount:   originalCount,
		GroundedCount:   groundedCount,
		RescuedCount:    rescued,
		DropReasons:     dropReasons,
		OriginalOutcome: original.Outcome,
		FinalOutcome:    final.Outcome,
	})).WithSource("handler:" + h.ai.name)
	return &evt
}

func (h *ReviewHandler) loadPRDiffGroundingScope(ctx context.Context, correlationID string) (prDiffGroundingScope, bool) {
	if h.ai.store == nil || correlationID == "" {
		return prDiffGroundingScope{}, false
	}
	events, err := h.ai.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return prDiffGroundingScope{}, false
	}
	for _, env := range events {
		if env.Type != event.ContextEnrichment {
			continue
		}
		var payload event.ContextEnrichmentPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			continue
		}
		if payload.Kind != "pr-diff" || payload.Summary == "" {
			continue
		}
		return parsePRDiffGroundingScope(payload.Summary)
	}
	return prDiffGroundingScope{}, false
}

func parsePRDiffGroundingScope(summary string) (prDiffGroundingScope, bool) {
	scope := prDiffGroundingScope{
		changedFiles: make(map[string]struct{}),
		changedLines: make(map[string]map[int]string),
	}

	lines := strings.Split(summary, "\n")
	inFileList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "## PR Changed Files":
			inFileList = true
			continue
		case "## PR Diff":
			inFileList = false
			continue
		}
		if inFileList && strings.HasPrefix(trimmed, "- `") && strings.HasSuffix(trimmed, "`") {
			file := strings.TrimSuffix(strings.TrimPrefix(trimmed, "- `"), "`")
			scope.changedFiles[file] = struct{}{}
		}
	}

	diff := extractDiffBlock(summary)
	if diff == "" {
		return scope, len(scope.changedFiles) > 0
	}

	parseUnifiedDiff(&scope, diff)
	return scope, len(scope.changedFiles) > 0 || len(scope.changedLines) > 0
}

func extractDiffBlock(summary string) string {
	start := strings.Index(summary, "```diff\n")
	if start < 0 {
		return ""
	}
	start += len("```diff\n")
	end := strings.Index(summary[start:], "\n```")
	if end < 0 {
		return ""
	}
	return summary[start : start+end]
}

func parseUnifiedDiff(scope *prDiffGroundingScope, diff string) {
	var currentFile string
	currentNewLine := 0

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			currentFile = strings.TrimPrefix(line, "+++ b/")
			scope.changedFiles[currentFile] = struct{}{}
		case strings.HasPrefix(line, "@@ "):
			match := hunkHeaderRe.FindStringSubmatch(line)
			if match == nil {
				currentNewLine = 0
				continue
			}
			currentNewLine = parseLineNumber(match[1])
		case currentFile == "" || currentNewLine == 0:
			continue
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if _, ok := scope.changedLines[currentFile]; !ok {
				scope.changedLines[currentFile] = make(map[int]string)
			}
			scope.changedLines[currentFile][currentNewLine] = strings.TrimSpace(line[1:])
			currentNewLine++
		case strings.HasPrefix(line, " "):
			currentNewLine++
			// Removed lines ("-") and "\ No newline at end of file" markers
			// fall through — they must not advance the new-file cursor.
		}
	}
}

// groundIssue checks whether issue anchors to an exact changed line in the
// PR diff. Returns (issue, true, "") when grounded; (zero, false, reason) when
// dropped, where reason classifies why for the VerdictGroundingSummary tally.
// When the cited line is not changed but the token appears elsewhere in the
// file's changed lines, the issue is rescued: returned with Line=0 and reason
// GroundingRescuedFileScope so the consolidator emits it as an unanchored body
// bullet rather than an inline comment at a hallucinated line.
func (s prDiffGroundingScope) groundIssue(issue event.Issue) (event.Issue, bool, event.GroundingDropReason) {
	if issue.File == "" {
		issue.File, issue.Line = extractFileRef(issue.Description)
	}
	file := s.resolveFile(issue.File)
	if file == "" || issue.Line <= 0 {
		return event.Issue{}, false, event.GroundingDropFileNotInScope
	}
	if _, hasLines := s.changedLines[file]; !hasLines {
		return event.Issue{}, false, event.GroundingDropLineNotInChanged
	}
	if !s.matchesChangedLine(file, issue.Line, issue.Description) {
		// Try file-scope rescue on both sub-cases: token_not_near_line (the
		// cited line IS changed but the token isn't within ±1) and
		// line_not_in_changed (the cited line isn't changed at all). In both
		// cases the token is verifiably in the diff — rescue clears Line=0 so
		// the consolidator emits an unanchored bullet, never an inline comment
		// at a wrong location. Drop-reason classification only applies when
		// rescue fails, preserving the existing forensic taxonomy unchanged.
		if rescued, ok := s.rescueByFileScope(issue); ok {
			return rescued, true, event.GroundingRescuedFileScope
		}
		// Distinguish "line is changed but token didn't appear nearby" from
		// "line not changed at all" — both useful forensic signal.
		if _, lineChanged := s.changedLines[file][issue.Line]; lineChanged {
			return event.Issue{}, false, event.GroundingDropTokenNotNearLine
		}
		return event.Issue{}, false, event.GroundingDropLineNotInChanged
	}
	issue.File = file
	return issue, true, ""
}

// rescueByFileScope tries to recover an issue whose cited line wasn't
// changed but whose backtick token DOES appear somewhere in the changed
// lines for that file. Returns the issue with Line cleared (forcing the
// consolidator to emit it as an unanchored body bullet, never an inline
// comment at a hallucinated location).
//
// Semantics: requires at least one identifier-shaped token from
// groundingTokens(); accepts the issue if ANY such identifier token
// appears in the file's changed-line blob (not ALL tokens — the LLM
// commonly mixes one real code reference with several illustrative
// example values in backticks). Non-identifier tokens (e.g.
// "LOG_LEVEL=infio", "info", "debug") are skipped. File-allowlist
// guard already passed before this is called — this method only relaxes
// the line guard.
func (s prDiffGroundingScope) rescueByFileScope(issue event.Issue) (event.Issue, bool) {
	file := s.resolveFile(issue.File)
	if file == "" {
		return event.Issue{}, false
	}
	lines, ok := s.changedLines[file]
	if !ok || len(lines) == 0 {
		return event.Issue{}, false
	}
	tokens := groundingTokens(issue.Description)
	if len(tokens) == 0 {
		return event.Issue{}, false
	}
	var fileText strings.Builder
	for _, text := range lines {
		fileText.WriteString(text)
		fileText.WriteByte('\n')
	}
	blob := fileText.String()
	// At least one anchor-quality identifier token must appear somewhere in
	// the file's changed lines. "Anchor-quality" means:
	//   1. The token matches identifierLikeTokenRe (identifier shape), AND
	//   2. It is at least 8 characters long.
	//
	// The length gate filters out short, common prose words wrapped in
	// backticks for emphasis ("info", "debug", "mise" — all ≤ 5 chars)
	// that are too ambiguous to safely use as diff-blob substring anchors.
	// Real code references like "slog.LevelInfo" (14), "REDUCE_SUM" (10),
	// "CreateObservabilityResources" (30), "io.Copy(&buf, r)" (16) all
	// comfortably exceed the threshold.
	//
	// We require ANY match, not ALL: the LLM frequently mixes one real
	// code reference with several illustrative example values inside
	// backticks. The Line=0 fallback contract makes the looser check safe
	// (no inline comment lands at a hallucinated line).
	const minAnchorTokenLen = 8
	hasIdentifierToken := false
	for _, tok := range tokens {
		if !identifierLikeTokenRe.MatchString(tok) {
			continue
		}
		if len(tok) < minAnchorTokenLen {
			continue
		}
		hasIdentifierToken = true
		if strings.Contains(blob, tok) {
			issue.File = file
			issue.Line = 0
			// Strip backtick-wrapped file:line citation tokens from the
			// description so the compact output doesn't carry the hallucinated
			// line number. The pattern matches any `file.ext:N` span that
			// groundingTokens would have skipped.
			issue.Description = codeSpanRefRe.ReplaceAllStringFunc(issue.Description, func(span string) string {
				inner := strings.TrimSpace(span[1 : len(span)-1]) // strip outer backticks
				if fileLineTokenRe.MatchString(inner) {
					return ""
				}
				return span
			})
			issue.Description = strings.TrimSpace(issue.Description)
			return issue, true
		}
	}
	if !hasIdentifierToken {
		// No identifier-shaped token at all — can't safely rescue. The
		// finding may be valid prose, but we have no anchor that would let
		// the consolidator validate it.
		return event.Issue{}, false
	}
	// All identifier tokens were present but none matched the diff blob.
	return event.Issue{}, false
}

func (s prDiffGroundingScope) resolveFile(ref string) string {
	ref = strings.Trim(ref, "` ")
	if ref == "" {
		return ""
	}
	if _, ok := s.changedFiles[ref]; ok {
		return ref
	}

	match := ""
	base := filepath.Base(ref)
	for candidate := range s.changedFiles {
		switch {
		case strings.HasSuffix(candidate, "/"+ref):
			return candidate
		case filepath.Base(candidate) == base:
			if match != "" && match != candidate {
				return ""
			}
			match = candidate
		}
	}
	return match
}

func (s prDiffGroundingScope) matchesChangedLine(file string, line int, description string) bool {
	lines, ok := s.changedLines[file]
	if !ok {
		return false
	}

	var nearby strings.Builder
	for n := line - 1; n <= line+1; n++ {
		if text, ok := lines[n]; ok {
			nearby.WriteString(text)
			nearby.WriteByte('\n')
		}
	}
	nearbyText := nearby.String()
	if nearbyText == "" {
		return false
	}

	tokens := groundingTokens(description)
	if len(tokens) == 0 {
		return true
	}
	for _, token := range tokens {
		if !strings.Contains(nearbyText, token) {
			return false
		}
	}
	return true
}

// fileLineTokenRe matches tokens that are themselves file:line references
// (e.g. "monitoring_observability.go:81") — these are location citations, not
// code identifiers, and must not be used as blob-search tokens.
var fileLineTokenRe = regexp.MustCompile(`^[^/]+\.\w+:\d+$`)

// identifierLikeTokenRe matches tokens that look like Go/JS/Python
// identifiers (with optional dotted package qualifiers and optional
// parenthesized arg lists), e.g. "slog.LevelInfo", "io.Copy(&buf, r)",
// "viper.BindEnv". Used by the rescue path to distinguish real code
// references from prose-only values like "infio" or "info" wrapped in
// backticks for emphasis.
var identifierLikeTokenRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*(?:\([^)]*\))?$`)

func groundingTokens(description string) []string {
	matches := codeSpanRefRe.FindAllStringSubmatch(description, -1)
	tokens := make([]string, 0, len(matches))
	for _, match := range matches {
		token := strings.TrimSpace(match[1])
		if token == "" || token == "Makefile" || strings.Contains(token, "/") {
			continue
		}
		// Skip file:line citation tokens — they are location references, not
		// code identifiers that would appear verbatim in the changed-line text.
		if fileLineTokenRe.MatchString(token) {
			continue
		}
		tokens = append(tokens, token)
	}
	return tokens
}

func buildCompactPRCategoryReviewOutput(verdict Verdict, issues []event.Issue) string {
	if verdict.Outcome != event.VerdictFail || len(issues) == 0 {
		return "No grounded issues found in the changed lines for this review category.\n\nVERDICT: PASS"
	}

	var b strings.Builder
	if verdict.Summary != "" {
		b.WriteString(verdict.Summary)
		b.WriteString("\n\n")
	}
	b.WriteString("VERDICT: FAIL\n\n")
	for i, issue := range issues {
		fmt.Fprintf(&b, "%d. %s\n", i+1, compactIssueDescription(issue))
	}
	return strings.TrimSpace(b.String())
}

func compactIssueDescription(issue event.Issue) string {
	location := issue.File
	if issue.Line > 0 {
		location = fmt.Sprintf("%s:%d", issue.File, issue.Line)
	}
	desc := strings.TrimSpace(issue.Description)
	if location == "" || strings.Contains(desc, location) || strings.Contains(desc, filepath.Base(issue.File)) {
		return desc
	}
	return fmt.Sprintf("`%s` — %s", location, desc)
}

// rewriteAIResponseText replaces the canonical Output of the AIResponseReceived
// event with text (the post-grounding rewritten string). When rawText differs
// from text — i.e. grounding actually mutated the LLM output — the original
// LLM text is preserved in OutputRaw for forensics. Consumers continue to read
// Output as the canonical text; OutputRaw is forensics-only.
func rewriteAIResponseText(events []event.Envelope, text string, rawText string) []event.Envelope {
	for i := range events {
		if events[i].Type != event.AIResponseReceived {
			continue
		}
		var payload event.AIResponsePayload
		if err := unmarshalPayload(events[i].Payload, &payload); err != nil {
			return events
		}
		output, _ := json.Marshal(text)
		payload.Structured = false
		payload.Output = output
		if rawText != "" && rawText != text {
			rawOutput, _ := json.Marshal(rawText)
			payload.OutputRaw = rawOutput
		}
		events[i].Payload = event.MustMarshal(payload)
		return events
	}
	return events
}

// unmarshalPayload unmarshals JSON payload data.
func unmarshalPayload(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
