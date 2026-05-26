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

	// botLogin returned by GetAuthenticatedUser. Empty string ⇒ return a User
	// with empty Login (identity dedup disabled). botLoginErr forces an error
	// to exercise the graceful-degradation path.
	botLogin       string
	botLoginErr    error
	botLoginCalls  int

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

func (f *fakeGithub) GetAuthenticatedUser(_ context.Context) (*gh.User, error) {
	f.botLoginCalls++
	if f.botLoginErr != nil {
		return nil, f.botLoginErr
	}
	return &gh.User{Login: f.botLogin}, nil
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
			Structured: false,
			Output:     event.MustMarshal("developer code"),
		})).WithCorrelation(corr).WithSource("handler:developer"),

		// pr-replier AI output — this is what must be posted.
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
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

// ---------------------------------------------------------------------------
// Correlation-chain self-check (Fix A)
//
// GitHub-side body-hash dedup misses across feedback-loop iterations because
// the pr-replier LLM produces different wording each round. These tests guard
// the deterministic "did I already post in this correlation?" check that
// consults the poster's own PRCommentPosted history before calling GitHub.
// ---------------------------------------------------------------------------

// seedPriorPost appends a PRCommentPosted event authored by the poster to the
// correlation, mirroring what executeDispatch would persist after a successful
// post. Used by the Fix A regression tests to simulate a prior iteration.
func seedPriorPost(store *mockStore, corr, handlerName, kind string, inReplyToID, commentID int, skipped bool) {
	evt := event.New(event.PRCommentPosted, 1, event.MustMarshal(event.PRCommentPostedPayload{
		Repo:        "owner/repo",
		PRNumber:    7,
		Kind:        kind,
		CommentID:   commentID,
		InReplyToID: inReplyToID,
		BodyHash:    "dummyhash",
		Skipped:     skipped,
	})).WithCorrelation(corr).WithSource("handler:" + handlerName)
	store.correlationEvents[corr] = append(store.correlationEvents[corr], evt)
}

// TestPRCommentPoster_SkipsInlineReplyWhenPriorPostExists is the core Fix A
// regression: iteration 2's reply text differs from iteration 1's, so
// GitHub-side hash dedup would miss. The correlation-chain check must skip
// regardless.
func TestPRCommentPoster_SkipsInlineReplyWhenPriorPostExists(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":500,"body":"Iteration 2 — different wording than iteration 1."}
	]}`
	trig := seedPosterChain(store, "corr-iter-reply", "gh:owner/repo#7", "pr-replier", body)

	// Iteration 1 already posted on this thread; body hash on GitHub would
	// differ from iteration 2's body, so the existing GitHub-side dedup would
	// not catch it. The chain-history check must.
	seedPriorPost(store, "corr-iter-reply", "pr-reply-poster", "inline-reply", 500, 901, false)

	fg := &fakeGithub{
		// Simulate the prior post landing on GitHub with the *old* body so
		// the body-hash dedup cannot catch the new text.
		existingReview: []gh.ReviewComment{
			{ID: 901, InReplyToID: 500, Body: "Iteration 1 — original wording."},
		},
	}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 0 {
		t.Errorf("expected 0 reply posts via chain-history dedup, got %d", fg.replyCalls)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 skipped event, got %d", len(events))
	}
	p := posterPayload(t, events[0])
	if !p.Skipped {
		t.Errorf("expected Skipped=true after chain-history match")
	}
	if p.Kind != "inline-reply" || p.InReplyToID != 500 {
		t.Errorf("expected kind=inline-reply InReplyToID=500, got kind=%q InReplyToID=%d", p.Kind, p.InReplyToID)
	}
}

// TestPRCommentPoster_SkipsSummaryWhenPriorPostExists is the summary-side
// twin: the LLM's iteration-2 summary text differs from iteration-1's, so
// only the chain-history check catches it.
func TestPRCommentPoster_SkipsSummaryWhenPriorPostExists(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"Iteration 2 summary — fresh wording.","inline_replies":[]}`
	trig := seedPosterChain(store, "corr-iter-summary", "gh:owner/repo#7", "pr-replier", body)

	seedPriorPost(store, "corr-iter-summary", "pr-reply-poster", "summary", 0, 50, false)

	fg := &fakeGithub{
		existing: []gh.IssueComment{
			{ID: 50, Body: "Iteration 1 summary — original wording."},
		},
	}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 {
		t.Errorf("expected 0 summary posts via chain-history dedup, got %d", fg.postedCalls)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 skipped summary event, got %d", len(events))
	}
	p := posterPayload(t, events[0])
	if !p.Skipped || p.Kind != "summary" {
		t.Errorf("expected skipped summary event, got kind=%q skipped=%v", p.Kind, p.Skipped)
	}
}

