package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	gh "github.com/marconn/rick-event-driven-development/internal/github"
)

// PRCommentPosterHandler posts PR comments on behalf of a text-only upstream
// persona (currently pr-replier; the design accepts any text-only composer).
// It exists to solve the same failure mode as pr-consolidator: when the LLM
// that composed the body also had tool access, it would run `gh pr comment`
// proactively AND the handler would post again, producing duplicates (see the
// 2026-04-17 pr-feedback incident on hulilabs/huli#689).
//
// Two input shapes are supported:
//  1. Structured JSON (current pr-replier contract) —
//     {"summary": "...", "inline_replies": [{"comment_id": N, "body": "..."}]}
//     The poster posts the summary (when non-empty) as a top-level issue
//     comment and posts each inline_replies entry as a reply on the specified
//     inline review-comment thread. Per-thread dedup matches `in_reply_to_id`
//     plus body hash against the live review-comment list.
//  2. Plain text (legacy / fallback) — the whole upstream body is posted as a
//     single top-level issue comment with SHA-256 dedup against recent issue
//     comments. This path fires whenever the upstream output isn't parseable
//     JSON, so a pre-contract persona or a mangled output still lands
//     *something* on the PR instead of failing the workflow.
//
// Every action the poster takes emits one PRCommentPosted event; a single
// Handle call can therefore return multiple events (one summary + N
// inline-reply + any skipped entries).
type PRCommentPosterHandler struct {
	name     string
	upstream string // handler name whose AI output becomes the body
	kind     string // PRCommentPostedPayload.Kind for the legacy plain-text path
	gh       prCommentClient
	store    eventstore.Store
}

// prCommentClient is the subset of github.Client used by the poster. Narrowing
// the surface here keeps the handler trivially mockable in tests without
// round-tripping real HTTP.
type prCommentClient interface {
	CreatePRComment(ctx context.Context, owner, repo string, prNumber int, body string) (*gh.PRComment, error)
	CreatePRReviewCommentReply(ctx context.Context, owner, repo string, prNumber, commentID int, body string) (*gh.ReviewComment, error)
	GetIssueComments(ctx context.Context, owner, repo string, number int) ([]gh.IssueComment, error)
	GetPRReviewComments(ctx context.Context, owner, repo string, prNumber int) ([]gh.ReviewComment, error)
}

// PRCommentClientAdapter wraps a *github.Client so it satisfies the narrower
// prCommentClient interface used by the poster. Keeps the handler testable
// without importing the heavier github.Client API in tests.
type PRCommentClientAdapter struct {
	Client *gh.Client
}

// CreatePRComment delegates to the wrapped client and maps the response shape.
func (a PRCommentClientAdapter) CreatePRComment(ctx context.Context, owner, repo string, prNumber int, body string) (*gh.PRComment, error) {
	return a.Client.CreatePRComment(ctx, owner, repo, prNumber, body)
}

// CreatePRReviewCommentReply delegates to the wrapped client.
func (a PRCommentClientAdapter) CreatePRReviewCommentReply(ctx context.Context, owner, repo string, prNumber, commentID int, body string) (*gh.ReviewComment, error) {
	return a.Client.CreatePRReviewCommentReply(ctx, owner, repo, prNumber, commentID, body)
}

// GetIssueComments delegates to the wrapped client.
func (a PRCommentClientAdapter) GetIssueComments(ctx context.Context, owner, repo string, number int) ([]gh.IssueComment, error) {
	return a.Client.GetIssueComments(ctx, owner, repo, number)
}

// GetPRReviewComments delegates to the wrapped client.
func (a PRCommentClientAdapter) GetPRReviewComments(ctx context.Context, owner, repo string, prNumber int) ([]gh.ReviewComment, error) {
	return a.Client.GetPRReviewComments(ctx, owner, repo, prNumber)
}

