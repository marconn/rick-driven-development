package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	gh "github.com/marconn/rick-event-driven-development/internal/github"
)

// ---------------------------------------------------------------------------
// Mock github client for the poster
// ---------------------------------------------------------------------------

type fakeGithub struct {
	// Existing comments returned by GetIssueComments. Nil ⇒ return nil slice.
	existing []gh.IssueComment
	// Existing inline review comments returned by GetPRReviewComments.
	// Used by the inline-reply dedup path.
	existingReview []gh.ReviewComment
	// Err returned by GetIssueComments. Non-nil bypasses dedup (handler posts
	// anyway — "list failed, tolerate risk of duplicate" is the documented
	// behavior in pr_comment_poster.go).
	listErr error

	// Captured on CreatePRComment.
	postedOwner    string
	postedRepo     string
	postedPR       int
	postedBody     string
	postedCalls    int
	createErr      error
	createResponse *gh.PRComment

	// Captured on CreatePRReviewCommentReply. Each call appends an entry.
	replies       []capturedReply
	replyCalls    int
	replyErr      error // non-nil → all replies fail
	replyErrOn    map[int]error // map comment_id → err (overrides replyErr for that ID only)
	nextReplyID   int
}

type capturedReply struct {
	Owner     string
	Repo      string
	PRNumber  int
	CommentID int
	Body      string
}

func (f *fakeGithub) CreatePRComment(_ context.Context, owner, repo string, prNumber int, body string) (*gh.PRComment, error) {
	f.postedCalls++
	f.postedOwner = owner
	f.postedRepo = repo
	f.postedPR = prNumber
	f.postedBody = body
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResponse != nil {
		return f.createResponse, nil
	}
	return &gh.PRComment{ID: 42, Body: body}, nil
}

func (f *fakeGithub) CreatePRReviewCommentReply(_ context.Context, owner, repo string, prNumber, commentID int, body string) (*gh.ReviewComment, error) {
	f.replyCalls++
	f.replies = append(f.replies, capturedReply{Owner: owner, Repo: repo, PRNumber: prNumber, CommentID: commentID, Body: body})
	if err, ok := f.replyErrOn[commentID]; ok {
		return nil, err
	}
	if f.replyErr != nil {
		return nil, f.replyErr
	}
	id := f.nextReplyID
	if id == 0 {
		id = 1000 + commentID
	} else {
		f.nextReplyID++
	}
	return &gh.ReviewComment{ID: id, InReplyToID: commentID, Body: body}, nil
}

func (f *fakeGithub) GetIssueComments(_ context.Context, _, _ string, _ int) ([]gh.IssueComment, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.existing, nil
}

func (f *fakeGithub) GetPRReviewComments(_ context.Context, _, _ string, _ int) ([]gh.ReviewComment, error) {
	return f.existingReview, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// seedPosterChain seeds a correlation with a WorkflowRequested (source) and
// the upstream persona's AIResponseReceived body. Returns the triggering
// envelope the handler will receive (PersonaCompleted for the upstream).
func seedPosterChain(store *mockStore, corr, source, upstream, body string) event.Envelope {
	req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "address PR feedback",
		Source: source,
	})).WithCorrelation(corr)

	// AIResponseReceived comes from the upstream persona. PlainText=true in
	// production means Structured=false and Output is a JSON string.
	outputJSON := event.MustMarshal(body)
	aiResp := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Phase:      "pr-reply",
		Backend:    "gemini",
		Structured: false,
		Output:     outputJSON,
	})).WithCorrelation(corr).WithSource("handler:" + upstream)

	store.correlationEvents[corr] = append(store.correlationEvents[corr], req, aiResp)

	return event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: upstream,
	})).WithCorrelation(corr)
}

func newReplyPoster(t *testing.T, store *mockStore, ghc prCommentClient) *PRCommentPosterHandler {
	t.Helper()
	return NewPRCommentPoster(PRCommentPosterConfig{
		Name:     "pr-reply-poster",
		Upstream: "pr-replier",
		Kind:     "reply",
		GitHub:   ghc,
		Store:    store,
	})
}