// TestPRCommentPoster_SkipsLegacyWhenPriorPostExists covers the plain-text
// fallback path through the same lens.
func TestPRCommentPoster_SkipsLegacyWhenPriorPostExists(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-iter-legacy", "gh:owner/repo#7", "pr-replier", "Plain text iteration 2.")

	seedPriorPost(store, "corr-iter-legacy", "pr-reply-poster", "reply", 0, 99, false)

	fg := &fakeGithub{
		existing: []gh.IssueComment{
			{ID: 99, Body: "Plain text iteration 1."},
		},
	}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 {
		t.Errorf("expected 0 legacy posts via chain-history dedup, got %d", fg.postedCalls)
	}
	p := posterPayload(t, events[0])
	if !p.Skipped || p.Kind != "reply" {
		t.Errorf("expected skipped legacy reply event, got kind=%q skipped=%v", p.Kind, p.Skipped)
	}
}

// TestPRCommentPoster_PartialPriorHistory verifies the chain check is
// per-thread: a prior reply on thread A must not suppress a fresh reply on
// thread B in the same correlation.
func TestPRCommentPoster_PartialPriorHistory(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":100,"body":"Already replied iteration 1."},
		{"comment_id":200,"body":"New thread, first reply."}
	]}`
	trig := seedPosterChain(store, "corr-partial-history", "gh:owner/repo#7", "pr-replier", body)

	// Only thread 100 has prior history.
	seedPriorPost(store, "corr-partial-history", "pr-reply-poster", "inline-reply", 100, 901, false)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 1 {
		t.Errorf("expected 1 reply post (thread 200 only), got %d", fg.replyCalls)
	}
	if fg.replyCalls == 1 && fg.replies[0].CommentID != 200 {
		t.Errorf("expected reply on thread 200, got %d", fg.replies[0].CommentID)
	}

	var skipped, posted int
	for _, e := range events {
		p := posterPayload(t, e)
		if p.Kind != "inline-reply" {
			continue
		}
		if p.Skipped {
			skipped++
			if p.InReplyToID != 100 {
				t.Errorf("expected skipped event for thread 100, got %d", p.InReplyToID)
			}
		} else {
			posted++
			if p.InReplyToID != 200 {
				t.Errorf("expected posted event for thread 200, got %d", p.InReplyToID)
			}
		}
	}
	if skipped != 1 || posted != 1 {
		t.Errorf("event distribution mismatch: skipped=%d posted=%d", skipped, posted)
	}
}

// TestPRCommentPoster_SkippedPriorDoesNotSuppress asserts the chain check
// only counts non-skipped history. A prior Skipped=true event means we never
// actually called GitHub — it must NOT short-circuit a real post.
func TestPRCommentPoster_SkippedPriorDoesNotSuppress(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":777,"body":"Real reply this time."}
	]}`
	trig := seedPosterChain(store, "corr-skipped-prior", "gh:owner/repo#7", "pr-replier", body)

	// A prior Skipped=true event (e.g. the upstream noop signal or a prior
	// chain-history skip). Must not block a fresh real attempt.
	seedPriorPost(store, "corr-skipped-prior", "pr-reply-poster", "inline-reply", 777, 0, true)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 1 {
		t.Errorf("expected 1 reply post (prior was Skipped=true), got %d", fg.replyCalls)
	}
}