// PRCommentPosterConfig configures a poster instance.
type PRCommentPosterConfig struct {
	Name     string           // handler name, e.g. "pr-reply-poster"
	Upstream string           // predecessor handler whose AI output is posted
	Kind     string           // PRCommentPostedPayload.Kind for the plain-text fallback path, e.g. "reply"
	GitHub   prCommentClient  // may be nil — handler short-circuits with an enrichment-only path
	Store    eventstore.Store // required for loading correlation chain
}

// NewPRCommentPoster creates a poster handler.
func NewPRCommentPoster(cfg PRCommentPosterConfig) *PRCommentPosterHandler {
	return &PRCommentPosterHandler{
		name:     cfg.Name,
		upstream: cfg.Upstream,
		kind:     cfg.Kind,
		gh:       cfg.GitHub,
		store:    cfg.Store,
	}
}

// Name returns the unique handler identifier.
func (h *PRCommentPosterHandler) Name() string { return h.name }

// Subscribes returns nil — DAG dispatch handles routing.
func (h *PRCommentPosterHandler) Subscribes() []event.Type { return nil }

// replierPayload mirrors the structured JSON contract the pr-replier persona
// emits — see internal/persona/prompts/pr-replier.md.
type replierPayload struct {
	Summary       string         `json:"summary"`
	InlineReplies []inlineReply  `json:"inline_replies"`
}

type inlineReply struct {
	CommentID int    `json:"comment_id"`
	Body      string `json:"body"`
}

// Handle reads the upstream body and posts whatever the composer asked for.
func (h *PRCommentPosterHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	if env.CorrelationID == "" {
		return nil, fmt.Errorf("%s: missing correlation id", h.name)
	}

	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("%s: load correlation chain: %w", h.name, err)
	}

	source, body, err := extractPosterInputs(events, h.upstream)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", h.name, err)
	}

	fullRepo, prNumberStr, err := parsePRSource(source)
	if err != nil {
		return nil, fmt.Errorf("%s: parse source %q: %w", h.name, source, err)
	}
	prNumber, err := strconv.Atoi(prNumberStr)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid pr number %q: %w", h.name, prNumberStr, err)
	}
	owner, repo, ok := splitOwnerRepo(fullRepo)
	if !ok {
		return nil, fmt.Errorf("%s: invalid repo %q (expected owner/repo)", h.name, fullRepo)
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("%s: upstream %s produced empty body", h.name, h.upstream)
	}

	// Try the structured-JSON contract first. If the upstream output doesn't
	// parse as the replier payload — or parses but carries no summary and no
	// inline replies — fall back to the legacy plain-text top-level post so a
	// non-compliant composer still produces something visible on the PR.
	if payload, okJSON := parseReplierJSON(body); okJSON {
		return h.postStructured(ctx, fullRepo, owner, repo, prNumber, payload)
	}

	return h.postLegacyPlain(ctx, fullRepo, owner, repo, prNumber, body)
}