func posterPayload(t *testing.T, env event.Envelope) event.PRCommentPostedPayload {
	t.Helper()
	if env.Type != event.PRCommentPosted {
		t.Fatalf("expected event type %q, got %q", event.PRCommentPosted, env.Type)
	}
	var p event.PRCommentPostedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Happy path — body posted, PRCommentPosted with comment_id
// ---------------------------------------------------------------------------

func TestPRCommentPoster_PostsAndEmitsEvent(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-1", "gh:owner/repo#7", "pr-replier", "Thanks for the review — all addressed.")
	fg := &fakeGithub{}

	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	if fg.postedCalls != 1 {
		t.Errorf("expected 1 CreatePRComment call, got %d", fg.postedCalls)
	}
	if fg.postedOwner != "owner" || fg.postedRepo != "repo" || fg.postedPR != 7 {
		t.Errorf("routing mismatch: owner=%q repo=%q pr=%d", fg.postedOwner, fg.postedRepo, fg.postedPR)
	}
	if fg.postedBody != "Thanks for the review — all addressed." {
		t.Errorf("body mismatch: got %q", fg.postedBody)
	}

	p := posterPayload(t, events[0])
	if p.Skipped {
		t.Errorf("expected Skipped=false, got true")
	}
	if p.CommentID != 42 {
		t.Errorf("expected CommentID=42, got %d", p.CommentID)
	}
	if p.Kind != "reply" {
		t.Errorf("expected Kind=reply, got %q", p.Kind)
	}
	if p.BodyHash == "" || len(p.BodyHash) != 64 {
		t.Errorf("expected 64-char sha256 hex, got %q (len=%d)", p.BodyHash, len(p.BodyHash))
	}
}

// ---------------------------------------------------------------------------
// Dedup — identical body already on PR => skip, do not post, emit skipped event
// ---------------------------------------------------------------------------

func TestPRCommentPoster_DedupesByBodyHash(t *testing.T) {
	store := newMockStore()
	body := "Thanks for the review — all addressed."
	trig := seedPosterChain(store, "corr-dedup", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		existing: []gh.IssueComment{
			{ID: 11, Body: "unrelated comment"},
			{ID: 12, Body: body}, // exact match → dedup fires
		},
	}

	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 {
		t.Errorf("expected 0 CreatePRComment calls after dedup, got %d", fg.postedCalls)
	}

	p := posterPayload(t, events[0])
	if !p.Skipped {
		t.Errorf("expected Skipped=true after dedup")
	}
	if p.CommentID != 0 {
		t.Errorf("expected CommentID=0 when skipped, got %d", p.CommentID)
	}
}

// TestPRCommentPoster_DedupesWhitespaceIdentical verifies the hash is stable
// across leading/trailing whitespace because the handler trims before hashing.
func TestPRCommentPoster_DedupesWhitespaceIdentical(t *testing.T) {
	store := newMockStore()
	// Upstream body has trailing newlines — trimmed before hashing.
	trig := seedPosterChain(store, "corr-ws", "gh:owner/repo#7", "pr-replier", "addressed\n\n\n")

	fg := &fakeGithub{
		existing: []gh.IssueComment{
			{ID: 1, Body: "addressed"},
		},
	}

	h := newReplyPoster(t, store, fg)
	_, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 {
		t.Errorf("whitespace-only difference should dedup; CreatePRComment called %d times", fg.postedCalls)
	}
}

// TestPRCommentPoster_ListFailureIsNonFatal — dedup list call failure tolerates
// and posts anyway. Documented trade-off in pr_comment_poster.go: a flapping
// GitHub would otherwise stall the workflow entirely.
func TestPRCommentPoster_ListFailureIsNonFatal(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-listerr", "gh:owner/repo#7", "pr-replier", "body")

	fg := &fakeGithub{listErr: errors.New("HTTP 502: bad gateway")}

	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 1 {
		t.Errorf("expected 1 CreatePRComment call (list err is non-fatal), got %d", fg.postedCalls)
	}
	p := posterPayload(t, events[0])
	if p.Skipped {
		t.Errorf("expected Skipped=false when dedup list fails and post succeeds")
	}
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestPRCommentPoster_MissingCorrelation(t *testing.T) {
	h := newReplyPoster(t, newMockStore(), &fakeGithub{})
	_, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil))
	if err == nil {
		t.Fatal("expected error for missing correlation id")
	}
}