// TestPRCommentPoster_OtherHandlerHistoryIgnored guards against cross-handler
// confusion. A PRCommentPosted authored by pr-consolidator on the same
// correlation must not suppress pr-reply-poster's work — Source filter must
// match `handler:<name>` exactly.
func TestPRCommentPoster_OtherHandlerHistoryIgnored(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":888,"body":"My own first reply."}
	]}`
	trig := seedPosterChain(store, "corr-other-handler", "gh:owner/repo#7", "pr-replier", body)

	// Posted by a *different* handler — must be ignored by pr-reply-poster's
	// chain check.
	seedPriorPost(store, "corr-other-handler", "pr-consolidator", "inline-reply", 888, 999, false)

	fg := &fakeGithub{}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 1 {
		t.Errorf("expected 1 reply post (other-handler history must not suppress), got %d", fg.replyCalls)
	}
}

// ---------------------------------------------------------------------------
// Author-identity dedup (Fix B)
//
// Closes the cross-correlation gap that Fix A cannot: when the same PR is
// re-targeted by a fresh pr-feedback run, the new correlation has no history
// of its own, so Fix A's chain-check passes through. The bot's GitHub login
// (resolved lazily via GET /user) is the durable identity signal — if the
// bot already replied on this thread, suppress regardless of body text.
// ---------------------------------------------------------------------------

// TestPRCommentPoster_InlineReplySkipsWhenBotAlreadyReplied is the core Fix B
// scenario for inline threads: a fresh correlation, no prior chain history,
// but the bot has a reply on the thread from a previous workflow. Author
// identity catches it; body-hash would miss because LLM wording differs.
func TestPRCommentPoster_InlineReplySkipsWhenBotAlreadyReplied(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":500,"body":"Fresh correlation, different wording."}
	]}`
	trig := seedPosterChain(store, "corr-fresh", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		botLogin: "rick-bot",
		existingReview: []gh.ReviewComment{
			// Bot's prior reply on the target thread from an earlier run —
			// completely different body, so duplicateReviewReplyHash misses.
			{ID: 901, InReplyToID: 500, Body: "Older wording.", User: gh.User{Login: "rick-bot"}},
		},
	}
	h := newReplyPoster(t, store, fg)
	events, err := h.Handle(context.Background(), trig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 0 {
		t.Errorf("expected 0 reply posts via identity dedup, got %d", fg.replyCalls)
	}
	if len(events) != 1 || !posterPayload(t, events[0]).Skipped {
		t.Errorf("expected single skipped event")
	}
}

// TestPRCommentPoster_InlineReplyIgnoresOtherUserReplies guards the precision
// of the identity check: a reply on the same thread by a non-bot user must
// NOT suppress Rick's reply.
func TestPRCommentPoster_InlineReplyIgnoresOtherUserReplies(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":500,"body":"Rick's first reply."}
	]}`
	trig := seedPosterChain(store, "corr-other-user", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		botLogin: "rick-bot",
		existingReview: []gh.ReviewComment{
			// Human reviewer replied on the thread — must not block Rick.
			{ID: 901, InReplyToID: 500, Body: "Reviewer follow-up.", User: gh.User{Login: "alice"}},
		},
	}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 1 {
		t.Errorf("non-bot replies must not suppress; expected 1 reply, got %d", fg.replyCalls)
	}
}

// TestPRCommentPoster_InlineReplyIgnoresBotOnDifferentThread guards thread
// precision: a bot reply on a *different* thread must not suppress this
// thread's reply. authorRepliedOnThread filters by InReplyToID.
func TestPRCommentPoster_InlineReplyIgnoresBotOnDifferentThread(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"","inline_replies":[
		{"comment_id":500,"body":"Reply for thread 500."}
	]}`
	trig := seedPosterChain(store, "corr-other-thread", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		botLogin: "rick-bot",
		existingReview: []gh.ReviewComment{
			// Bot replied on a different thread — must not suppress thread 500.
			{ID: 901, InReplyToID: 999, Body: "Bot reply on 999.", User: gh.User{Login: "rick-bot"}},
		},
	}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.replyCalls != 1 {
		t.Errorf("bot on different thread must not suppress; expected 1 reply, got %d", fg.replyCalls)
	}
}