// postStructured handles the current pr-replier contract: optional summary
// top-level comment plus per-thread inline replies. Partial failures do NOT
// abort the whole handler — each post is attempted independently so one bad
// `comment_id` doesn't suppress the other inline replies or the summary.
// Errors are collected and returned so the caller sees the failure surface.
func (h *PRCommentPosterHandler) postStructured(ctx context.Context, fullRepo, owner, repo string, prNumber int, payload replierPayload) ([]event.Envelope, error) {
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" && len(payload.InlineReplies) == 0 {
		// Replier explicitly returned "nothing to say" — emit a single skipped
		// event so the DAG advances and the audit trail records the decision.
		return []event.Envelope{h.postedEvent("summary", fullRepo, prNumber, "", 0, 0, true)}, nil
	}

	var out []event.Envelope
	var firstErr error

	if summary != "" {
		evts, err := h.postSummary(ctx, fullRepo, owner, repo, prNumber, summary)
		out = append(out, evts...)
		if err != nil {
			firstErr = err
		}
	}

	for _, reply := range payload.InlineReplies {
		body := strings.TrimSpace(reply.Body)
		if reply.CommentID == 0 || body == "" {
			continue
		}
		evt, err := h.postInlineReply(ctx, fullRepo, owner, repo, prNumber, reply.CommentID, body)
		if evt.Type != "" {
			out = append(out, evt)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return out, firstErr
}

// postSummary posts the top-level summary as an issue comment with SHA-256
// dedup against the existing issue comments. Kind="summary".
func (h *PRCommentPosterHandler) postSummary(ctx context.Context, fullRepo, owner, repo string, prNumber int, body string) ([]event.Envelope, error) {
	bodyHash := hashBody(body)

	if h.gh == nil {
		return []event.Envelope{h.postedEvent("summary", fullRepo, prNumber, bodyHash, 0, 0, true)}, nil
	}

	if existing, err := h.gh.GetIssueComments(ctx, owner, repo, prNumber); err == nil {
		if duplicateIssueHash(existing, bodyHash) {
			return []event.Envelope{h.postedEvent("summary", fullRepo, prNumber, bodyHash, 0, 0, true)}, nil
		}
	}

	comment, err := h.gh.CreatePRComment(ctx, owner, repo, prNumber, body)
	if err != nil {
		return nil, fmt.Errorf("%s: create PR summary comment: %w", h.name, err)
	}
	return []event.Envelope{h.postedEvent("summary", fullRepo, prNumber, bodyHash, comment.ID, 0, false)}, nil
}

// postInlineReply posts a reply on an existing inline review-comment thread
// with per-thread dedup (match InReplyToID==root + body hash). Errors on a
// single reply are returned alongside a skipped-or-nil event so the caller
// can continue with the remaining replies.
func (h *PRCommentPosterHandler) postInlineReply(ctx context.Context, fullRepo, owner, repo string, prNumber, rootCommentID int, body string) (event.Envelope, error) {
	bodyHash := hashBody(body)

	if h.gh == nil {
		return h.postedEvent("inline-reply", fullRepo, prNumber, bodyHash, 0, rootCommentID, true), nil
	}

	if existing, err := h.gh.GetPRReviewComments(ctx, owner, repo, prNumber); err == nil {
		if duplicateReviewReplyHash(existing, rootCommentID, bodyHash) {
			return h.postedEvent("inline-reply", fullRepo, prNumber, bodyHash, 0, rootCommentID, true), nil
		}
	}

	reply, err := h.gh.CreatePRReviewCommentReply(ctx, owner, repo, prNumber, rootCommentID, body)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("%s: reply on comment %d: %w", h.name, rootCommentID, err)
	}
	return h.postedEvent("inline-reply", fullRepo, prNumber, bodyHash, reply.ID, rootCommentID, false), nil
}

// postLegacyPlain posts the entire upstream body as a single top-level issue
// comment — the pre-JSON contract behavior. Kept for back-compat with any
// composer that emits plain text (and as a safety net when the replier's JSON
// is malformed). Kind is whatever was configured on the poster (default
// "reply").
func (h *PRCommentPosterHandler) postLegacyPlain(ctx context.Context, fullRepo, owner, repo string, prNumber int, body string) ([]event.Envelope, error) {
	bodyHash := hashBody(body)

	if h.gh == nil {
		return []event.Envelope{h.postedEvent(h.kind, fullRepo, prNumber, bodyHash, 0, 0, true)}, nil
	}

	if existing, err := h.gh.GetIssueComments(ctx, owner, repo, prNumber); err == nil {
		if duplicateIssueHash(existing, bodyHash) {
			return []event.Envelope{h.postedEvent(h.kind, fullRepo, prNumber, bodyHash, 0, 0, true)}, nil
		}
	}
	// A failed dedup list is non-fatal — post anyway. Worst case: duplicate
	// comment if GitHub is flapping. The alternative (fail the handler) stalls
	// the workflow for something we can tolerate.

	comment, err := h.gh.CreatePRComment(ctx, owner, repo, prNumber, body)
	if err != nil {
		return nil, fmt.Errorf("%s: create pr comment: %w", h.name, err)
	}

	return []event.Envelope{h.postedEvent(h.kind, fullRepo, prNumber, bodyHash, comment.ID, 0, false)}, nil
}