func TestPRCommentPoster_NoWorkflowRequestedInChain(t *testing.T) {
	store := newMockStore()
	corr := "corr-nosource"
	store.correlationEvents[corr] = []event.Envelope{
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:      "pr-reply",
			Backend:    "gemini",
			Structured: false,
			Output:     event.MustMarshal("body"),
		})).WithCorrelation(corr).WithSource("handler:pr-replier"),
	}
	trig := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corr)

	h := newReplyPoster(t, store, &fakeGithub{})
	_, err := h.Handle(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error when WorkflowRequested is missing from chain")
	}
}

func TestPRCommentPoster_NoUpstreamOutputInChain(t *testing.T) {
	store := newMockStore()
	corr := "corr-nobody"
	store.correlationEvents[corr] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "x",
			Source: "gh:owner/repo#1",
		})).WithCorrelation(corr),
	}
	trig := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corr)

	h := newReplyPoster(t, store, &fakeGithub{})
	_, err := h.Handle(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error when upstream AIResponseReceived is missing")
	}
}

// TestPRCommentPoster_IgnoresOtherUpstreams guards against a regression where
// the poster might pick up output from the wrong upstream (e.g., developer
// instead of pr-replier). It must filter AIResponseReceived by
// Source="handler:<upstream>".
func TestPRCommentPoster_IgnoresOtherUpstreams(t *testing.T) {
	store := newMockStore()
	corr := "corr-multi"

	store.correlationEvents[corr] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "x",
			Source: "gh:owner/repo#3",
		})).WithCorrelation(corr),

		// Developer AI output — must NOT be posted.
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:      "develop",
			Structured: false,
			Output:     event.MustMarshal("developer code"),
		})).WithCorrelation(corr).WithSource("handler:developer"),

		// pr-replier AI output — this is what must be posted.
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:      "pr-reply",
			Structured: false,
			Output:     event.MustMarshal("reply body"),
		})).WithCorrelation(corr).WithSource("handler:pr-replier"),
	}
	trig := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corr)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedBody != "reply body" {
		t.Errorf("expected pr-replier body to be posted, got %q", fg.postedBody)
	}
}

// TestPRCommentPoster_UsesLatestUpstreamOutput — feedback loops cause the
// upstream to run multiple times. The poster must use the latest
// AIResponseReceived, not the first.
func TestPRCommentPoster_UsesLatestUpstreamOutput(t *testing.T) {
	store := newMockStore()
	corr := "corr-iter"

	store.correlationEvents[corr] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Source: "gh:owner/repo#4",
		})).WithCorrelation(corr),

		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Output: event.MustMarshal("stale body from iteration 1"),
		})).WithCorrelation(corr).WithSource("handler:pr-replier"),

		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Output: event.MustMarshal("fresh body from iteration 2"),
		})).WithCorrelation(corr).WithSource("handler:pr-replier"),
	}
	trig := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corr)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedBody != "fresh body from iteration 2" {
		t.Errorf("expected latest iteration's body, got %q", fg.postedBody)
	}
}

func TestPRCommentPoster_InvalidSource(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-bad", "raw", "pr-replier", "body")

	h := newReplyPoster(t, store, &fakeGithub{})
	_, err := h.Handle(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error for non-gh source")
	}
}

func TestPRCommentPoster_EmptyBodyErrors(t *testing.T) {
	store := newMockStore()
	// Upstream produced an empty string — signals a composer failure we
	// shouldn't paper over by posting nothing.
	trig := seedPosterChain(store, "corr-empty", "gh:owner/repo#1", "pr-replier", "   \n\t  ")

	h := newReplyPoster(t, store, &fakeGithub{})
	_, err := h.Handle(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error on whitespace-only body")
	}
}

