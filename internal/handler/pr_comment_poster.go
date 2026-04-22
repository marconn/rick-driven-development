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

// PRCommentPosterHandler posts a comment to a GitHub PR on behalf of a text-only
// upstream persona (currently pr-replier; the design accepts any text-only
// composer). It exists to solve the same failure mode as pr-consolidator: when
// the LLM that composed the body also had tool access, it would run
// `gh pr comment` proactively AND the handler would post again, producing
// duplicates (see the 2026-04-17 pr-feedback incident on hulilabs/huli#689).
//
// The poster:
//  1. Loads WorkflowRequestedPayload to resolve owner/repo/PR number from Source.
//  2. Reads the upstream persona's AIResponseReceived from the correlation
//     chain (matched by Source = "handler:<upstream>"). That text becomes the
//     comment body verbatim.
//  3. Dedupes by SHA-256: if the last page of issue comments already contains
//     a body whose hash matches, the post is skipped and a PRCommentPosted
//     event with Skipped=true is still emitted so the event stream records
//     the decision.
//  4. Otherwise, calls github.Client.CreatePRComment and emits PRCommentPosted
//     with the resulting comment ID.
//
// Registered once per (Name, Upstream, Kind) tuple — currently only the
// pr-reply-poster instance exists.
type PRCommentPosterHandler struct {
	name     string
	upstream string // handler name whose AI output becomes the body
	kind     string // "reply" or "summary" — recorded on PRCommentPosted
	gh       prCommentClient
	store    eventstore.Store
}

// prCommentClient is the subset of github.Client used by the poster. Narrowing
// the surface here keeps the handler trivially mockable in tests without
// round-tripping real HTTP.
type prCommentClient interface {
	CreatePRComment(ctx context.Context, owner, repo string, prNumber int, body string) (*gh.PRComment, error)
	GetIssueComments(ctx context.Context, owner, repo string, number int) ([]gh.IssueComment, error)
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

// GetIssueComments delegates to the wrapped client.
func (a PRCommentClientAdapter) GetIssueComments(ctx context.Context, owner, repo string, number int) ([]gh.IssueComment, error) {
	return a.Client.GetIssueComments(ctx, owner, repo, number)
}

// PRCommentPosterConfig configures a poster instance.
type PRCommentPosterConfig struct {
	Name     string            // handler name, e.g. "pr-reply-poster"
	Upstream string            // predecessor handler whose AI output is posted
	Kind     string            // PRCommentPostedPayload.Kind, e.g. "reply"
	GitHub   prCommentClient   // may be nil — handler short-circuits with an enrichment-only path
	Store    eventstore.Store  // required for loading correlation chain
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

// Handle reads the upstream body and posts (or skips) the PR comment.
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
	bodyHash := hashBody(body)

	// Nil GitHub client = observability-only mode (tests, or env without
	// GITHUB_TOKEN). Emit the skipped event so the DAG still advances.
	if h.gh == nil {
		return []event.Envelope{h.postedEvent(fullRepo, prNumber, bodyHash, 0, true)}, nil
	}

	if existing, err := h.gh.GetIssueComments(ctx, owner, repo, prNumber); err == nil {
		if duplicateHash(existing, bodyHash) {
			return []event.Envelope{h.postedEvent(fullRepo, prNumber, bodyHash, 0, true)}, nil
		}
	}
	// A failed dedup list is non-fatal — post anyway. Worst case: duplicate
	// comment if GitHub is flapping. The alternative (fail the handler) stalls
	// the workflow for something we can tolerate.

	comment, err := h.gh.CreatePRComment(ctx, owner, repo, prNumber, body)
	if err != nil {
		return nil, fmt.Errorf("%s: create pr comment: %w", h.name, err)
	}

	return []event.Envelope{h.postedEvent(fullRepo, prNumber, bodyHash, comment.ID, false)}, nil
}

// postedEvent builds the observability event. Stored on a persona-scoped
// aggregate by the runner, so it survives correlation-chain replay.
func (h *PRCommentPosterHandler) postedEvent(repo string, prNumber int, bodyHash string, commentID int, skipped bool) event.Envelope {
	return event.New(event.PRCommentPosted, 1, event.MustMarshal(event.PRCommentPostedPayload{
		Repo:      repo,
		PRNumber:  prNumber,
		Kind:      h.kind,
		CommentID: commentID,
		BodyHash:  bodyHash,
		Skipped:   skipped,
	})).WithSource("handler:" + h.name)
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

// duplicateHash returns true if any comment body in the slice hashes to the
// target. Intentionally O(n) — comment lists are paginated at 30 by default
// and we only care about the most recent page's worth.
func duplicateHash(comments []gh.IssueComment, target string) bool {
	for _, c := range comments {
		if hashBody(c.Body) == target {
			return true
		}
	}
	return false
}
