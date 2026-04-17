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
	ai          *AIHandler
	targetPhase string // phase to send feedback to on failure (e.g., "develop")
}

// ReviewHandlerConfig configures a review handler.
type ReviewHandlerConfig struct {
	AIConfig    AIHandlerConfig
	TargetPhase string // phase that should be rescheduled on fail (e.g., "develop")
}

// NewReviewHandler creates a handler that parses verdicts from AI responses.
func NewReviewHandler(cfg ReviewHandlerConfig) *ReviewHandler {
	return &ReviewHandler{
		ai:          NewAIHandler(cfg.AIConfig),
		targetPhase: cfg.TargetPhase,
	}
}

func (h *ReviewHandler) Name() string             { return h.ai.Name() }
func (h *ReviewHandler) Phase() string            { return h.ai.Phase() }
func (h *ReviewHandler) Subscribes() []event.Type { return nil }

// Handle calls the AI backend, parses the verdict, and returns AI events
// plus a VerdictRendered event.
func (h *ReviewHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	aiEvents, err := h.ai.Handle(ctx, env)
	if err != nil {
		return nil, err
	}

	// Extract AI response text from the AIResponseReceived event
	responseText := h.extractResponseText(aiEvents)

	verdict := ParseVerdict(responseText)
	issues := ParseIssues(responseText, verdict.Outcome)
	if h.ai.phase == "pr-category-review" {
		responseText, verdict, issues = h.groundPRCategoryReview(ctx, env, responseText, verdict, issues)
		aiEvents = rewriteAIResponseText(aiEvents, responseText)
	}

	verdictEvt := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       h.targetPhase,
		SourcePhase: h.ai.phase,
		Outcome:     verdict.Outcome,
		Issues:      issues,
		Summary:     verdict.Summary,
	})).WithSource("handler:" + h.ai.name)

	return append(aiEvents, verdictEvt), nil
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

// Verdict holds the parsed result from AI review output.
type Verdict struct {
	Outcome event.VerdictOutcome
	Summary string
}

type prDiffGroundingScope struct {
	changedFiles map[string]struct{}
	changedLines map[string]map[int]string
}

// ParseVerdict extracts VERDICT: PASS or VERDICT: FAIL from AI output.
// Defaults to VerdictPass if no verdict line is found.
func ParseVerdict(text string) Verdict {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		upper := strings.ToUpper(line)

		if strings.Contains(upper, "VERDICT:") {
			if strings.Contains(upper, "FAIL") {
				summary := extractSummary(lines, i)
				return Verdict{Outcome: event.VerdictFail, Summary: summary}
			}
			if strings.Contains(upper, "PASS") {
				return Verdict{Outcome: event.VerdictPass, Summary: "passed review"}
			}
		}
	}
	// No explicit verdict — default to pass (optimistic)
	return Verdict{Outcome: event.VerdictPass, Summary: "no explicit verdict found; defaulting to pass"}
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
) (string, Verdict, []event.Issue) {
	scope, ok := h.loadPRDiffGroundingScope(ctx, env.CorrelationID)
	if !ok {
		return responseText, verdict, issues
	}

	grounded := make([]event.Issue, 0, len(issues))
	for _, issue := range issues {
		if gi, ok := scope.groundIssue(issue); ok {
			grounded = append(grounded, gi)
		}
	}

	if verdict.Outcome == event.VerdictFail && len(grounded) == 0 {
		verdict = Verdict{
			Outcome: event.VerdictPass,
			Summary: "no grounded issues found in the changed lines for this review category",
		}
	}
	if verdict.Outcome != event.VerdictFail {
		grounded = nil
	}

	return buildCompactPRCategoryReviewOutput(verdict, grounded), verdict, grounded
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

func (s prDiffGroundingScope) groundIssue(issue event.Issue) (event.Issue, bool) {
	if issue.File == "" {
		issue.File, issue.Line = extractFileRef(issue.Description)
	}
	file := s.resolveFile(issue.File)
	if file == "" || issue.Line <= 0 {
		return event.Issue{}, false
	}
	if !s.matchesChangedLine(file, issue.Line, issue.Description) {
		return event.Issue{}, false
	}
	issue.File = file
	return issue, true
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

func groundingTokens(description string) []string {
	matches := codeSpanRefRe.FindAllStringSubmatch(description, -1)
	tokens := make([]string, 0, len(matches))
	for _, match := range matches {
		token := strings.TrimSpace(match[1])
		if token == "" || token == "Makefile" || strings.Contains(token, "/") {
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

func rewriteAIResponseText(events []event.Envelope, text string) []event.Envelope {
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
		events[i].Payload = event.MustMarshal(payload)
		return events
	}
	return events
}

// unmarshalPayload unmarshals JSON payload data.
func unmarshalPayload(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
