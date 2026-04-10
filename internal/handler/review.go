package handler

import (
	"context"
	"encoding/json"
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

func (h *ReviewHandler) Name() string            { return h.ai.Name() }
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
var fileRefRe = regexp.MustCompile(`(\w[\w./]*\.go)(?::(\d+))?`)

// extractFileRef tries to find a file:line reference in the description.
func extractFileRef(desc string) (string, int) {
	match := fileRefRe.FindStringSubmatch(desc)
	if match == nil {
		return "", 0
	}
	file := match[1]
	line := 0
	if match[2] != "" {
		for _, c := range match[2] {
			line = line*10 + int(c-'0')
		}
	}
	return file, line
}

// unmarshalPayload unmarshals JSON payload data.
func unmarshalPayload(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