func TestPRCommentPoster_CreateFailureReturnsError(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-err", "gh:owner/repo#1", "pr-replier", "body")

	fg := &fakeGithub{createErr: errors.New("HTTP 403: forbidden")}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err == nil {
		t.Fatal("expected error from CreatePRComment failure")
	}
}

// ---------------------------------------------------------------------------
// Observability-only mode: nil github client → skipped event, no crash
// ---------------------------------------------------------------------------

func TestPRCommentPoster_NilClientSkips(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-nil", "gh:owner/repo#1", "pr-replier", "body")

	h := newReplyPoster(t, store, nil)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	p := posterPayload(t, events[0])
	if !p.Skipped {
		t.Error("expected Skipped=true when GitHub client is nil")
	}
}

// ---------------------------------------------------------------------------
// Structured-JSON contract (current pr-replier output shape)
// ---------------------------------------------------------------------------

// TestPRCommentPoster_StructuredInlineReplies verifies that a replier output
// carrying ONLY inline_replies (no summary) fans out to one reply post per
// thread and does not emit a top-level issue comment.
func TestPRCommentPoster_StructuredInlineReplies(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":101,"body":"Fixed in commit abc123."},
		{"comment_id":202,"body":"Deferred — tracked in #789."}
	]}`
	trig := seedPosterChain(store, "corr-inline", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if fg.postedCalls != 0 {
		t.Errorf("expected 0 top-level posts, got %d", fg.postedCalls)
	}
	if fg.replyCalls != 2 {
		t.Fatalf("expected 2 inline reply posts, got %d", fg.replyCalls)
	}
	gotRoots := []int{fg.replies[0].CommentID, fg.replies[1].CommentID}
	if gotRoots[0] != 101 || gotRoots[1] != 202 {
		t.Errorf("reply targeting mismatch: got %v, want [101 202]", gotRoots)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 PRCommentPosted events, got %d", len(events))
	}
	for i, e := range events {
		p := posterPayload(t, e)
		if p.Kind != "inline-reply" {
			t.Errorf("event %d: kind=%q, want inline-reply", i, p.Kind)
		}
		if p.Skipped {
			t.Errorf("event %d: expected Skipped=false", i)
		}
	}
}

// TestPRCommentPoster_StructuredSummaryAndReplies exercises the full contract:
// a top-level summary alongside per-thread replies. Must produce one summary
// post + one reply per thread, each with the correct kind.
func TestPRCommentPoster_StructuredSummaryAndReplies(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"Addressed 2 of 3. One deferred — see inline.","inline_replies":[
		{"comment_id":10,"body":"Done."},
		{"comment_id":20,"body":"Deferred."}
	]}`
	trig := seedPosterChain(store, "corr-mix", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if fg.postedCalls != 1 {
		t.Errorf("expected 1 summary post, got %d", fg.postedCalls)
	}
	if fg.replyCalls != 2 {
		t.Errorf("expected 2 inline reply posts, got %d", fg.replyCalls)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events (1 summary + 2 inline), got %d", len(events))
	}

	kinds := map[string]int{}
	for _, e := range events {
		p := posterPayload(t, e)
		kinds[p.Kind]++
	}
	if kinds["summary"] != 1 || kinds["inline-reply"] != 2 {
		t.Errorf("kind distribution mismatch: %v", kinds)
	}
}

