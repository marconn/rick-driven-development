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

// PRConsolidatorHandler collects outputs from the dedicated category
// reviewers (see prCategoryReviewerLabels below for the authoritative list),
// calls AI to produce a structured review payload, and posts it to
// the PR as a single GitHub *pull request review* — with each actionable
// finding attached as an inline comment anchored to the diff. This is the only
// handler in the flow with an external side-effect (posting the review).
type PRConsolidatorHandler struct {
	backend  backend.Backend
	model    string
	store    eventstore.Store
	registry *persona.Registry
	builder  *persona.PromptBuilder
	workDir  string
	yolo     bool

	// Injectable gh side-effects. Tests override to exercise the 422 fallback
	// tiers without shelling out. Keep this scoped to the handler — the
	// broader `internal/handler/pr_*.go` surface still uses inline
	// exec.CommandContext and should not be partially abstracted.
	//
	// fetchRawDiff takes the workspace path + base branch first so the default
	// impl can read the authoritative diff from the local clone (no vendor
	// size cap). It falls back to `gh pr diff` only when the workspace is
	// unreachable.
	postReview      func(ctx context.Context, fullRepo, prNumber string, payload reviewPayload) error
	postComment     func(ctx context.Context, fullRepo, prNumber, body string) error
	fetchHeadSHA    func(ctx context.Context, fullRepo, prNumber string) (string, error)
	fetchRawDiff    func(ctx context.Context, workspacePath, base, fullRepo, prNumber string) string
	viewerDidAuthor func(ctx context.Context, fullRepo, prNumber string) (bool, error)
}

// ConsolidatorModel is the Claude model this handler pins to. The consolidator
// does pure synthesis — dedupe findings from the category reviewers, validate
// inline anchors against the diff — and does not need a frontier-class model.
// Haiku is ~5× cheaper and faster; pin here (not via RICK_MODEL) so it stays
// decoupled from the developer/reviewer defaults.
const ConsolidatorModel = "claude-haiku-4-5-20251001"

