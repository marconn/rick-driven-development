package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// PRConsolidatorHandler collects outputs from the 11 dedicated category
// reviewers, calls AI to produce a structured review payload, and posts it to
// the PR as a single GitHub *pull request review* — with each actionable
// finding attached as an inline comment anchored to the diff. This is the only
// handler in the flow with an external side-effect (posting the review).
type PRConsolidatorHandler struct {
	backend  backend.Backend
	store    eventstore.Store
	registry *persona.Registry
	builder  *persona.PromptBuilder
	workDir  string
	yolo     bool
}

// NewPRConsolidator creates a PRConsolidatorHandler from the shared Deps.
func NewPRConsolidator(d Deps) *PRConsolidatorHandler {
	return &PRConsolidatorHandler{
		backend:  d.Backend,
		store:    d.Store,
		registry: d.Personas,
		builder:  d.Builder,
		workDir:  d.WorkDir,
		yolo:     d.Yolo,
	}
}

// Name returns the unique handler identifier.
func (h *PRConsolidatorHandler) Name() string { return "pr-consolidator" }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *PRConsolidatorHandler) Subscribes() []event.Type { return nil }

// Handle loads all AI outputs from the correlation chain, builds a consolidation
// prompt, calls AI, posts the result as a GitHub PR review (inline comments
// where anchored, summary + unanchored list in the review body), and emits
// ContextEnrichment.
func (h *PRConsolidatorHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: load correlation chain: %w", err)
	}

	params, phaseOutputs, workspacePath := extractConsolidatorInputs(events)

	fullRepo, prNumber, err := parsePRSource(params.Source)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: parse source %q: %w", params.Source, err)
	}

	// Fetch the authoritative diff directly (not the truncated enrichment
	// summary) so the AI has the full context and we can validate anchors.
	diff := fetchPRRawDiff(ctx, fullRepo, prNumber)

	aiOutput, err := h.callAI(ctx, env, params, phaseOutputs, workspacePath, diff)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: AI call: %w", err)
	}

	summary, postErr := h.postConsolidatedReview(ctx, fullRepo, prNumber, diff, aiOutput)
	if postErr != nil {
		return nil, fmt.Errorf("pr-consolidator: post review: %w", postErr)
	}

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "pr-consolidator",
		Kind:    "pr-review",
		Summary: summary,
	})).WithSource("handler:pr-consolidator")

	return []event.Envelope{enrichEvt}, nil
}

// extractConsolidatorInputs scans the correlation chain and returns the
// WorkflowRequestedPayload, a map of handler name → AI output text, and the
// workspace path provisioned by pr-workspace. Keyed by handler name (from
// event Source "handler:<name>") so that multiple handlers sharing the same
// phase template don't collide. The workspace path is required so the backend
// runs inside a git repo — codex refuses with "not inside a trusted directory"
// otherwise.
func extractConsolidatorInputs(events []event.Envelope) (event.WorkflowRequestedPayload, map[string]string, string) {
	var params event.WorkflowRequestedPayload
	handlerOutputs := make(map[string]string)
	var workspacePath string

	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			_ = json.Unmarshal(e.Payload, &params)

		case event.WorkspaceReady:
			var p event.WorkspaceReadyPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Path != "" {
				workspacePath = p.Path
			}

		case event.AIResponseReceived:
			var p event.AIResponsePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				key := strings.TrimPrefix(e.Source, "handler:")
				if key == "" {
					key = p.Phase // fallback for events without Source
				}
				handlerOutputs[key] = unmarshalOutput(p.Output, p.Structured)
			}
		}
	}

	return params, handlerOutputs, workspacePath
}

// callAI builds the consolidation prompt and invokes the AI backend.
func (h *PRConsolidatorHandler) callAI(
	ctx context.Context,
	env event.Envelope,
	params event.WorkflowRequestedPayload,
	phaseOutputs map[string]string,
	workspacePath string,
	diff string,
) (string, error) {
	systemPrompt, err := h.registry.LoadSystemPrompt(persona.PRConsolidator)
	if err != nil {
		return "", fmt.Errorf("load system prompt: %w", err)
	}

	userPrompt := buildConsolidationPrompt(params, phaseOutputs, diff)

	// Prefer the PR workspace the pr-workspace handler provisioned so the
	// backend runs inside a git repo (codex refuses "not inside a trusted
	// directory" otherwise). Fall back to the static workDir when the
	// workspace payload is missing — a legitimate path in non-PR flows that
	// might reuse this handler.
	workDir := h.workDir
	if workspacePath != "" {
		workDir = workspacePath
	}

	// Yolo is always false here — the consolidator only synthesises text
	// from the reviewer outputs. Tool access caused a double-post bug where
	// Claude proactively ran `gh pr comment` AND the handler posted the
	// output again.
	backendCtx := ctx
	if env.CorrelationID != "" {
		backendCtx = backend.WithStickyKey(backendCtx, env.CorrelationID+":pr-consolidator")
	}
	resp, err := h.backend.Run(backendCtx, backend.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		WorkDir:      workDir,
		Yolo:         false,
	})
	if err != nil {
		return "", fmt.Errorf("backend: %w", err)
	}

	return resp.Output, nil
}