// postedEvent builds the observability event. Stored on a persona-scoped
// aggregate by the runner, so it survives correlation-chain replay.
func (h *PRCommentPosterHandler) postedEvent(kind, repo string, prNumber int, bodyHash string, commentID, inReplyToID int, skipped bool) event.Envelope {
	return event.New(event.PRCommentPosted, 1, event.MustMarshal(event.PRCommentPostedPayload{
		Repo:        repo,
		PRNumber:    prNumber,
		Kind:        kind,
		CommentID:   commentID,
		InReplyToID: inReplyToID,
		BodyHash:    bodyHash,
		Skipped:     skipped,
	})).WithSource("handler:" + h.name)
}

// parseReplierJSON attempts to decode the upstream body as the structured
// pr-replier contract. Returns (_, false) only when the body is not valid
// JSON for this shape — at which point the caller falls back to the
// plain-text legacy path. Valid JSON with both fields empty is the
// replier's "nothing to say" signal and MUST be treated as the contract
// (the caller emits a skipped summary event instead of re-posting the
// raw JSON as a comment).
func parseReplierJSON(body string) (replierPayload, bool) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "{") {
		return replierPayload{}, false
	}
	var p replierPayload
	if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
		return replierPayload{}, false
	}
	return p, true
}

// extractPosterInputs pulls the PR source and the upstream persona's AI output
// body from the correlation chain. The latest AIResponseReceived for the
// upstream handler wins — accounts for feedback-loop re-runs where an older
// body would be stale.
func extractPosterInputs(events []event.Envelope, upstream string) (source, body string, err error) {
	wantSource := "handler:" + upstream
	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			var p event.WorkflowRequestedPayload
			if unmarshalErr := json.Unmarshal(e.Payload, &p); unmarshalErr == nil {
				source = p.Source
			}
		case event.AIResponseReceived:
			if e.Source != wantSource {
				continue
			}
			var p event.AIResponsePayload
			if unmarshalErr := json.Unmarshal(e.Payload, &p); unmarshalErr == nil {
				body = unmarshalOutput(p.Output, p.Structured)
			}
		}
	}
	if source == "" {
		return "", "", fmt.Errorf("no WorkflowRequested event found in correlation")
	}
	if body == "" {
		return "", "", fmt.Errorf("no AIResponseReceived from upstream %q", upstream)
	}
	return source, body, nil
}

// splitOwnerRepo splits "owner/repo" into its components.
func splitOwnerRepo(fullRepo string) (owner, repo string, ok bool) {
	parts := strings.SplitN(fullRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// hashBody returns the SHA-256 hex digest of the given body. Used as the
// dedup fingerprint — any whitespace-equivalent retry produces the same hash.
func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// duplicateIssueHash returns true if any issue comment hashes to the target.
// O(n) on the returned page — we only care about the most recent page's
// worth, since GitHub paginates at 30 by default.
func duplicateIssueHash(comments []gh.IssueComment, target string) bool {
	for _, c := range comments {
		if hashBody(strings.TrimSpace(c.Body)) == target {
			return true
		}
	}
	return false
}

// duplicateReviewReplyHash returns true if any existing review comment is a
// reply on `rootCommentID` with a body that hashes to the target. The thread
// root itself is intentionally excluded — Rick only wants to dedup against
// its OWN prior replies, not against the reviewer's original note (which has
// a different body anyway).
func duplicateReviewReplyHash(comments []gh.ReviewComment, rootCommentID int, target string) bool {
	for _, c := range comments {
		if c.InReplyToID != rootCommentID {
			continue
		}
		if hashBody(strings.TrimSpace(c.Body)) == target {
			return true
		}
	}
	return false
}
