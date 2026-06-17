package handler

import (
	"slices"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestGroundingTokensExtractsDoubleBacktickIdentifier is a regression for the
// silent-drop bug found on gh:hulilabs/huli#2082 (workflow 7384fb65): reviewers
// wrap a cited identifier in a DOUBLE-backtick markdown code span (`` `Sym` ``),
// the idiom for code spans whose content contains backticks — which the review
// prompt's own Grounding Contract examples render. The single-backtick extractor
// mis-paired the delimiters, captured the surrounding whitespace/prose instead of
// the identifier, and the real finding (including a `critical`) collapsed to a
// canned PASS. The extractor must recover the identifier regardless of how many
// backticks the LLM used to delimit the span.
func TestGroundingTokensExtractsDoubleBacktickIdentifier(t *testing.T) {
	cases := []struct {
		name string
		desc string
		want string // identifier that MUST be extracted as a grounding token
	}{
		{
			// Verbatim shape of the dropped pr-api-contract finding #1 on #2082.
			name: "double_backtick_wrapped_identifier",
			desc: "`major` `errors.go:108` `` `ErrSignupCountryUnsupported` `` — new HTTP 400 (`HULI-10055`) breaks clients.",
			want: "ErrSignupCountryUnsupported",
		},
		{
			// Verbatim shape of the dropped pr-api-contract finding #2 on #2082.
			name: "double_backtick_wrapped_method",
			desc: "`critical` `queries.go:263` `` `SubscriptionCreateGated` `` — returns `402 Payment Required`.",
			want: "SubscriptionCreateGated",
		},
		{
			// Backward-compat guard: the prescribed single-backtick form must keep
			// working exactly as before.
			name: "single_backtick_still_extracts",
			desc: "`major` `errors.go:108` `ErrSignupCountryUnsupported` — breaks clients.",
			want: "ErrSignupCountryUnsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens := groundingTokens(tc.desc)
			if !slices.Contains(tokens, tc.want) {
				t.Fatalf("groundingTokens(%q) = %q; want it to contain %q", tc.desc, tokens, tc.want)
			}
		})
	}
}

// TestGroundIssueRescuesDoubleBacktickFinding drives the full grounding path with
// a finding whose identifier is double-backtick-wrapped and whose cited line is a
// real changed line. On the buggy single-backtick extractor the identifier is
// lost, no rescue anchor exists, and the issue drops as token_not_near_line —
// exactly what silenced #2082's critical finding. With the fix the identifier is
// recovered and the finding is rescued as an unanchored body bullet (Line=0).
func TestGroundIssueRescuesDoubleBacktickFinding(t *testing.T) {
	// errors.go: ErrSignupCountryUnsupported is defined on changed line 98.
	diff := "diff --git a/errors.go b/errors.go\n" +
		"--- a/errors.go\n" +
		"+++ b/errors.go\n" +
		"@@ -97,1 +97,3 @@ var (\n" +
		"+\t// signup country guard\n" +
		"+\tErrSignupCountryUnsupported = &apperrors.AppError{Code: \"HULI-10055\", StatusCode: 400}\n" +
		"+)\n"
	scope := buildTestScope(diff)

	issue := event.Issue{
		File:        "errors.go",
		Line:        98, // the real changed line; the severity token blocks inline match, forcing rescue
		Description: "`major` `errors.go:98` `` `ErrSignupCountryUnsupported` `` — new HTTP 400 breaks clients.",
	}

	gi, ok, reason := scope.groundIssue(issue)
	if !ok {
		t.Fatalf("groundIssue silently dropped a grounded double-backtick finding; reason=%q", reason)
	}
	if reason != event.GroundingRescuedFileScope {
		t.Fatalf("reason = %q; want %q (rescued via file-scope token match)", reason, event.GroundingRescuedFileScope)
	}
	if gi.Line != 0 {
		t.Fatalf("rescued issue Line = %d; want 0 (unanchored body bullet, never an inline comment at a wrong line)", gi.Line)
	}
}