// prCategoryReviewerLabels maps handler name → human label for the consolidation prompt.
var prCategoryReviewerLabels = []struct {
	key   string
	label string
}{
	{"pr-security", "Security Review"},
	{"pr-concurrency", "Concurrency Review"},
	{"pr-error-handling", "Error Handling Review"},
	{"pr-observability", "Observability Review"},
	{"pr-api-contract", "API Contract Review"},
	{"pr-idempotency", "Idempotency Review"},
	{"pr-testing", "Testing Review"},
	{"pr-integration", "Integration Review"},
	{"pr-performance", "Performance Review"},
	{"pr-data", "Data Integrity Review"},
	{"pr-hygiene", "Code Hygiene Review"},
}

// buildConsolidationPrompt assembles the user prompt for the consolidator AI call.
// It provides the category-reviewer outputs plus the authoritative PR diff so the
// AI can anchor findings to exact file/line positions.
func buildConsolidationPrompt(params event.WorkflowRequestedPayload, handlerOutputs map[string]string, diff string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## PR Review Consolidation Task\n\n")
	fmt.Fprintf(&b, "Source: %s\n\n", params.Source)

	if params.Prompt != "" {
		fmt.Fprintf(&b, "**PR Description / Task Context**:\n%s\n\n", params.Prompt)
	}

	for _, r := range prCategoryReviewerLabels {
		output := handlerOutputs[r.key]
		if output == "" {
			output = "(no output)"
		}
		fmt.Fprintf(&b, "---\n### %s\n\n%s\n\n", r.label, output)
	}

	b.WriteString("---\n## PR Diff\n\n")
	if strings.TrimSpace(diff) == "" {
		b.WriteString("(diff unavailable — prefer `unanchored` entries over inventing file:line)\n\n")
	} else {
		b.WriteString("Use this diff to anchor inline comments. Only emit a `comment` when its `path` + `line` are present in a hunk below. Any finding that cannot be grounded to an exact line goes in `unanchored`.\n\n")
		b.WriteString("```diff\n")
		b.WriteString(diff)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("---\n\nProduce the consolidated review as a single JSON object conforming to the schema in your system prompt. Output JSON only — no prose, no code fences.")

	return b.String()
}

// ---------------------------------------------------------------------------
// AI-output parsing
// ---------------------------------------------------------------------------

// consolidatorReview mirrors the JSON schema the consolidator AI must emit.
type consolidatorReview struct {
	Summary    string                `json:"summary"`
	Event      string                `json:"event"`
	Comments   []consolidatorComment `json:"comments"`
	Unanchored []string              `json:"unanchored"`
}

type consolidatorComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// parseConsolidatorJSON extracts a consolidatorReview from the raw AI output.
// Tolerates a surrounding ```json ... ``` fence and ignores prose before/after
// the outermost JSON object.
func parseConsolidatorJSON(raw string) (consolidatorReview, error) {
	jsonText := extractJSONObject(raw)
	if jsonText == "" {
		return consolidatorReview{}, fmt.Errorf("no JSON object found in AI output")
	}

	var r consolidatorReview
	if err := json.Unmarshal([]byte(jsonText), &r); err != nil {
		return consolidatorReview{}, fmt.Errorf("unmarshal consolidator JSON: %w", err)
	}
	return r, nil
}

// extractJSONObject returns the first balanced `{...}` substring from s,
// ignoring braces inside double-quoted strings. Returns "" when none found.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Diff anchor validation
// ---------------------------------------------------------------------------

// diffAnchors holds the set of (path, line) positions valid for inline comments
// on each side of the diff.
type diffAnchors struct {
	right map[string]map[int]bool
	left  map[string]map[int]bool
}

func newDiffAnchors() *diffAnchors {
	return &diffAnchors{
		right: map[string]map[int]bool{},
		left:  map[string]map[int]bool{},
	}
}

func (d *diffAnchors) allows(path string, line int, side string) bool {
	if path == "" || line <= 0 {
		return false
	}
	var m map[string]map[int]bool
	if strings.EqualFold(side, "LEFT") {
		m = d.left
	} else {
		m = d.right
	}
	lines, ok := m[path]
	if !ok {
		return false
	}
	return lines[line]
}

var consolidatorHunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseDiffAnchors walks a unified diff and collects the line numbers that are
// valid inline-comment anchors on each side.
//   - RIGHT: added (`+`) or context (` `) lines, indexed by the new-side line number.
//   - LEFT:  removed (`-`) or context (` `) lines, indexed by the old-side line number.
func parseDiffAnchors(diff string) *diffAnchors {
	anchors := newDiffAnchors()
	if diff == "" {
		return anchors
	}

	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var curPath string
	var oldLine, newLine int
	inHunk := false

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			curPath = stripDiffPathPrefix(strings.TrimPrefix(line, "+++ "))
			if curPath != "" {
				if anchors.right[curPath] == nil {
					anchors.right[curPath] = map[int]bool{}
				}
				if anchors.left[curPath] == nil {
					anchors.left[curPath] = map[int]bool{}
				}
			}
			inHunk = false
		case strings.HasPrefix(line, "--- "):
			// Handled via +++; ignore.
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			m := consolidatorHunkHeaderRe.FindStringSubmatch(line)
			if len(m) < 3 {
				inHunk = false
				continue
			}
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			inHunk = curPath != ""
		case inHunk && strings.HasPrefix(line, "+"):
			anchors.right[curPath][newLine] = true
			newLine++
		case inHunk && strings.HasPrefix(line, "-"):
			anchors.left[curPath][oldLine] = true
			oldLine++
		case inHunk && strings.HasPrefix(line, " "):
			anchors.right[curPath][newLine] = true
			anchors.left[curPath][oldLine] = true
			newLine++
			oldLine++
		}
	}
	return anchors
}