// TestPRCommentPoster_SummarySkipsWhenBotAlreadyCommented mirrors the inline
// behavior for top-level issue comments.
func TestPRCommentPoster_SummarySkipsWhenBotAlreadyCommented(t *testing.T) {
	store := newMockStore()
	body := `{"summary":"Cross-correlation summary, fresh wording.","inline_replies":[]}`
	trig := seedPosterChain(store, "corr-summary-fresh", "gh:owner/repo#7", "pr-replier", body)

	fg := &fakeGithub{
		botLogin: "rick-bot",
		existing: []gh.IssueComment{
			// Bot's prior summary from a previous workflow — different body.
			{ID: 50, Body: "Older summary.", User: gh.User{Login: "rick-bot"}},
		},
	}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 {
		t.Errorf("expected 0 summary posts via identity dedup, got %d", fg.postedCalls)
	}
}

// TestPRCommentPoster_LegacySkipsWhenBotAlreadyCommented covers the
// plain-text fallback path through the same lens.
func TestPRCommentPoster_LegacySkipsWhenBotAlreadyCommented(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-legacy-fresh", "gh:owner/repo#7", "pr-replier", "Fresh plain-text body.")

	fg := &fakeGithub{
		botLogin: "rick-bot",
		existing: []gh.IssueComment{
			{ID: 50, Body: "Older plain-text body.", User: gh.User{Login: "rick-bot"}},
		},
	}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fg.postedCalls != 0 {
		t.Errorf("expected 0 legacy posts via identity dedup, got %d", fg.postedCalls)
	}
}

// TestPRCommentPoster_BotLoginLookupFailureDegradesGracefully verifies the
// safety net: if GET /user fails, identity dedup is silently disabled and
// the existing body-hash + correlation-chain layers must still work.
func TestPRCommentPoster_BotLoginLookupFailureDegradesGracefully(t *testing.T) {
	store := newMockStore()
	trig := seedPosterChain(store, "corr-whoami-fail", "gh:owner/repo#7", "pr-replier", "Hello.")

	fg := &fakeGithub{botLoginErr: errors.New("HTTP 401: bad credentials")}
	h := newReplyPoster(t, store, fg)
	if _, err := h.Handle(context.Background(), trig); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Identity dedup off → still posts via body-hash path.
	if fg.postedCalls != 1 {
		t.Errorf("expected 1 post after identity dedup degraded, got %d", fg.postedCalls)
	}
}

// TestPRCommentPoster_BotLoginResolvedOnce verifies the sync.Once contract:
// across multiple Handle invocations, GET /user is called exactly once.
func TestPRCommentPoster_BotLoginResolvedOnce(t *testing.T) {
	store := newMockStore()

	fg := &fakeGithub{botLogin: "rick-bot"}
	h := newReplyPoster(t, store, fg)

	for i, corr := range []string{"a", "b", "c"} {
		body := `{"summary":"","inline_replies":[{"comment_id":1,"body":"x"}]}`
		trig := seedPosterChain(store, corr, "gh:owner/repo#1", "pr-replier", body)
		if _, err := h.Handle(context.Background(), trig); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if fg.botLoginCalls != 1 {
		t.Errorf("expected GET /user called exactly once across invocations, got %d", fg.botLoginCalls)
	}
}