// TestPRCommentPoster_StructuredDedupsPerThread verifies that an inline reply
// whose body already exists on the thread (matched by in_reply_to_id + body
// hash) is skipped, not re-posted. Critical for idempotent feedback loops —
// a second pr-feedback round must not double-reply on the same thread.
func TestPRCommentPoster_StructuredDedupsPerThread(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":500,"body":"Already addressed in abc123."}
	]}`
	trig := seedPosterChain(store, "corr-dedup-reply", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		existingReview: []gh.ReviewComment{
			// Unrelated reply on a different thread.
			{ID: 900, InReplyToID: 600, Body: "Already addressed in abc123."},
			// Rick's prior reply on the target thread — must trigger dedup.
			{ID: 901, InReplyToID: 500, Body: "Already addressed in abc123."},
		},
	}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 0 {
		t.Errorf("expected 0 reply posts after dedup, got %d", fg.replyCalls)
	}
	p := posterPayload(t, events[0])
	if !p.Skipped {
		t.Errorf("expected Skipped=true for deduped inline reply")
	}
	if p.InReplyToID != 500 {
		t.Errorf("expected InReplyToID=500, got %d", p.InReplyToID)
	}
}

// TestPRCommentPoster_StructuredNoopReturnsSkipped guards the empty contract:
// when replier returns {"summary":"","inline_replies":[]}, the poster must
// NOT fall through to the legacy plain-text path (which would post the raw
// JSON). Instead it emits a single skipped summary event so the DAG advances.
func TestPRCommentPoster_StructuredNoopReturnsSkipped(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[]}`
	trig := seedPosterChain(store, "corr-noop", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 || fg.replyCalls != 0 {
		t.Errorf("expected zero posts, got summary=%d replies=%d", fg.postedCalls, fg.replyCalls)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 skipped event, got %d", len(events))
	}
	p := posterPayload(t, events[0])
	if !p.Skipped || p.Kind != "summary" {
		t.Errorf("expected skipped summary event, got kind=%q skipped=%v", p.Kind, p.Skipped)
	}
}

// TestPRCommentPoster_StructuredPartialFailureContinues — when one inline
// reply fails (e.g., GitHub returns 404 on a stale comment_id), the other
// replies must still post. Handler returns the failure so the caller
// surfaces it, but successful posts still land.
func TestPRCommentPoster_StructuredPartialFailureContinues(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":11,"body":"OK A"},
		{"comment_id":22,"body":"OK B — but this one 404s"},
		{"comment_id":33,"body":"OK C"}
	]}`
	trig := seedPosterChain(store, "corr-partial", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		replyErrOn: map[int]error{22: errors.New("HTTP 404: comment gone")},
	}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err == nil {
		t.Fatal("expected error surfacing the failed inline reply")
	}

	if fg.replyCalls != 3 {
		t.Errorf("expected 3 reply attempts (partial-failure continues), got %d", fg.replyCalls)
	}
	// Two successful replies emit events; the failed one does not.
	success := 0
	for _, e := range events {
		p := posterPayload(t, e)
		if p.Kind == "inline-reply" && !p.Skipped && p.CommentID != 0 {
			success++
		}
	}
	if success != 2 {
		t.Errorf("expected 2 successful inline replies to emit events, got %d", success)
	}
}

// TestPRCommentPoster_LegacyPlainFallback verifies that a non-JSON upstream
// body still posts a top-level comment — the safety net for composers that
// haven't migrated to the structured contract yet.
func TestPRCommentPoster_LegacyPlainFallback(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-legacy", "gh:owner/repo#7", "pr-replier", "Thanks — addressed.")

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 1 {
		t.Errorf("expected 1 top-level post, got %d", fg.postedCalls)
	}
	if fg.replyCalls != 0 {
		t.Errorf("expected 0 inline reply posts, got %d", fg.replyCalls)
	}
	p := posterPayload(t, events[0])
	if p.Kind != "reply" {
		t.Errorf("expected Kind=reply (legacy fallback), got %q", p.Kind)
	}
}

// ---------------------------------------------------------------------------
// Hash stability — same body → same hash across calls
// ---------------------------------------------------------------------------

func TestHashBody_Stable(t *testing.T) {
	a := hashBody("hello world")
	b := hashBody("hello world")
	if a != b {
		t.Errorf("hashBody non-stable: %q != %q", a, b)
	}
	if a == hashBody("hello world ") {
		t.Errorf("trailing whitespace should change hash (handler trims caller-side before hashing)")
	}
}