// stripDiffPathPrefix removes the standard `a/` or `b/` prefix git places on
// diff file headers, and drops trailing whitespace/timestamps. Returns "" for
// /dev/null (file was created or deleted on the other side).
func stripDiffPathPrefix(s string) string {
	// The +++ / --- lines may include a trailing tab + timestamp — strip it.
	if tab := strings.IndexByte(s, '\t'); tab >= 0 {
		s = s[:tab]
	}
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}

// splitComments partitions AI-proposed comments into those that anchor cleanly
// onto the diff and those that do not. Unanchored items (and invalid ones) are
// returned as text bullets to fold into the review body.
func splitComments(review consolidatorReview, anchors *diffAnchors) (valid []consolidatorComment, unanchored []string) {
	unanchored = append(unanchored, review.Unanchored...)

	for _, c := range review.Comments {
		if strings.TrimSpace(c.Body) == "" {
			continue
		}
		side := strings.ToUpper(strings.TrimSpace(c.Side))
		if side != "LEFT" && side != "RIGHT" {
			side = "RIGHT"
		}
		if !anchors.allows(c.Path, c.Line, side) {
			// Preserve the context so the author still sees where it was aimed.
			label := c.Path
			if label == "" {
				label = "(unknown path)"
			}
			unanchored = append(unanchored,
				fmt.Sprintf("**%s:%d** — %s", label, c.Line, strings.TrimSpace(c.Body)))
			continue
		}
		c.Side = side
		valid = append(valid, c)
	}
	return valid, unanchored
}