// NewPRConsolidator creates a PRConsolidatorHandler from the shared Deps.
// The handler always runs on a dedicated Claude driver + Haiku model, ignoring
// the review-phase rotation. Rationale: the task is summarisation plus JSON
// conformance, and round-robin rotation made per-run attribution fuzzy and
// amplified prompt-drift across three different models.
func NewPRConsolidator(d Deps) *PRConsolidatorHandler {
	b := d.Backend
	if claude, err := backend.New("claude"); err == nil {
		b = claude
	}
	return &PRConsolidatorHandler{
		backend:         b,
		model:           ConsolidatorModel,
		store:           d.Store,
		registry:        d.Personas,
		builder:         d.Builder,
		workDir:         d.WorkDir,
		yolo:            d.Yolo,
		postReview:      postPRReview,
		postComment:     postPRComment,
		fetchHeadSHA:    fetchPRHeadSHA,
		fetchRawDiff:    fetchPRRawDiff,
		viewerDidAuthor: queryViewerDidAuthor,
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

	params, phaseOutputs, workspacePath, workspaceBase, diffTruncated := extractConsolidatorInputs(events)

	fullRepo, prNumber, err := parsePRSource(params.Source)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: parse source %q: %w", params.Source, err)
	}

	// Authoritative diff from the workspace clone. The default impl falls
	// back to `gh pr diff` only if the clone is unavailable.
	diff := h.fetchRawDiff(ctx, workspacePath, workspaceBase, fullRepo, prNumber)

	aiOutput, err := h.callAI(ctx, env, params, phaseOutputs, workspacePath, diff)
	if err != nil {
		return nil, fmt.Errorf("pr-consolidator: AI call: %w", err)
	}

	// If the reviewer-facing diff was truncated (giant PR), every reviewer
	// only saw the first ~512KB of hunks. A unanimous APPROVE on a partial
	// diff is not a confident approval — downgrade to COMMENT with an
	// explicit caveat so humans know the review is incomplete.
	if diffTruncated {
		aiOutput = downgradeApproveOnTruncatedDiff(aiOutput)
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

// extractConsolidatorInputs scans the correlation chain and returns
// everything the consolidator needs:
//   - params         — the original WorkflowRequestedPayload
//   - handlerOutputs — map of handler name → AI output text. Keyed by
//     handler name (from event Source "handler:<name>") so multiple
//     handlers sharing the same phase template don't collide.
//   - workspacePath  — the clone provisioned by pr-workspace. Required so
//     the backend runs inside a git repo (codex refuses "not inside a
//     trusted directory" otherwise) and so the raw-diff fetcher can read
//     from git locally.
//   - workspaceBase  — base branch name, needed to derive `origin/<base>`
//     for `git diff` when fetching the authoritative diff.
//   - diffTruncated  — true when pr-workspace emitted the reviewer-facing
//     diff enrichment with kind="pr-diff-truncated". Signals that every
//     reviewer operated on a partial diff; a unanimous pass must not be
//     promoted to APPROVE.
func extractConsolidatorInputs(events []event.Envelope) (event.WorkflowRequestedPayload, map[string]string, string, string, bool) {
	var params event.WorkflowRequestedPayload
	handlerOutputs := make(map[string]string)
	var workspacePath, workspaceBase string
	var diffTruncated bool

	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			_ = json.Unmarshal(e.Payload, &params)

		case event.WorkspaceReady:
			var p event.WorkspaceReadyPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Path != "" {
				workspacePath = p.Path
				workspaceBase = p.Base
			}

		case event.ContextEnrichment:
			var p event.ContextEnrichmentPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Kind == "pr-diff-truncated" {
				diffTruncated = true
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

	return params, handlerOutputs, workspacePath, workspaceBase, diffTruncated
}

// downgradeApproveOnTruncatedDiff mutates the consolidator's AI-produced
// JSON so that an APPROVE event becomes a COMMENT with an explicit caveat
// prepended to the summary. Other events (COMMENT, REQUEST_CHANGES) and
// already-present caveats are left untouched. Falls back to the input
// unchanged if the output is not parseable JSON — the downstream post path
// already handles non-JSON output with a text fallback.
func downgradeApproveOnTruncatedDiff(aiOutput string) string {
	var payload struct {
		Summary    string            `json:"summary"`
		Event      string            `json:"event"`
		Comments   []json.RawMessage `json:"comments"`
		Unanchored []string          `json:"unanchored"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(aiOutput)), &payload); err != nil {
		return aiOutput
	}
	if payload.Event != "APPROVE" {
		return aiOutput
	}
	payload.Event = "COMMENT"
	caveat := "> ⚠️ **Partial review.** The PR diff exceeded the reviewer context budget (>512 KB). Each category reviewer saw only the first ~512 KB of hunks; findings outside that window will not appear here. Treat this as a sanity check, not an approval. Manual review recommended for completeness.\n\n"
	payload.Summary = caveat + payload.Summary
	// json.Marshal on nil slices emits `null`; keep the consolidator's
	// zero-finding convention of empty arrays for downstream consumers.
	if payload.Comments == nil {
		payload.Comments = []json.RawMessage{}
	}
	if payload.Unanchored == nil {
		payload.Unanchored = []string{}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return aiOutput
	}
	return string(out)
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
		Model:        h.model,
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
	{"pr-vendor-resilience", "Vendor Resilience Review"},
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

// inlineOnlyReviewBody is the canned top-level review body we emit when every
// finding is attached as an inline comment (no unanchored leftovers). Keeps
// the PR tab free of duplicated content.
const inlineOnlyReviewBody = "See inline comments."

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
// the diff, and posts a single GitHub pull-request review. Handles three
// failure tiers:
//
//  1. Preflight: GitHub rejects APPROVE *and* REQUEST_CHANGES when the viewer
//     authored the PR (verified via GraphQL viewerDidAuthor). Downgrade to
//     COMMENT before posting to avoid a guaranteed 422.
//  2. Reactive self-author: if the probe was unavailable (e.g. installation
//     token without a viewer) and GitHub still returns 422 with a self-author
//     rejection body, retry once with event: COMMENT.
//  3. Reactive bad-anchor: 422 on any other cause — most often an inline
//     comment pointing outside the diff — strip inline comments and retry.
//
// When the AI output isn't parseable JSON we fall back to a plain issue-level
// comment so we're never worse than the previous "one big comment" behaviour.
// Returns a short human summary describing what was posted.
func (h *PRConsolidatorHandler) postConsolidatedReview(
	ctx context.Context,
	fullRepo, prNumber, diff, aiOutput string,
) (string, error) {
	review, err := parseConsolidatorJSON(aiOutput)
	if err != nil {
		if fbErr := h.postComment(ctx, fullRepo, prNumber, aiOutput); fbErr != nil {
			return "", fmt.Errorf("JSON parse failed (%v) and fallback pr comment failed: %w", err, fbErr)
		}
		return fmt.Sprintf("Posted fallback issue comment to %s#%s (AI JSON parse failed: %v)", fullRepo, prNumber, err), nil
	}

	anchors := parseDiffAnchors(diff)
	validComments, unanchored := splitComments(review, anchors)

	// When every finding is anchored inline (no unanchored leftovers), the
	// review body is pure framing — don't let the AI restate the same
	// content that's already attached as inline comments. Invariant
	// enforced here because prompt adherence drifts across models and the
	// prompt-level "do not duplicate" rule is routinely ignored.
	var body string
	if len(validComments) > 0 && len(unanchored) == 0 {
		body = inlineOnlyReviewBody
	} else {
		body = renderReviewBody(review.Summary, unanchored)
	}
	eventType := normalizeReviewEvent(review.Event)
	originalEvent := eventType

	// Tier 1 (preflight): authors can't APPROVE or REQUEST_CHANGES on their
	// own PR. Probe with GraphQL — single round-trip, server-side identity,
	// no login-string drift across bot-suffix / SSO / case. Probe failures
	// (network flake, installation tokens without a viewer) fall through to
	// the reactive tier below.
	if eventType != "COMMENT" {
		if authored, probeErr := h.viewerDidAuthor(ctx, fullRepo, prNumber); probeErr == nil && authored {
			eventType = "COMMENT"
		}
	}

	headSHA, shaErr := h.fetchHeadSHA(ctx, fullRepo, prNumber)
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

	err = h.postReview(ctx, fullRepo, prNumber, payload)
	if err == nil {
		if eventType != originalEvent {
			return fmt.Sprintf("Posted review to %s#%s as %s (downgraded from %s: viewer authored PR) (%d inline, %d unanchored)",
				fullRepo, prNumber, eventType, originalEvent, len(payload.Comments), len(unanchored)), nil
		}
		return fmt.Sprintf("Posted review to %s#%s as %s (%d inline, %d unanchored)",
			fullRepo, prNumber, eventType, len(payload.Comments), len(unanchored)), nil
	}

	// Tier 2 (reactive): GitHub rejected the review as self-authored despite
	// the preflight. Happens when the probe failed open or GitHub's identity
	// resolution differs from GraphQL (e.g. installation tokens). Retry with
	// COMMENT — authors can always comment on their own PRs.
	if isSelfAuthorRejection(err) && payload.Event != "COMMENT" {
		retry := payload
		retry.Event = "COMMENT"
		if retryErr := h.postReview(ctx, fullRepo, prNumber, retry); retryErr == nil {
			return fmt.Sprintf("Posted review to %s#%s as COMMENT (downgraded from %s after self-author rejection) (%d inline, %d unanchored)",
				fullRepo, prNumber, payload.Event, len(retry.Comments), len(unanchored)), nil
		} else {
			err = retryErr
			payload = retry
		}
	}

	// Tier 3 (reactive): assume the 422 is a bad inline anchor GitHub won't
	// accept. Strip inline comments and fold them into the body so we still
	// deliver the signal.
	if len(payload.Comments) > 0 {
		fallback := payload
		fallback.Comments = nil
		fallback.Body = body + "\n\n> Note: inline comments could not be attached; collapsed into this summary."
		if fbErr := h.postReview(ctx, fullRepo, prNumber, fallback); fbErr != nil {
			return "", fmt.Errorf("post review: %w (fallback also failed: %v)", err, fbErr)
		}
		return fmt.Sprintf("Posted review to %s#%s as %s (inline comments dropped after 422)", fullRepo, prNumber, payload.Event), nil
	}

	return "", fmt.Errorf("post review: %w", err)
}

// isSelfAuthorRejection returns true when the error looks like GitHub's
// "author can't APPROVE / REQUEST_CHANGES on own PR" 422. The exact phrasing
// ("Can not approve your own pull request") lives in errors[].message and
// isn't a documented contract, so match loosely on the stable tokens
// ("approve your own" / "request changes on your own") and case-fold.
func isSelfAuthorRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "approve your own") ||
		strings.Contains(msg, "request changes on your own")
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

// fetchPRRawDiff returns the full (untruncated) unified diff for the PR,
// used both to feed the AI and to validate inline comment anchors. Prefers
// the workspace clone over `gh pr diff` because the GitHub REST endpoint
// returns HTTP 406 for PRs with >300 files — which silently produced an
// empty diff and a unanimous-pass false APPROVE on hulilabs/huli#802
// (2026-04-24). Falls back to the REST call only when the workspace is
// unreachable or the base branch is unknown.
func fetchPRRawDiff(ctx context.Context, workspacePath, base, fullRepo, prNumber string) string {
	if diff := fetchWorkspaceRawDiff(ctx, workspacePath, base); diff != "" {
		return diff
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "diff", prNumber,
		"--repo", fullRepo)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// queryViewerDidAuthor reports whether the authenticated viewer is the author
// of the PR. Uses GraphQL's viewerDidAuthor field (server-side identity
// match, one round-trip) rather than comparing `gh api user` to
// `gh pr view --json author` — that login-compare breaks on bot suffixes
// (`foo[bot]` vs `app/foo`), SSO normalisation, and installation tokens
// (which return 403 on /user). viewerDidAuthor handles all three cleanly:
// installation tokens resolve with a null viewer and return false, which is
// the safe fallthrough since bots can approve PRs opened by other bots.
func queryViewerDidAuthor(ctx context.Context, fullRepo, prNumber string) (bool, error) {
	owner, repo, ok := strings.Cut(fullRepo, "/")
	if !ok {
		return false, fmt.Errorf("invalid repo %q (want owner/name)", fullRepo)
	}
	prNum, err := strconv.Atoi(prNumber)
	if err != nil {
		return false, fmt.Errorf("invalid pr number %q: %w", prNumber, err)
	}

	query := `query($owner:String!,$repo:String!,$num:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$num){viewerDidAuthor}}}`
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql",
		"-f", "query="+query,
		"-F", "owner="+owner,
		"-F", "repo="+repo,
		"-F", fmt.Sprintf("num=%d", prNum),
		"--jq", ".data.repository.pullRequest.viewerDidAuthor")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gh api graphql viewerDidAuthor: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected viewerDidAuthor response: %q", strings.TrimSpace(string(out)))
	}
}