// renderReviewBody composes the top-level body of the GitHub review from the
// AI summary and any findings that couldn't be anchored inline.
func renderReviewBody(summary string, unanchored []string) string {
	var b strings.Builder
	trimmed := strings.TrimSpace(summary)
	if trimmed == "" {
		trimmed = "Review completed."
	}
	b.WriteString(trimmed)
	if len(unanchored) > 0 {
		b.WriteString("\n\n### Additional findings\n\n")
		for _, item := range unanchored {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(item))
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Posting
// ---------------------------------------------------------------------------

// normalizeReviewEvent coerces the AI-provided event string into a value
// GitHub's reviews API accepts. Defaults to COMMENT on any unrecognised value.
func normalizeReviewEvent(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "APPROVE":
		return "APPROVE"
	case "REQUEST_CHANGES", "REQUESTCHANGES", "REQUEST CHANGES":
		return "REQUEST_CHANGES"
	default:
		return "COMMENT"
	}
}

// reviewPayload is the request body for POST /repos/{owner}/{repo}/pulls/{n}/reviews.
type reviewPayload struct {
	CommitID string                `json:"commit_id,omitempty"`
	Event    string                `json:"event"`
	Body     string                `json:"body"`
	Comments []reviewCommentInput  `json:"comments,omitempty"`
}

type reviewCommentInput struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// postConsolidatedReview parses the AI JSON, validates inline comments against
// the diff, and posts a single GitHub pull-request review. Falls back to a
// plain issue-level comment when the AI output cannot be parsed as JSON — so
// we are never *worse* than the previous "one big comment" behaviour.
// Returns a short human summary describing what was posted.
func (h *PRConsolidatorHandler) postConsolidatedReview(
	ctx context.Context,
	fullRepo, prNumber, diff, aiOutput string,
) (string, error) {
	review, err := parseConsolidatorJSON(aiOutput)
	if err != nil {
		if fbErr := postPRComment(ctx, fullRepo, prNumber, aiOutput); fbErr != nil {
			return "", fmt.Errorf("JSON parse failed (%v) and fallback pr comment failed: %w", err, fbErr)
		}
		return fmt.Sprintf("Posted fallback issue comment to %s#%s (AI JSON parse failed: %v)", fullRepo, prNumber, err), nil
	}

	anchors := parseDiffAnchors(diff)
	validComments, unanchored := splitComments(review, anchors)

	body := renderReviewBody(review.Summary, unanchored)
	eventType := normalizeReviewEvent(review.Event)

	headSHA, shaErr := fetchPRHeadSHA(ctx, fullRepo, prNumber)
	if shaErr != nil {
		// Without a commit_id GitHub defaults to the latest commit, which is
		// acceptable but less precise. Continue with an empty SHA.
		headSHA = ""
	}

	payload := reviewPayload{
		CommitID: headSHA,
		Event:    eventType,
		Body:     body,
	}
	for _, c := range validComments {
		payload.Comments = append(payload.Comments, reviewCommentInput(c))
	}

	if err := postPRReview(ctx, fullRepo, prNumber, payload); err != nil {
		// Retry once without inline comments. GitHub rejects the whole review
		// on any single invalid anchor — falling back to a plain review body
		// preserves signal even when our diff parsing missed an edge case.
		fallback := payload
		fallback.Comments = nil
		if len(validComments) > 0 {
			fallback.Body = body + "\n\n> Note: inline comments could not be attached; collapsed into this summary."
		}
		if fbErr := postPRReview(ctx, fullRepo, prNumber, fallback); fbErr != nil {
			return "", fmt.Errorf("post review: %w (fallback also failed: %v)", err, fbErr)
		}
		return fmt.Sprintf("Posted review to %s#%s as %s (inline comments dropped after 422)", fullRepo, prNumber, eventType), nil
	}

	return fmt.Sprintf("Posted review to %s#%s as %s (%d inline, %d unanchored)",
		fullRepo, prNumber, eventType, len(payload.Comments), len(unanchored)), nil
}

// postPRReview POSTs a structured review to the GitHub API via `gh api`.
func postPRReview(ctx context.Context, fullRepo, prNumber string, payload reviewPayload) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal review payload: %w", err)
	}
	cmd := exec.CommandContext(ctx, "gh", "api",
		"--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%s/reviews", fullRepo, prNumber),
		"--input", "-")
	cmd.Stdin = bytes.NewReader(buf)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api reviews: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// postPRComment posts a plain issue-level comment to the PR — used only as a
// last-resort fallback when the AI output is not parseable JSON.
func postPRComment(ctx context.Context, fullRepo, prNumber, body string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", prNumber,
		"--repo", fullRepo,
		"--body", body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// fetchPRHeadSHA returns the head commit SHA for a PR so review comments
// anchor to the exact revision the reviewers analysed.
func fetchPRHeadSHA(ctx context.Context, fullRepo, prNumber string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber,
		"--repo", fullRepo,
		"--json", "headRefOid")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view headRefOid: %w", err)
	}
	var r struct {
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("unmarshal headRefOid: %w", err)
	}
	return strings.TrimSpace(r.HeadRefOid), nil
}

// fetchPRRawDiff returns the full (untruncated) unified diff for the PR, used
// both to feed the AI and to validate inline comment anchors.
func fetchPRRawDiff(ctx context.Context, fullRepo, prNumber string) string {
	cmd := exec.CommandContext(ctx, "gh", "pr", "diff", prNumber,
		"--repo", fullRepo)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}
